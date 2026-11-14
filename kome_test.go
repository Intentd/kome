package kome_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/intentd/kome"
	"github.com/intentd/kome/declaration"
	"github.com/intentd/kome/engine"
	"github.com/intentd/kome/field"
	"github.com/intentd/kome/internal/testutil"
	"github.com/intentd/kome/kind"
	"github.com/intentd/tools/resolver"
)

const fieldRecord = "kome.intentd.io/migrate-field-snapshot"

func fieldMigration(moves ...field.Move) kome.Migrations {
	return kome.Migrations{{Name: "old", Level: declaration.LevelField, Field: field.Migration{Name: "old", GVK: testutil.OldGVK, Moves: moves}}}
}

// randomText is n characters that do not compress.
func randomText(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)
	return base64.StdEncoding.EncodeToString(b)[:n]
}

func spec(t *testing.T, u *unstructured.Unstructured) map[string]any {
	t.Helper()
	require.NotNil(t, u)
	s, _, _ := unstructured.NestedMap(u.Object, "spec")
	return s
}

func TestField_RelocateTransformFanout_UpDownClean(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t, testutil.OldObj("a", map[string]any{"replicas": int64(2), "timeoutSeconds": int64(30), "schedule": "5 1 * * *", "image": "nginx"}))
	e := &kome.Engine{Kube: testutil.Kube()}
	ms := fieldMigration(
		field.Move{Key: "replicas", From: "replicas", Field: resolver.Field{Path: "scaling.min"}},
		field.Move{Key: "timeout", From: "timeoutSeconds", Field: resolver.Field{Path: "timeout", Value: `{{ printf "%vs" .value }}`}},
		field.Move{Key: "schedule", From: "schedule", Field: resolver.Field{Values: map[string]string{
			"schedule.minute": "{{ cronMinute .value }}",
			"schedule.hour":   "{{ cronHour .value }}",
		}}},
	)

	res, err := e.Up(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	require.Len(t, res, 1)
	assert.ElementsMatch(t, []string{"replicas", "schedule", "timeout"}, res[0].Objects[0].Fields)

	got := spec(t, k.Get(testutil.OldGVR, "team", "a"))
	assert.Equal(t, map[string]any{"min": int64(2)}, got["scaling"])
	assert.Equal(t, "30s", got["timeout"])
	assert.Equal(t, map[string]any{"minute": "5", "hour": "1"}, got["schedule"])
	assert.NotContains(t, got, "replicas")
	assert.NotContains(t, got, "timeoutSeconds")
	assert.Equal(t, "nginx", got["image"], "fields no move touches stay")

	// A rerun does nothing: every move is already done.
	res, err = e.Up(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	assert.True(t, res[0].Objects[0].Skipped)

	// down restores every field and removes the record.
	res, err = e.Down(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"replicas", "schedule", "timeout"}, res[0].Objects[0].Fields)
	back := k.Get(testutil.OldGVR, "team", "a")
	got = spec(t, back)
	assert.Equal(t, int64(2), got["replicas"])
	assert.Equal(t, int64(30), got["timeoutSeconds"])
	assert.Equal(t, "5 1 * * *", got["schedule"])
	assert.NotContains(t, got, "scaling")
	assert.NotContains(t, back.GetAnnotations(), fieldRecord)

	// clean after up removes only the record.
	_, err = e.Up(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	_, err = e.Clean(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	cleaned := k.Get(testutil.OldGVR, "team", "a")
	assert.NotContains(t, cleaned.GetAnnotations(), fieldRecord)
	assert.Equal(t, map[string]any{"min": int64(2)}, spec(t, cleaned)["scaling"])
}

func TestField_InPlaceTransformRunsOnce(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t, testutil.OldObj("a", map[string]any{"tier": "gold"}))
	e := &kome.Engine{Kube: testutil.Kube()}
	ms := fieldMigration(field.Move{Key: "tier", From: "tier", Field: resolver.Field{Value: "{{ .value }}-v2"}})

	for range 2 {
		_, err := e.Up(ctx, ms, kome.Options{Strict: true})
		require.NoError(t, err)
	}
	assert.Equal(t, "gold-v2", spec(t, k.Get(testutil.OldGVR, "team", "a"))["tier"], "a rerun does not transform again")
}

func TestField_DriftIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t, testutil.OldObj("a", map[string]any{"replicas": int64(2)}))
	e := &kome.Engine{Kube: testutil.Kube()}
	ms := fieldMigration(field.Move{Key: "replicas", From: "replicas", Field: resolver.Field{Path: "scaling.min"}})
	_, err := e.Up(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)

	// Someone edits the migrated value.
	obj := k.Get(testutil.OldGVR, "team", "a")
	require.NoError(t, unstructured.SetNestedField(obj.Object, int64(5), "spec", "scaling", "min"))
	k.Update(obj)

	res, err := e.Down(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	r := res[0].Objects[0]
	assert.True(t, r.AllDrifted)
	assert.Equal(t, []string{"replicas"}, r.DriftedFields)
	assert.Equal(t, int64(5), spec(t, k.Get(testutil.OldGVR, "team", "a"))["scaling"].(map[string]any)["min"])
	assert.Contains(t, k.Get(testutil.OldGVR, "team", "a").GetAnnotations(), fieldRecord, "the record is kept")
}

