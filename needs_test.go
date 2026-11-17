package kome_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/intentd/kome"
	"github.com/intentd/kome/declaration"
	"github.com/intentd/kome/field"
	"github.com/intentd/kome/internal/testutil"
	"github.com/intentd/tools/resolver"
)

// chain is v2 (a → b) and v3 (b → c, needing v2): the same value moved twice.
func chain() kome.Migrations {
	v2 := kome.Migration{Name: "v2", Level: declaration.LevelField, NeededBy: []string{"v3"}, Field: field.Migration{
		Name: "v2", GVK: testutil.OldGVK, Moves: field.Moves{{Key: "a", From: "a", Field: resolver.Field{Path: "b"}}},
	}}
	v3 := kome.Migration{Name: "v3", Level: declaration.LevelField, Needs: []string{"v2"}, Field: field.Migration{
		Name: "v3", GVK: testutil.OldGVK, Moves: field.Moves{{Key: "b", From: "b", Field: resolver.Field{Path: "c"}}},
	}}
	return kome.Migrations{v2, v3}
}

func TestNeeds_ChainUpThenDownOneStepAtATime(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t, testutil.OldObj("x", map[string]any{"a": "value"}))
	e := &kome.Engine{Kube: testutil.Kube()}
	all := chain()
	o := kome.Options{Strict: true, All: all}

	_, err := e.Up(ctx, all, o)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"c": "value"}, spec(t, k.Get(testutil.OldGVR, "team", "x")), "both ran, in order")

	// Undo only v3: c goes back to b, and v2's entry stays for later.
	_, err = e.Down(ctx, all[1:], o)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"b": "value"}, spec(t, k.Get(testutil.OldGVR, "team", "x")))
	assert.Contains(t, k.Get(testutil.OldGVR, "team", "x").GetAnnotations()[fieldRecord], `"v2"`)
	assert.NotContains(t, k.Get(testutil.OldGVR, "team", "x").GetAnnotations()[fieldRecord], `"v3"`)

	_, err = e.Down(ctx, all[:1], o)
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"a": "value"}, spec(t, k.Get(testutil.OldGVR, "team", "x")))
	assert.NotContains(t, k.Get(testutil.OldGVR, "team", "x").GetAnnotations(), fieldRecord, "the last entry takes the annotation with it")
}

func TestNeeds_UpRefusesWhileANeededMigrationIsPending(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t, testutil.OldObj("x", map[string]any{"a": "value"}))
	e := &kome.Engine{Kube: testutil.Kube()}
	all := chain()

	_, err := e.Up(ctx, all[1:], kome.Options{All: all})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "v3 needs v2, which is still pending on 1 object: run it first, or both together")
	assert.Equal(t, map[string]any{"a": "value"}, spec(t, k.Get(testutil.OldGVR, "team", "x")), "nothing ran")

	// Once v2 is done, v3 runs on its own.
	_, err = e.Up(ctx, all[:1], kome.Options{All: all, Strict: true})
	require.NoError(t, err)
	_, err = e.Up(ctx, all[1:], kome.Options{All: all, Strict: true})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"c": "value"}, spec(t, k.Get(testutil.OldGVR, "team", "x")))

	// Without All, a need outside the run can't be checked.
	_, err = e.Up(ctx, all[1:], kome.Options{})
	assert.ErrorContains(t, err, "not among the migrations given")
}

func TestNeeds_DownRefusesWhileADependentIsApplied(t *testing.T) {
	ctx := context.Background()
	k := testutil.NewCluster(t, testutil.OldObj("x", map[string]any{"a": "value"}))
	e := &kome.Engine{Kube: testutil.Kube()}
	all := chain()
	_, err := e.Up(ctx, all, kome.Options{All: all, Strict: true})
	require.NoError(t, err)

	_, err = e.Down(ctx, all[:1], kome.Options{All: all})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "v2 is needed by v3, which is still applied on 1 object: undo it first, or both together")
	assert.Equal(t, map[string]any{"c": "value"}, spec(t, k.Get(testutil.OldGVR, "team", "x")), "nothing undone")

	// Both together: down goes latest first.
	_, err = e.Down(ctx, all, kome.Options{All: all, Strict: true})
	require.NoError(t, err)
	assert.Equal(t, map[string]any{"a": "value"}, spec(t, k.Get(testutil.OldGVR, "team", "x")))
}

func TestStatus(t *testing.T) {
	ctx := context.Background()
	testutil.NewCluster(t, testutil.OldObj("x", map[string]any{"a": "1"}), testutil.OldObj("y", map[string]any{"a": "2"}))
	e := &kome.Engine{Kube: testutil.Kube()}
	all := chain()

	st, err := e.Status(ctx, all, kome.Options{All: all})
	require.NoError(t, err)
	assert.Equal(t, []kome.Status{{Migration: "v2", Pending: 2}, {Migration: "v3"}}, st)

	_, err = e.Up(ctx, all[:1], kome.Options{All: all, Strict: true})
	require.NoError(t, err)
	st, err = e.Status(ctx, all, kome.Options{All: all})
	require.NoError(t, err)
	assert.Equal(t, kome.Status{Migration: "v2", Migrated: 2}, st[0])
	assert.Equal(t, kome.Status{Migration: "v3", Pending: 2}, st[1])
}
