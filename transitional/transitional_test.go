package transitional_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/intentd/kome/transitional"
	"github.com/intentd/tools/kube"
)

const typed = "../declaration/testdata/typed/"

func TestGenerate(t *testing.T) {
	out, err := transitional.Generate(typed+"cronjob-old.yaml", typed+"cronjob.yaml", []transitional.Move{
		{From: "replicas", To: []string{"scaling.min"}},
		{From: "timeoutSeconds", To: []string{"timeout"}},
	})
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(out, &crd))
	spec := kube.StorageSpecSchema(&crd)
	require.NotNil(t, spec)

	for _, p := range []string{"replicas", "timeoutSeconds", "scaling.min", "timeout", "image"} {
		assert.True(t, kube.SchemaHoldsV1(spec, p), p)
	}
	assert.Equal(t, "integer", spec.Properties["replicas"].Type, "the from path keeps its type")
	assert.Equal(t, "string", spec.Properties["timeout"].Type, "the to path keeps its type")
	assert.False(t, kube.SchemaHoldsV1(spec, "legacy"), "only moved paths are added")
	assert.Empty(t, crd.Status.StoredVersions)
}

func TestGenerate_MakesToPathsOptional(t *testing.T) {
	dir := t.TempDir()
	to := filepath.Join(dir, "to.yaml")
	data, err := os.ReadFile(typed + "cronjob.yaml")
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(data, &crd))
	spec := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	spec.Required = []string{"scaling", "image"}
	scaling := spec.Properties["scaling"]
	scaling.Required = []string{"min"}
	spec.Properties["scaling"] = scaling
	crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"] = spec
	out, _ := yaml.Marshal(crd)
	require.NoError(t, os.WriteFile(to, out, 0o600))

	gen, err := transitional.Generate(typed+"cronjob-old.yaml", to, []transitional.Move{{From: "replicas", To: []string{"scaling.min"}}})
	require.NoError(t, err)
	var got apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(gen, &got))
	s := kube.StorageSpecSchema(&got)
	assert.Equal(t, []string{"image"}, s.Required, "scaling is optional until objects are migrated")
	assert.Empty(t, s.Properties["scaling"].Required)
}

func TestGenerate_RefusesAVersionChange(t *testing.T) {
	_, err := transitional.Generate(typed+"cronjob-old.yaml", typed+"cronjob-transitional-v2.yaml", nil)
	assert.ErrorContains(t, err, "is a conversion, not a field move")
}

// withColumns writes the CRD at path, with cols as its printer columns, into
// dir and returns the new path.
func withColumns(t *testing.T, dir, path string, cols ...apiextensionsv1.CustomResourceColumnDefinition) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(data, &crd))
	crd.Spec.Versions[0].AdditionalPrinterColumns = cols
	out, err := yaml.Marshal(crd)
	require.NoError(t, err)
	p := filepath.Join(dir, filepath.Base(path))
	require.NoError(t, os.WriteFile(p, out, 0o600))
	return p
}

func col(name, path string) apiextensionsv1.CustomResourceColumnDefinition {
	return apiextensionsv1.CustomResourceColumnDefinition{Name: name, Type: "string", JSONPath: path}
}

func TestGenerate_ShowsMovedPathsOldColumns(t *testing.T) {
	dir := t.TempDir()
	from := withColumns(t, dir, typed+"cronjob-old.yaml",
		col("Image", ".spec.image"),
		col("Replicas", ".spec.replicas"),
		col("Timeout", ".spec.timeoutSeconds"),
		col("Legacy", ".spec.legacy"),
	)
	toDir := filepath.Join(dir, "to")
	require.NoError(t, os.Mkdir(toDir, 0o755))
	to := withColumns(t, toDir, typed+"cronjob.yaml",
		col("Image", ".spec.image"),
		col("MinReplicas", ".spec.scaling.min"),
		col("Timeout", ".spec.timeout"),
		col("Age", ".metadata.creationTimestamp"),
	)

	out, err := transitional.Generate(from, to, []transitional.Move{
		{From: "replicas", To: []string{"scaling.min"}},
		{From: "timeoutSeconds", To: []string{"timeout"}},
	})
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(out, &crd))
	var got []string
	for _, c := range crd.Spec.Versions[0].AdditionalPrinterColumns {
		got = append(got, c.Name+" "+c.JSONPath)
	}
	assert.Equal(t, []string{
		"Image .spec.image",
		"Replicas .spec.replicas", // beside its destination
		"MinReplicas .spec.scaling.min",
		"OldTimeout .spec.timeoutSeconds", // the name stays with to's column
		"Timeout .spec.timeout",
		"Age .metadata.creationTimestamp",
	}, got, "only moved paths' columns are added")
}