func TestField_SelectionExcludes(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t, testutil.OldObj("a", map[string]any{"replicas": int64(2), "env": "dev"}), testutil.OldObj("b", map[string]any{"replicas": int64(3), "env": "prod"}))
	e := &kome.Engine{Kube: testutil.Kube()}
	ms := fieldMigration(field.Move{Key: "replicas", From: "replicas", Field: resolver.Field{Path: "scaling.min"}})
	ms[0].Field.Selection = engine.Selection{When: whenEquals("spec.env", "prod")}

	res, err := e.Up(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	byName := map[string]engine.ObjectResult{}
	for _, o := range res[0].Objects {
		byName[o.Name] = o
	}
	assert.True(t, byName["a"].Excluded)
	assert.Equal(t, []string{"replicas"}, byName["b"].Fields)
	assert.Contains(t, spec(t, k.Get(testutil.OldGVR, "team", "a")), "replicas")
}

func TestField_DryRunWritesNothing(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t, testutil.OldObj("a", map[string]any{"replicas": int64(2)}))
	e := &kome.Engine{Kube: testutil.Kube()}
	ms := fieldMigration(field.Move{Key: "replicas", From: "replicas", Field: resolver.Field{Path: "scaling.min"}})
	_, err := e.Up(ctx, ms, kome.Options{DryRun: true, Strict: true})
	require.NoError(t, err)
	assert.Contains(t, spec(t, k.Get(testutil.OldGVR, "team", "a")), "replicas")
}

// An object whose annotations are near the limit has no room for its
// record: the dry run reports it, and nothing is written.
func TestField_RecordTooLarge(t *testing.T) {
	ctx := context.Background()
	big := testutil.OldObj("big", map[string]any{"replicas": int64(2)})
	big.SetAnnotations(map[string]string{"kubectl.kubernetes.io/last-applied-configuration": strings.Repeat("x", 256*1024-100)})
	k := testutil.NewCluster(t, big, testutil.OldObj("small", map[string]any{"replicas": int64(3)}))
	e := &kome.Engine{Kube: testutil.Kube()}
	ms := fieldMigration(field.Move{Key: "replicas", From: "replicas", Field: resolver.Field{Path: "scaling.min"}})
	for _, dryRun := range []bool{true, false} {
		res, err := e.Up(ctx, ms, kome.Options{DryRun: dryRun})
		require.NoError(t, err)
		byName := map[string]engine.ObjectResult{}
		for _, o := range res[0].Objects {
			byName[o.Name] = o
		}
		require.Error(t, byName["big"].Err, "dryRun=%v", dryRun)
		assert.Contains(t, byName["big"].Err.Error(), "the record is too large")
		assert.True(t, byName["big"].TooLarge, "its own outcome, not a failure")
		assert.NoError(t, byName["small"].Err)
		assert.Contains(t, spec(t, k.Get(testutil.OldGVR, "team", "big")), "replicas", "big is left as it was")
	}
	assert.Contains(t, spec(t, k.Get(testutil.OldGVR, "team", "small")), "scaling")
}

