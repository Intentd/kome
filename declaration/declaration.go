// Package declaration is the KOME file: the migrations it declares, read
// strictly, with each migration's level found from its content and the
// offline checks that need no cluster.
//
//	migrations:
//	  cronjob:                     # field level: same kind, values move
//	    from: {schema: ./schemas/cronjob-old.yaml}
//	    to:   {schema: ./schemas/cronjob.yaml}
//	    spec:
//	      replicas: {to: scaling.min}
//	  appconfig:                   # kind level: objects move to another kind
//	    from: {schema: ./schemas/appconfig.yaml}
//	    to:   {schema: ./schemas/newconfig.yaml, copyStatus: true}
//	    lifecycle:
//	      prune: true
//	      children: reparent
//	      finalizers: [example.io/cleanup]
package declaration

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"sigs.k8s.io/yaml"

	"github.com/intentd/tools/condition"
	"github.com/intentd/tools/fileio"
)

// File is a KOME file. It is not a Kubernetes object: its one key is
// migrations.
type File struct {
	Migrations map[string]Migration `json:"migrations"`

	// Dir is the file's directory; relative paths in it resolve from here.
	Dir string `json:"-"`
}

// Migration is one entry of the file.
type Migration struct {
	// From is the schema the objects are stored under, and those objects.
	From Shape `json:"from"`
	// Transitional is the schema holding both the old and the new paths while
	// objects move (field level). Generated when absent.
	Transitional *Transitional `json:"transitional,omitempty"`
	// To is the schema the objects end up under.
	To Destination `json:"to"`
	// Spec are the field moves, keyed by the stored path under spec (field
	// level).
	Spec map[string]Field `json:"spec,omitempty"`
	// Selection narrows the objects migrated.
	Selection Selection `json:"selection,omitempty"`
	// Lifecycle is what happens to each source after its move (kind level).
	Lifecycle *Lifecycle `json:"lifecycle,omitempty"`
	// Needs names the migrations that must be done before this one, as in
	// GitHub Actions: up runs them first and refuses while one is pending;
	// down undoes this one first and refuses while it is still needed.
	Needs []string `json:"needs,omitempty"`
	// FieldManager names the migration's writes; empty is KOME's own
	// ("kome"). Declaring an operator's manager attributes the changes to
	// it; KOME then writes with plain create and patch, never server-side
	// apply, so the operator's next apply does not remove what KOME wrote.
	FieldManager string `json:"fieldManager,omitempty"`
}

// Lifecycle is what a kind move does with each source once its destination
// holds every source field.
type Lifecycle struct {
	// Prune deletes the source.
	Prune bool `json:"prune,omitempty"`
	// Children is what happens to the objects the source owns (by
	// ownerReference) when it is pruned: reparent (to the destination),
	// orphan (keep them, ownerless) or delete (with the source). Required
	// when a source has children; a source with children and no policy is
	// not pruned.
	Children string `json:"children,omitempty"`
	// Finalizers are the finalizers KOME may remove from a source before
	// pruning it. A source with any other finalizer is not pruned.
	Finalizers []string `json:"finalizers,omitempty"`
}

// The children policies.
const (
	ChildrenReparent = "reparent"
	ChildrenOrphan   = "orphan"
	ChildrenDelete   = "delete"
)

// Prunes reports whether the migration deletes its sources.
func (m Migration) Prunes() bool { return m.Lifecycle != nil && m.Lifecycle.Prune }

// Shape is a schema and objects stored in it. The schema is a CRD manifest
// file, or, without one, a declared identity whose schema is read from the
// cluster.
type Shape struct {
	Schema     string   `json:"schema,omitempty"`
	APIVersion string   `json:"apiVersion,omitempty"`
	Kind       string   `json:"kind,omitempty"`
	Plural     string   `json:"plural,omitempty"`
	Objects    []string `json:"objects,omitempty"`
}

