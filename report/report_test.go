package report_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/intentd/kome/engine"
	"github.com/intentd/kome/report"
)

func results() []*engine.Result {
	return []*engine.Result{{
		Migration: "appconfig", Kind: "AppConfig", To: "NewConfig",
		Selection: engine.Selection{Namespaces: []string{"default"}},
		Warnings:  []string{"namespaces ignored"},
		Objects: []engine.ObjectResult{
			{Namespace: "default", Name: "api", Fields: []string{"spec"}, Pruned: true, AfterSpec: map[string]any{"a": 1}},
			{Namespace: "default", Name: "web", Drifted: true},
			{Namespace: "default", Name: "old", Skipped: true},
			{Namespace: "default", Name: "dev", Skipped: true, Excluded: true},
			{Namespace: "default", Name: "bad", Err: errors.New("boom")},
			{Name: "global", Fields: []string{"spec"}, Kept: true},
		},
	}}
}

func TestBuild_Totals(t *testing.T) {
	r := report.Build("up", false, false, results())
	assert.Equal(t, report.Totals{CRs: 6, Migrated: 2, Drifted: 1, Skipped: 1, Excluded: 1, Pruned: 1, Kept: 1, Failed: 1}, r.Totals)
	assert.True(t, r.Failed())
	assert.Equal(t, &report.Scope{Namespaces: []string{"default"}}, r.Kinds[0].Scope)
	assert.Nil(t, r.Kinds[0].CRs[0].AfterSpec, "afterSpec only when verbose")
	assert.NotNil(t, report.Build("up", false, true, results()).Kinds[0].CRs[0].AfterSpec)

	outcomes := map[string]string{}
	for _, o := range r.Kinds[0].CRs {
		outcomes[o.Ref] = report.Outcome(o)
	}
	assert.Equal(t, map[string]string{
		"default/api": "migrated", "default/web": "drifted", "default/old": "skipped",
		"default/dev": "excluded", "default/bad": "failed", "global": "kept",
	}, outcomes)
}

func TestJSONAndText(t *testing.T) {
	r := report.Build("up", true, false, results())
	var buf bytes.Buffer
	require.NoError(t, r.JSON(&buf))
	var doc map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &doc))
	assert.Equal(t, "up", doc["command"])
	assert.Equal(t, true, doc["dryRun"])

	buf.Reset()
	r.Text(&buf)
	out := buf.String()
	assert.Contains(t, out, "dry run: nothing written")
	assert.Contains(t, out, "appconfig (AppConfig → NewConfig)")
	assert.Contains(t, out, "! namespaces ignored")
	assert.Contains(t, out, "failed    default/bad: boom")
	assert.Contains(t, out, "migrated  default/api, source pruned")
	assert.Contains(t, out, "up: 6 objects: 2 migrated, 1 skipped, 1 excluded, 1 drifted, 1 pruned, 1 kept, 0 too-large, 1 failed")
}

func TestPruneBlockedGatheredWithLinesToAdd(t *testing.T) {
	blocked := func(b engine.PruneBlocked) error { return &b }
	declared := []string{"example.io/already"}
	results := []*engine.Result{{Migration: "appconfig", Kind: "AppConfig", To: "NewConfig", Objects: []engine.ObjectResult{
		{Namespace: "default", Name: "a", Err: blocked(engine.PruneBlocked{
			UndeclaredFinalizers: []string{"example.io/cleanup"}, DeclaredFinalizers: declared,
		})},
		{Namespace: "default", Name: "b", Err: blocked(engine.PruneBlocked{
			UndeclaredFinalizers: []string{"example.io/cleanup", "kubernetes.io/pvc-protection"}, DeclaredFinalizers: declared,
			Children: []string{"ConfigMap default/b-config"},
		})},
		{Namespace: "platform", Name: "c", Err: blocked(engine.PruneBlocked{
			CannotOwn: []string{"ClusterRoleBinding/c-binding"}, DeclaredFinalizers: declared,
		})},
		{Namespace: "default", Name: "d", Pruned: true, Fields: []string{"spec"}},
	}}}

	r := report.Build("up", true, false, results)
	p := r.Kinds[0].PruneBlocked
	require.NotNil(t, p)
	assert.Equal(t, 3, p.Sources)
	assert.Equal(t, map[string]int{"example.io/cleanup": 2, "kubernetes.io/pvc-protection": 1}, p.Finalizers)
	assert.Equal(t, []string{"default/b: ConfigMap default/b-config"}, p.Children)
	assert.Equal(t, []string{"platform/c: ClusterRoleBinding/c-binding"}, p.CannotOwn)
	assert.Equal(t, "lifecycle:\n  children: reparent   # or orphan, delete\n  finalizers: [example.io/already, example.io/cleanup]", p.Add,
		"declared ones kept, Kubernetes' own never suggested")

	var out strings.Builder
	r.Text(&out)
	text := out.String()
	assert.Contains(t, text, "⚠ 3 sources not pruned")
	assert.Contains(t, text, "example.io/cleanup                   2 sources")
	assert.Contains(t, text, "kubernetes.io/pvc-protection         1 source  (Kubernetes': never removed")
	assert.Contains(t, text, "own objects, and lifecycle.children is not set:\n      default/b: ConfigMap default/b-config")
	assert.Contains(t, text, "to prune them, add to the migration:\n      lifecycle:\n        children: reparent   # or orphan, delete\n        finalizers: [example.io/already, example.io/cleanup]")

	var js strings.Builder
	require.NoError(t, r.JSON(&js))
	assert.Contains(t, js.String(), `"pruneBlocked"`)
}

func TestNoPruneBlockedWhenNothingRefused(t *testing.T) {
	r := report.Build("up", false, false, []*engine.Result{{Migration: "m", Kind: "K", Objects: []engine.ObjectResult{{Name: "a", Pruned: true}}}})
	assert.Nil(t, r.Kinds[0].PruneBlocked)
	var js strings.Builder
	require.NoError(t, r.JSON(&js))
	assert.NotContains(t, js.String(), "pruneBlocked")
}

func TestTooLargeIsItsOwnOutcome(t *testing.T) {
	err := fmt.Errorf("%w, even compressed", engine.ErrTooLarge)
	r := report.Build("up", true, false, []*engine.Result{{Migration: "m", Kind: "K", Objects: []engine.ObjectResult{
		{Name: "big", Err: err, TooLarge: errors.Is(err, engine.ErrTooLarge)},
	}}})
	assert.Equal(t, "too-large", report.Outcome(r.Kinds[0].CRs[0]))
	assert.Equal(t, 1, r.Totals.TooLarge)
	assert.Equal(t, 0, r.Totals.Failed)
	assert.True(t, r.Failed(), "the migration did not finish")
}
