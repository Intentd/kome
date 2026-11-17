package kome_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/intentd/kome"
	"github.com/intentd/kome/field"
	"github.com/intentd/kome/internal/testutil"
)

func TestFieldManager_DeclaredInTheFileCreatesInsteadOfApplying(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t, source("a"))
	ms := pruning("")
	ms[0].FieldManager = "appconfig-operator"

	_, err := (&kome.Engine{Kube: testutil.Kube()}).Up(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	dst := k.Get(testutil.NewGVR, "team", "a")
	require.NotNil(t, dst)
	assert.Equal(t, []string{"Update appconfig-operator"}, testutil.Managers(dst),
		"created, not applied: an operator's next apply would remove what KOME applied as it")
	assert.Equal(t, "appconfig-operator", kindRecord(t, dst)["fieldManager"])
}

func TestFieldManager_KOMEsOwnApplies(t *testing.T) {
	ctx := context.Background()
	for name, e := range map[string]*kome.Engine{
		"default":        {},
		"caller's own":   {FieldManager: "inrun-gateway"},
		"caller wins":    {FieldManager: "inrun-gateway"}, // over the file's below
		"file is absent": {},
	} {
		t.Run(name, func(t *testing.T) {
			k := testutil.NewCluster(t, source("a"))
			e.Kube = testutil.Kube()
			ms := pruning("")
			if name == "caller wins" {
				ms[0].FieldManager = "appconfig-operator"
			}
			_, err := e.Up(ctx, ms, kome.Options{Strict: true})
			require.NoError(t, err)
			want := "kome"
			if e.FieldManager != "" {
				want = e.FieldManager
			}
			assert.Equal(t, []string{"Apply " + want}, testutil.Managers(k.Get(testutil.NewGVR, "team", "a")))
		})
	}
}

func TestFieldManager_DownWritesAsTheRecordedManager(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t, source("a"))
	ms := pruning("")
	ms[0].FieldManager = "appconfig-operator"
	_, err := (&kome.Engine{Kube: testutil.Kube()}).Up(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)

	// The file changes after the move; down still undoes it as the manager that made it.
	ms[0].FieldManager = "someone-else"
	_, err = (&kome.Engine{Kube: testutil.Kube()}).Down(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"Update appconfig-operator"}, testutil.Managers(k.Get(testutil.OldGVR, "team", "a")), "the source is recreated as the recorded manager")
}

func TestFieldManager_FieldRecordAndRestore(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t, testutil.OldObj("a", map[string]any{"replicas": int64(3)}))
	ms := fieldMigration(field.Move{Key: "replicas", From: "replicas"})
	ms[0].Field.Moves[0].Field.Path = "scaling.min"
	ms[0].FieldManager = "appconfig-operator"

	_, err := (&kome.Engine{Kube: testutil.Kube()}).Up(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	obj := k.Get(testutil.OldGVR, "team", "a")
	assert.Contains(t, obj.GetAnnotations()[fieldRecord], `"fieldManager":"appconfig-operator"`)

	ms[0].FieldManager = "someone-else"
	_, err = (&kome.Engine{Kube: testutil.Kube()}).Down(ctx, ms, kome.Options{Strict: true})
	require.NoError(t, err)
	got := testutil.Managers(k.Get(testutil.OldGVR, "team", "a"))
	assert.Contains(t, got, "Update appconfig-operator", "restored as the manager that moved it")
	assert.NotContains(t, got, "Update someone-else")
}

func TestOtherWritersAreReported(t *testing.T) {
	ctx := context.Background()

	// Field level: whoever wrote the old path will write it again.
	k := testutil.NewCluster(t)
	k.ApplyAs("argocd-controller", testutil.OldObj("a", map[string]any{"replicas": int64(3)}))
	k.ApplyAs("helm", testutil.OldObj("a", map[string]any{"image": "nginx"})) // another field
	k.ScaleAs("kube-controller-manager", "team", "a", 3)                      // a subresource: not a claim
	ms := fieldMigration(field.Move{Key: "replicas", From: "replicas"})
	ms[0].Field.Moves[0].Field.Path = "scaling.min"
	res, err := (&kome.Engine{Kube: testutil.Kube()}).Up(ctx, ms, kome.Options{DryRun: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"spec.replicas is written by argocd-controller on 1 object: it still has the old shape and will set it again; update it too"},
		res[0].Warnings)

	// Kind level: whoever wrote the sources will write them again.
	k = testutil.NewCluster(t)
	k.ApplyAs("argocd-controller", source("a"))
	res, err = (&kome.Engine{Kube: testutil.Kube()}).Up(ctx, pruning(""), kome.Options{DryRun: true})
	require.NoError(t, err)
	require.Len(t, res[0].Warnings, 1)
	assert.True(t, strings.HasPrefix(res[0].Warnings[0],
		"Old objects are written by argocd-controller (1 source): it still writes the old kind and will recreate what the prune deletes; move it to New too"),
		res[0].Warnings[0])
}
