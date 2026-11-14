package report_test

import (
	"bytes"
	"encoding/json"
	"errors"
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
	assert.Contains(t, out, "up: 6 objects: 2 migrated, 1 skipped, 1 excluded, 1 drifted, 1 pruned, 1 kept, 1 failed")
}
