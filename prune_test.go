package kome_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/intentd/kome"
	"github.com/intentd/kome/pkg/declaration"
	"github.com/intentd/kome/pkg/kind"
	"github.com/intentd/kome/pkg/report"
	"github.com/intentd/kome/testutil"
	"github.com/intentd/tools/kubeclient/kubefake/object"
)

// pruning is a kind migration that prunes with children policy and finalizers.
func pruning(children string, finalizers ...string) kome.Migrations {
	return kome.Migrations{{Name: "move", Level: declaration.LevelKind, Kind: kind.Migration{
		Name: "move", From: testutil.OldGVK, To: testutil.NewGVK, PreserveSource: true,
		Prune: true, Children: children, Finalizers: finalizers,
	}}}
}

// source is an Old in namespace team.
func source(name string, finalizers ...string) *unstructured.Unstructured {
	src := testutil.OldObj(name, map[string]any{"image": "nginx"})
	if len(finalizers) > 0 {
		src.SetFinalizers(finalizers)
	}
	return src
}

// ownedBy is a ConfigMap in namespace ns with a controller reference to
// owner, as created (it needs its uid).
func ownedBy(name, ns string, owner *unstructured.Unstructured) *unstructured.Unstructured {
	cm := object.New("v1", "ConfigMap", name, object.InNamespace(ns))
	yes := true
	cm.SetOwnerReferences([]metav1.OwnerReference{{
		APIVersion: owner.GetAPIVersion(), Kind: owner.GetKind(), Name: owner.GetName(), UID: owner.GetUID(),
		Controller: &yes, BlockOwnerDeletion: &yes,
	}})
	return cm
}

func kindRecord(t *testing.T, dst *unstructured.Unstructured) map[string]any {
	t.Helper()
	var rec map[string]any
	require.NoError(t, json.Unmarshal([]byte(dst.GetAnnotations()["kome.intentd.io/migrate-kind-snapshot"]), &rec))
	return rec
}

func TestPrune_ChildrenWithoutPolicyRefuse(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t)
	k.Create(ownedBy("a-config", "team", k.Create(source("a"))))
	e := &kome.Engine{Kube: testutil.Kube()}

	// Dry-run says so before anything is written.
	res, err := e.Up(ctx, pruning(""), kome.Options{DryRun: true})
	require.NoError(t, err)
	require.Error(t, res[0].Objects[0].Err)
	assert.Contains(t, res[0].Objects[0].Err.Error(), "owns ConfigMap team/a-config; set lifecycle.children to reparent, orphan or delete")

	res, err = e.Up(ctx, pruning(""), kome.Options{})
	require.NoError(t, err)
	require.Error(t, res[0].Objects[0].Err)
	assert.NotNil(t, k.Get(testutil.OldGVR, "team", "a"), "the source stays")
	assert.Nil(t, k.Get(testutil.NewGVR, "team", "a"), "nothing was written: the prune is planned first")
	assert.NotNil(t, k.Get(testutil.ConfigMapGVR, "team", "a-config"), "its child stays")
}

func TestPrune_ReparentThenDownPointsChildrenBack(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t)
	src := k.Create(source("a"))
	k.Create(ownedBy("a-config", "team", src))
	other := source("other") // never created: only its uid is needed
	other.SetUID("uid-src-other")
	k.Create(ownedBy("unrelated", "team", other))
	e := &kome.Engine{Kube: testutil.Kube()}

	res, err := e.Up(ctx, pruning(declaration.ChildrenReparent), kome.Options{Strict: true})
	require.NoError(t, err)
	assert.True(t, res[0].Objects[0].Pruned)

	dst := k.Get(testutil.NewGVR, "team", "a")
	require.NotNil(t, dst)
	cm := k.Get(testutil.ConfigMapGVR, "team", "a-config")
	require.NotNil(t, cm, "the child survived the prune")
	refs := cm.GetOwnerReferences()
	require.Len(t, refs, 1)
	assert.Equal(t, dst.GetUID(), refs[0].UID, "it now belongs to the destination")
	assert.Equal(t, "New", refs[0].Kind)
	assert.True(t, *refs[0].Controller, "controller and blockOwnerDeletion are kept")
	assert.Nil(t, k.Get(testutil.OldGVR, "team", "a"), "deleted with the default propagation: nothing is left to cascade")

	rec := kindRecord(t, dst)
	assert.Equal(t, "reparent", rec["children"])
	assert.Equal(t, []any{map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "namespace": "team", "name": "a-config"}}, rec["reparented"])
	assert.Equal(t, k8stypes.UID("uid-src-other"), k.Get(testutil.ConfigMapGVR, "team", "unrelated").GetOwnerReferences()[0].UID, "another source's child is untouched")

	// down: the source comes back with a new uid, the child points at it, then
	// the destination goes.
	_, err = e.Down(ctx, pruning(declaration.ChildrenReparent), kome.Options{Strict: true})
	require.NoError(t, err)
	back := k.Get(testutil.OldGVR, "team", "a")
	require.NotNil(t, back)
	assert.NotEqual(t, src.GetUID(), back.GetUID(), "a recreated object has a new uid")
	refs = k.Get(testutil.ConfigMapGVR, "team", "a-config").GetOwnerReferences()
	require.Len(t, refs, 1)
	assert.Equal(t, back.GetUID(), refs[0].UID, "the child points at the restored source")
	assert.Equal(t, "Old", refs[0].Kind)
	assert.Nil(t, k.Get(testutil.NewGVR, "team", "a"))
}