// Destination is the shape objects end up in, and how a kind move writes them.
type Destination struct {
	Shape
	// Namespace receives objects of a cluster-scoped source moving to a
	// namespaced destination.
	Namespace string `json:"namespace,omitempty"`
	// Metadata is the destination's labels and annotations.
	Metadata *Metadata `json:"metadata,omitempty"`
	// CopyStatus copies the source's status onto the destination.
	CopyStatus bool `json:"copyStatus,omitempty"`
}

// Metadata is a kind move's destination labels and annotations. The
// source's are copied first unless PreserveSource is false; declared keys
// win and may be templates.
type Metadata struct {
	PreserveSource *bool             `json:"preserveSource,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Annotations    map[string]string `json:"annotations,omitempty"`
}

// Preserve reports whether the source's labels and annotations are copied.
func (m *Metadata) Preserve() bool { return m == nil || m.PreserveSource == nil || *m.PreserveSource }

// Transitional is a declared transitional schema.
type Transitional struct {
	Schema string `json:"schema"`
}

// Field is one field move. Paths are relative to spec.
type Field struct {
	// From is the source path; empty is the key.
	From string `json:"from,omitempty"`
	// To is the destination path; empty is the key (a transform in place).
	To string `json:"to,omitempty"`
	// Value produces the new value, by how it is written: a template
	// ({{ .value }}, text), a CEL expression (${ value * 2 }, its type kept),
	// or the value itself. The old value is value; the spec's top-level
	// fields are the rest of the data.
	Value string `json:"value,omitempty"`
	// Values fans one value out: destination path → value, each written as
	// Value is. To and Value are ignored when it is set.
	Values map[string]string `json:"values,omitempty"`
}

// Selection narrows the objects a migration moves.
type Selection struct {
	Namespaces []string             `json:"namespaces,omitempty"`
	Name       string               `json:"name,omitempty"`
	Selector   string               `json:"selector,omitempty"`
	When       condition.Conditions `json:"when,omitempty"`
	Or         condition.Conditions `json:"or,omitempty"`
}

// Load reads a KOME file. Unknown keys are errors.
func Load(path string) (*File, error) {
	data, err := fileio.ReadLocal(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	f.Dir = filepath.Dir(abs)
	return f, nil
}

// Parse reads a KOME file from data; relative paths resolve from the working
// directory until Dir is set.
func Parse(data []byte) (*File, error) {
	var f File
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// Names returns the migration names, sorted.
func (f *File) Names() []string {
	names := make([]string, 0, len(f.Migrations))
	for n := range f.Migrations {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Order returns the migration names with each after the ones it needs, and
// in name order where needs leave a choice. A cycle or an unknown name is an
// error (Validate reports them too).
func (f *File) Order() ([]string, error) {
	names := f.Names()
	state := map[string]int{} // 0 unseen, 1 in progress, 2 done
	var out []string
	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch state[name] {
		case 2:
			return nil
		case 1:
			return fmt.Errorf("needs form a cycle: %s", strings.Join(append(path, name), " → "))
		}
		m, ok := f.Migrations[name]
		if !ok {
			return fmt.Errorf("%s needs %q, which is not in the file", path[len(path)-1], name)
		}
		state[name] = 1
		needs := append([]string(nil), m.Needs...)
		sort.Strings(needs)
		for _, n := range needs {
			if err := visit(n, append(path, name)); err != nil {
				return err
			}
		}
		state[name] = 2
		out = append(out, name)
		return nil
	}
	for _, name := range names {
		if err := visit(name, nil); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Path resolves p against the file's directory.
func (f *File) Path(p string) string {
	if p == "" || filepath.IsAbs(p) || f.Dir == "" {
		return p
	}
	return filepath.Join(f.Dir, p)
}

// SourcePath returns the field's source path: From, else the key.
func (fl Field) SourcePath(key string) string {
	if fl.From != "" {
		return fl.From
	}
	return key
}

// Destinations returns the field's destination paths: the Values keys, else
// To, else the key.
func (fl Field) Destinations(key string) []string {
	if len(fl.Values) > 0 {
		out := make([]string, 0, len(fl.Values))
		for p := range fl.Values {
			out = append(out, p)
		}
		sort.Strings(out)
		return out
	}
	if fl.To != "" {
		return []string{fl.To}
	}
	return []string{key}
}