func TestGenerateChain_DeclaresEveryStepsPaths(t *testing.T) {
	dir := t.TempDir()
	// v1 has a; v2 renamed it to b; v3 renames b to c.
	v1 := writeCRD(t, dir, "v1.yaml", map[string]any{"a": map[string]any{"type": "string", "description": "the original"}}, "a")
	v2 := writeCRD(t, dir, "v2.yaml", map[string]any{"b": map[string]any{"type": "string"}}, "b")
	v3 := writeCRD(t, dir, "v3.yaml", map[string]any{"c": map[string]any{"type": "string"}}, "c")

	out, err := transitional.GenerateChain(v3,
		transitional.Step{FromFile: v1, Moves: []transitional.Move{{From: "a", To: []string{"b"}}}},
		transitional.Step{FromFile: v2, Moves: []transitional.Move{{From: "b", To: []string{"c"}}}},
	)
	require.NoError(t, err)
	props := specProps(t, out)
	for _, p := range []string{"a", "b", "c"} {
		assert.Contains(t, props, p, "an object anywhere in the chain keeps %s", p)
	}
	assert.Equal(t, "the original", props["a"].(map[string]any)["description"], "a's type and description come from its own step's schema")
	assert.Empty(t, required(t, out), "every path is optional while objects move")

	// One step is what Generate does.
	one, err := transitional.Generate(v1, v2, []transitional.Move{{From: "a", To: []string{"b"}}})
	require.NoError(t, err)
	chained, err := transitional.GenerateChain(v2, transitional.Step{FromFile: v1, Moves: []transitional.Move{{From: "a", To: []string{"b"}}}})
	require.NoError(t, err)
	assert.Equal(t, string(one), string(chained))
}

// writeCRD writes a CRD for things.example.com/v1 whose spec has props and
// requires req.
func writeCRD(t *testing.T, dir, name string, props map[string]any, req string) string {
	t.Helper()
	crd := map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
		"metadata": map[string]any{"name": "things.example.com"},
		"spec": map[string]any{
			"group": "example.com", "scope": "Namespaced",
			"names": map[string]any{"kind": "Thing", "plural": "things"},
			"versions": []any{map[string]any{"name": "v1", "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{"type": "object", "properties": map[string]any{
					"spec": map[string]any{"type": "object", "required": []any{req}, "properties": props},
				}}},
			}},
		},
	}
	b, err := yaml.Marshal(crd)
	require.NoError(t, err)
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, b, 0o644))
	return p
}

func specProps(t *testing.T, crdYAML []byte) map[string]any {
	t.Helper()
	spec := specSchema(t, crdYAML)
	props, _ := spec["properties"].(map[string]any)
	return props
}

func required(t *testing.T, crdYAML []byte) []any {
	t.Helper()
	req, _ := specSchema(t, crdYAML)["required"].([]any)
	return req
}

func specSchema(t *testing.T, crdYAML []byte) map[string]any {
	t.Helper()
	var crd map[string]any
	require.NoError(t, yaml.Unmarshal(crdYAML, &crd))
	v := crd["spec"].(map[string]any)["versions"].([]any)[0].(map[string]any)
	return v["schema"].(map[string]any)["openAPIV3Schema"].(map[string]any)["properties"].(map[string]any)["spec"].(map[string]any)
}