// Beside a large annotation, a field record that only fits compressed is
// written compressed, and down restores from it.
func TestField_LargeRecordIsCompressed(t *testing.T) {
	ctx := context.Background()
	cmd := strings.Repeat("echo hello; ", 10*1024) // ~120 KiB, compresses well
	obj := testutil.OldObj("a", map[string]any{"command": cmd})
	obj.SetAnnotations(map[string]string{"kubectl.kubernetes.io/last-applied-configuration": randomText(t, 100*1024)})
	k := testutil.NewCluster(t, obj)
	e := &kome.Engine{Kube: testutil.Kube()}
	ms := fieldMigration(field.Move{Key: "command", From: "command", Field: resolver.Field{Path: "run.command"}})

	_, err := e.Up(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	migrated := k.Get(testutil.OldGVR, "team", "a")
	assert.False(t, strings.HasPrefix(migrated.GetAnnotations()[fieldRecord], "{"), "compressed")

	_, err = e.Down(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	assert.Equal(t, cmd, spec(t, k.Get(testutil.OldGVR, "team", "a"))["command"])
}

func TestField_MissingCRD(t *testing.T) {
	e := &kome.Engine{Kube: testutil.Kube()}
	ms := fieldMigration(field.Move{Key: "x", From: "x"})
	ms[0].Field.GVK.Kind = "Absent"
	_, err := e.Up(context.Background(), ms, kome.Options{})
	assert.ErrorContains(t, err, "is not installed in the cluster")
}

// ── kind ──────────────────────────────────────────────────────────────────

func kindMigration(prune bool) kome.Migrations {
	return kome.Migrations{{Name: "move", Level: declaration.LevelKind, Kind: kind.Migration{
		Name: "move", From: testutil.OldGVK, To: testutil.NewGVK, Prune: prune, PreserveSource: true,
		Labels: map[string]string{"migrated-from": "{{ .metadata.name }}"},
	}}}
}

func TestKind_UpWithPruneThenDown(t *testing.T) {
	ctx := context.Background()
	src := testutil.OldObj("a", map[string]any{"image": "nginx"})
	src.SetAnnotations(map[string]string{"note": "keep", "kubectl.kubernetes.io/last-applied-configuration": "{}"})
	k := testutil.NewCluster(t, src)
	e := &kome.Engine{Kube: testutil.Kube()}

	res, err := e.Up(ctx, kindMigration(true), kome.Options{Strict: true})
	require.NoError(t, err)
	assert.True(t, res[0].Objects[0].Pruned)
	assert.Equal(t, "New", res[0].To)

	dst := k.Get(testutil.NewGVR, "team", "a")
	require.NotNil(t, dst)
	assert.Equal(t, "nginx", spec(t, dst)["image"])
	assert.Equal(t, "a", dst.GetLabels()["migrated-from"])
	assert.Equal(t, "demo", dst.GetLabels()["app"], "source labels are kept")
	assert.Equal(t, "keep", dst.GetAnnotations()["note"])
	assert.NotContains(t, dst.GetAnnotations(), "kubectl.kubernetes.io/last-applied-configuration", "tooling annotations are not copied")
	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(dst.GetAnnotations()["kome.intentd.io/migrate-kind-snapshot"]), &rec))
	assert.Equal(t, true, rec["pruned"])
	assert.Nil(t, k.Get(testutil.OldGVR, "team", "a"), "pruned")

	res, err = e.Down(ctx, kindMigration(true), kome.Options{Strict: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"spec"}, res[0].Objects[0].Fields)
	back := k.Get(testutil.OldGVR, "team", "a")
	require.NotNil(t, back, "the source is recreated from the record")
	assert.Equal(t, "nginx", spec(t, back)["image"])
	assert.Equal(t, "keep", back.GetAnnotations()["note"])
	assert.Nil(t, k.Get(testutil.NewGVR, "team", "a"), "the destination the migration created is deleted")
}

func TestKind_ExistingDestinationIsKeptOnDown(t *testing.T) {
	ctx := context.Background()
	src := testutil.OldObj("a", map[string]any{"image": "nginx"})
	existing := testutil.OldObj("a", map[string]any{"image": "nginx"})
	existing.SetAPIVersion(testutil.NewGVK.GroupVersion().String())
	existing.SetKind(testutil.NewGVK.Kind)
	k := testutil.NewCluster(t, src, existing)
	e := &kome.Engine{Kube: testutil.Kube()}

	res, err := e.Up(ctx, kindMigration(true), kome.Options{Strict: true})
	require.NoError(t, err)
	assert.True(t, res[0].Objects[0].Pruned, "a destination someone else wrote first still lets the prune finish")

	res, err = e.Down(ctx, kindMigration(true), kome.Options{Strict: true})
	require.NoError(t, err)
	assert.True(t, res[0].Objects[0].Kept)
	assert.NotNil(t, k.Get(testutil.OldGVR, "team", "a"))
	kept := k.Get(testutil.NewGVR, "team", "a")
	require.NotNil(t, kept, "a destination the migration did not create is kept")
	assert.NotContains(t, kept.GetAnnotations(), "kome.intentd.io/migrate-kind-snapshot")
}

// The kind record holds the source's spec. A large one is compressed, and
// down still restores the source from it.
func TestKind_LargeRecordIsCompressed(t *testing.T) {
	ctx := context.Background()
	image := strings.Repeat("nginx:1.27 ", 30*1024) // ~330 KiB, compresses well
	k := testutil.NewCluster(t, testutil.OldObj("big", map[string]any{"image": image}))
	e := &kome.Engine{Kube: testutil.Kube()}
	_, err := e.Up(ctx, kindMigration(true), kome.Options{Strict: true})
	require.NoError(t, err)
	dst := k.Get(testutil.NewGVR, "team", "big")
	require.NotNil(t, dst)
	assert.False(t, strings.HasPrefix(dst.GetAnnotations()["kome.intentd.io/migrate-kind-snapshot"], "{"), "compressed")
	require.Nil(t, k.Get(testutil.OldGVR, "team", "big"), "pruned")

	_, err = e.Down(ctx, kindMigration(true), kome.Options{Strict: true})
	require.NoError(t, err)
	back := k.Get(testutil.OldGVR, "team", "big")
	require.NotNil(t, back, "the source is recreated from the compressed record")
	assert.Equal(t, image, spec(t, back)["image"])
}

