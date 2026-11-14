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