func TestPrune_OrphanAndDeletePropagate(t *testing.T) {
	for _, policy := range []string{declaration.ChildrenOrphan, declaration.ChildrenDelete} {
		t.Run(policy, func(t *testing.T) {
			k := testutil.NewCluster(t)
			src := k.Create(source("a"))
			k.Create(ownedBy("a-config", "team", src))
			res, err := (&kome.Engine{Kube: testutil.Kube()}).Up(context.Background(), pruning(policy), kome.Options{Strict: true})
			require.NoError(t, err)
			assert.True(t, res[0].Objects[0].Pruned)
			// The API server's side of each propagation; the garbage collector
			// (not running here) does the rest.
			left := k.Get(testutil.OldGVR, "team", "a")
			if policy == declaration.ChildrenOrphan {
				require.NotNil(t, left, "kept until the garbage collector has orphaned its children")
				assert.Contains(t, left.GetFinalizers(), metav1.FinalizerOrphanDependents)
				assert.NotNil(t, left.GetDeletionTimestamp())
			} else {
				assert.Nil(t, left, "deleted at once; the garbage collector deletes its children after")
			}
			assert.Equal(t, src.GetUID(), k.Get(testutil.ConfigMapGVR, "team", "a-config").GetOwnerReferences()[0].UID, "not reparented")
		})
	}
}

func TestPrune_NoChildrenNeedsNoPolicy(t *testing.T) {
	k := testutil.NewCluster(t, source("a"))
	res, err := (&kome.Engine{Kube: testutil.Kube()}).Up(context.Background(), pruning(""), kome.Options{Strict: true})
	require.NoError(t, err)
	assert.True(t, res[0].Objects[0].Pruned)
	assert.Nil(t, k.Get(testutil.OldGVR, "team", "a"))
}

func TestPrune_Finalizers(t *testing.T) {
	ctx := context.Background()

	// An undeclared finalizer refuses the prune, declared or not the others.
	k := testutil.NewCluster(t, source("a", "example.io/cleanup", "other.io/keep"))
	res, err := (&kome.Engine{Kube: testutil.Kube()}).Up(ctx, pruning("", "example.io/cleanup"), kome.Options{})
	require.NoError(t, err)
	require.Error(t, res[0].Objects[0].Err)
	assert.Contains(t, res[0].Objects[0].Err.Error(), "finalizers other.io/keep are not in lifecycle.finalizers")
	assert.Equal(t, []string{"example.io/cleanup", "other.io/keep"}, k.Get(testutil.OldGVR, "team", "a").GetFinalizers(), "nothing removed")

	// Declared ones are removed before the delete, recorded, and put back by down.
	k = testutil.NewCluster(t, source("a", "example.io/cleanup"))
	e := &kome.Engine{Kube: testutil.Kube()}
	res, err = e.Up(ctx, pruning("", "example.io/cleanup"), kome.Options{Strict: true})
	require.NoError(t, err)
	assert.True(t, res[0].Objects[0].Pruned)
	assert.Nil(t, k.Get(testutil.OldGVR, "team", "a"))
	assert.Equal(t, []any{"example.io/cleanup"}, kindRecord(t, k.Get(testutil.NewGVR, "team", "a"))["finalizersRemoved"])

	_, err = e.Down(ctx, pruning("", "example.io/cleanup"), kome.Options{Strict: true})
	require.NoError(t, err)
	assert.Equal(t, []string{"example.io/cleanup"}, k.Get(testutil.OldGVR, "team", "a").GetFinalizers(), "restored as it was")
}

func TestPrune_FinishingAnEarlierRunPlansToo(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t)
	k.Create(ownedBy("a-config", "team", k.Create(source("a"))))
	e := &kome.Engine{Kube: testutil.Kube()}

	// A first run migrates without pruning; the second prunes, reparenting
	// the child onto the destination that already exists.
	noPrune := pruning(declaration.ChildrenReparent)
	noPrune[0].Kind.Prune = false
	_, err := e.Up(ctx, noPrune, kome.Options{Strict: true})
	require.NoError(t, err)
	require.NotNil(t, k.Get(testutil.OldGVR, "team", "a"))

	res, err := e.Up(ctx, pruning(declaration.ChildrenReparent), kome.Options{Strict: true})
	require.NoError(t, err)
	assert.True(t, res[0].Objects[0].Pruned)
	dst := k.Get(testutil.NewGVR, "team", "a")
	assert.Equal(t, dst.GetUID(), k.Get(testutil.ConfigMapGVR, "team", "a-config").GetOwnerReferences()[0].UID)
	rec := kindRecord(t, dst)
	assert.Equal(t, true, rec["pruned"])
	assert.Len(t, rec["reparented"], 1, "recorded, so down can point it back")
}

func TestPrune_DryRunReportSaysWhatToAdd(t *testing.T) {
	k := testutil.NewCluster(t, source("b"))
	k.Create(ownedBy("a-config", "team", k.Create(source("a", "example.io/cleanup"))))
	res, err := (&kome.Engine{Kube: testutil.Kube()}).Up(context.Background(), pruning(""), kome.Options{DryRun: true})
	require.NoError(t, err)

	var out strings.Builder
	report.Build("up", true, false, res).Text(&out)
	t.Log("\n" + out.String())
	text := out.String()
	assert.Contains(t, text, "⚠ 1 source not pruned")
	assert.Contains(t, text, "team/a: ConfigMap team/a-config")
	assert.Contains(t, text, "lifecycle:\n        children: reparent   # or orphan, delete\n        finalizers: [example.io/cleanup]")
}
