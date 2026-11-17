package declaration

import (
	"encoding/json"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/intentd/tools/structured"
)

// Set changes migration name for one run: assignment is "key=value" with the
// key a dot path relative to the migration ("lifecycle.prune", "to.namespace",
// "spec.replicas.to") and the value YAML ("true", "[a, b]", a string). The
// result is decoded strictly, so an unknown key or a wrong type is an error;
// run Validate after.
func (f *File) Set(name, assignment string) error {
	m, ok := f.Migrations[name]
	if !ok {
		return fmt.Errorf("--set: no migration %q", name)
	}
	key, raw, ok := strings.Cut(assignment, "=")
	if !ok || strings.TrimSpace(key) == "" {
		return fmt.Errorf("--set %q: want key=value", assignment)
	}
	var value any
	if err := yaml.Unmarshal([]byte(raw), &value); err != nil {
		return fmt.Errorf("--set %s: %w", key, err)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	if err := structured.SetNestedPath(doc, key, value); err != nil {
		return fmt.Errorf("--set %s: %w", key, err)
	}
	b, err = json.Marshal(doc)
	if err != nil {
		return err
	}
	var out Migration
	if err := yaml.UnmarshalStrict(b, &out); err != nil {
		return fmt.Errorf("--set %s: %w", key, err)
	}
	f.Migrations[name] = out
	return nil
}
