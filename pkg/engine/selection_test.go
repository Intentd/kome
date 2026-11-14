package engine_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/intentd/kome/pkg/engine"
)

func TestNarrow(t *testing.T) {
	declared := engine.Selection{Namespaces: []string{"a", "b"}, Selector: "tier=web"}

	got, err := declared.Narrow(engine.Selection{Namespaces: []string{"b", "c"}, Selector: "env=prod", Name: "x"})
	require.NoError(t, err)
	assert.Equal(t, []string{"b"}, got.Namespaces, "the scope narrows, never widens")
	assert.Equal(t, "env=prod,tier=web", got.Selector, "both selectors apply")
	assert.Equal(t, "x", got.Name)

	_, err = declared.Narrow(engine.Selection{Namespaces: []string{"c"}})
	assert.ErrorContains(t, err, "outside the migration's selection")

	_, err = engine.Selection{Name: "x"}.Narrow(engine.Selection{Name: "y"})
	assert.ErrorContains(t, err, `selects name "x"`)

	got, _ = engine.Selection{}.Narrow(engine.Selection{Namespaces: []string{"c"}})
	assert.Equal(t, []string{"c"}, got.Namespaces)
}

func TestIncludes(t *testing.T) {
	s := engine.Selection{Namespaces: []string{"team"}, Selector: "app=demo"}
	ok, err := s.Includes("a", "team", map[string]string{"app": "demo"})
	require.NoError(t, err)
	assert.True(t, ok)
	ok, _ = s.Includes("a", "other", map[string]string{"app": "demo"})
	assert.False(t, ok)
	ok, _ = s.Includes("a", "", map[string]string{"app": "demo"})
	assert.True(t, ok, "a cluster-scoped object has no namespace to match")
}

func TestAnnotations(t *testing.T) {
	a := engine.AnnotationsFor("inrun.dev")
	assert.Equal(t, "inrun.dev/migrate-field-snapshot", a.FieldSnapshot)
	assert.True(t, a.IsTooling("inrun.dev/managed-by"))
	assert.True(t, a.IsTooling("kubectl.kubernetes.io/last-applied-configuration"))
	assert.False(t, a.IsTooling("example.com/note"))
}

func TestKubeCheck(t *testing.T) {
	assert.ErrorContains(t, engine.Kube{}.Check(), "build it with ForConfig")
}