// A source too large to record even compressed is reported, and neither
// created nor pruned.
func TestKind_RecordTooLarge(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t, testutil.OldObj("big", map[string]any{"image": randomText(t, 300*1024)}))
	for _, dryRun := range []bool{true, false} {
		res, err := (&kome.Engine{Kube: testutil.Kube()}).Up(ctx, kindMigration(true), kome.Options{DryRun: dryRun})
		require.NoError(t, err)
		require.Error(t, res[0].Objects[0].Err, "dryRun=%v", dryRun)
		assert.Contains(t, res[0].Objects[0].Err.Error(), "the record is too large")
		assert.True(t, res[0].Objects[0].TooLarge)
	}
	assert.NotNil(t, k.Get(testutil.OldGVR, "team", "big"), "the source is not pruned")
	assert.Nil(t, k.Get(testutil.NewGVR, "team", "big"), "no destination")
}

func TestKind_DriftedDestination(t *testing.T) {
	ctx := context.Background()
	existing := testutil.OldObj("a", map[string]any{"image": "other"})
	existing.SetAPIVersion(testutil.NewGVK.GroupVersion().String())
	existing.SetKind(testutil.NewGVK.Kind)
	k := testutil.NewCluster(t, testutil.OldObj("a", map[string]any{"image": "nginx"}), existing)
	res, err := (&kome.Engine{Kube: testutil.Kube()}).Up(ctx, kindMigration(true), kome.Options{Strict: true})
	require.NoError(t, err)
	assert.True(t, res[0].Objects[0].Drifted)
	assert.NotNil(t, k.Get(testutil.OldGVR, "team", "a"), "a drifted pair is not pruned")
}

func TestKind_CustomPrefixAndFieldManager(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t, testutil.OldObj("a", map[string]any{"image": "nginx"}))
	e := &kome.Engine{Kube: testutil.Kube(), Prefix: "inrun.dev", FieldManager: "inrun-gateway"}
	_, err := e.Up(ctx, kindMigration(false), kome.Options{Strict: true})
	require.NoError(t, err)
	dst := k.Get(testutil.NewGVR, "team", "a")
	assert.Contains(t, dst.GetAnnotations(), "inrun.dev/migrate-kind-snapshot")
	assert.NotContains(t, dst.GetAnnotations(), "kome.intentd.io/migrate-kind-snapshot")
}

func TestField_LiveSchemaMustDeclarePaths(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t)
	k.Schema(map[string]any{"type": "object", "properties": map[string]any{
		"replicas": map[string]any{"type": "integer"},
	}})
	k.Create(testutil.OldObj("a", map[string]any{"replicas": int64(2)}))
	ms := fieldMigration(field.Move{Key: "replicas", From: "replicas", Field: resolver.Field{Path: "scaling.min"}})
	_, err := (&kome.Engine{Kube: testutil.Kube()}).Up(ctx, ms, kome.Options{Strict: true})
	assert.ErrorContains(t, err, "does not declare spec.scaling.min: apply the transitional schema first")
	assert.Contains(t, spec(t, k.Get(testutil.OldGVR, "team", "a")), "replicas", "nothing is written")

	// down does not need the new paths declared.
	_, err = (&kome.Engine{Kube: testutil.Kube()}).Down(ctx, ms, kome.Options{Strict: true})
	assert.NoError(t, err)
}

func TestField_CELKeepsTheType(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t, testutil.OldObj("a", map[string]any{"replicas": int64(3), "tier": "web"}))
	ms := fieldMigration(field.Move{Key: "replicas", From: "replicas",
		Field: resolver.Field{Path: "scaling.min", Value: `${ tier == "web" ? value * 2 : value }`}})
	e := &kome.Engine{Kube: testutil.Kube()}

	_, err := e.Up(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	got := spec(t, k.Get(testutil.OldGVR, "team", "a"))
	assert.Equal(t, int64(6), got["scaling"].(map[string]any)["min"], "an int, not the text a template would write")
	assert.NotContains(t, got, "replicas")

	_, err = e.Down(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	assert.Equal(t, int64(3), spec(t, k.Get(testutil.OldGVR, "team", "a"))["replicas"])
}
