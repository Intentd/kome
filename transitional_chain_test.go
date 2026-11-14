package kome_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/intentd/kome"
	"github.com/intentd/kome/declaration"
)

func thingCRD(t *testing.T, dir, file, kind, field string) {
	t.Helper()
	plural := map[string]string{"Thing": "things", "Other": "others"}[kind]
	crd := map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": plural + ".example.com"},
		"spec": map[string]any{"group": "example.com", "scope": "Namespaced",
			"names": map[string]any{"kind": kind, "plural": plural},
			"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "object", "properties": map[string]any{
					"spec": map[string]any{"type": "object", "properties": map[string]any{field: map[string]any{"type": "string"}}},
				}}}}}},
	}
	b, err := yaml.Marshal(crd)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, file), b, 0o644))
}

func TestTransitional_CoversTheNeedsChain(t *testing.T) {
	dir := t.TempDir()
	thingCRD(t, dir, "v1.yaml", "Thing", "a")
	thingCRD(t, dir, "v2.yaml", "Thing", "b")
	thingCRD(t, dir, "v3.yaml", "Thing", "c")
	thingCRD(t, dir, "other-v1.yaml", "Other", "x")
	thingCRD(t, dir, "other-v2.yaml", "Other", "y")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "kome.yaml"), []byte(`
migrations:
  v2:
    from: {schema: ./v1.yaml}
    to: {schema: ./v2.yaml}
    spec: {a: {to: b}}
  v3:
    needs: [v2, other]
    from: {schema: ./v2.yaml}
    to: {schema: ./v3.yaml}
    spec: {b: {to: c}}
  other:
    from: {schema: ./other-v1.yaml}
    to: {schema: ./other-v2.yaml}
    spec: {x: {to: y}}
`), 0o644))
	f, err := declaration.Load(filepath.Join(dir, "kome.yaml"))
	require.NoError(t, err)

	out, err := kome.Transitional(f, "v3")
	require.NoError(t, err)
	s := string(out)
	prop := func(name string) string { return "\n              " + name + ":\n" }
	for _, p := range []string{"a", "b", "c"} {
		assert.Contains(t, s, prop(p), "the chain v2 → v3 is covered")
	}
	assert.NotContains(t, s, prop("x"), "another kind's migration is not part of the chain")

	alone, err := kome.Transitional(f, "v2")
	require.NoError(t, err)
	assert.NotContains(t, string(alone), prop("c"), "v2 needs nothing: its own paths only")
}
