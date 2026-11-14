// Package kome migrates Kubernetes objects between schemas: field moves
// within a kind, and kind moves to another kind, group or scope, each
// reversible from a record on the object until it is made final.
//
// declaration reads the KOME file; Engine runs it against one cluster:
//
//	f, _ := declaration.Load("kome.yaml")
//	ms, _ := kome.FromFile(f)
//	results, err := (&kome.Engine{Kube: kube}).Up(ctx, ms, kome.Options{})
package kome

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/intentd/kel"

	"github.com/intentd/kome/declaration"
	"github.com/intentd/kome/engine"
	"github.com/intentd/kome/field"
	"github.com/intentd/kome/kind"
	"github.com/intentd/kome/transitional"
	"github.com/intentd/tools/kube"
	"github.com/intentd/tools/kubeclient"
	"github.com/intentd/tools/resolver"
)

// Migration is one migration of the file, ready to run: a field migration
// or a kind migration.
type Migration struct {
	Name  string
	Level declaration.Level
	Field field.Migration
	Kind  kind.Migration
}

// Migrations are the migrations of a file, in name order.
type Migrations []Migration

// FromFile turns the file's migrations into runs, in name order. It
// resolves identities but does not validate; run f.Validate first.
func FromFile(f *declaration.File) (Migrations, error) {
	var out Migrations
	for _, name := range f.Names() {
		d := f.Migrations[name]
		from, err := f.Identify(d.From)
		if err != nil {
			return nil, fmt.Errorf("%s.from: %w", name, err)
		}
		to, err := f.Identify(d.To.Shape)
		if err != nil {
			return nil, fmt.Errorf("%s.to: %w", name, err)
		}
		sel := engine.Selection{
			Namespaces: d.Selection.Namespaces, Name: d.Selection.Name, Selector: d.Selection.Selector,
			When: d.Selection.When, Or: d.Selection.Or,
		}
		m := Migration{Name: name, Level: d.Level()}
		switch m.Level {
		case declaration.LevelField:
			fm := field.Migration{Name: name, GVK: to.GVK, Selection: sel}
			for _, key := range sortedKeys(d.Spec) {
				fl := d.Spec[key]
				fm.Moves = append(fm.Moves, field.Move{
					Key: key, From: fl.SourcePath(key),
					Field: resolver.Field{Path: fl.To, Value: fl.Value, Values: fl.Values},
				})
			}
			m.Field = fm
		case declaration.LevelKind:
			km := kind.Migration{
				Name: name, From: from.GVK, To: to.GVK, Namespace: d.To.Namespace,
				PreserveSource: d.To.Metadata.Preserve(), CopyStatus: d.To.CopyStatus, Prune: d.Prune, Selection: sel,
			}
			if md := d.To.Metadata; md != nil {
				km.Labels, km.Annotations = md.Labels, md.Annotations
			}
			m.Kind = km
		}
		out = append(out, m)
	}
	return out, nil
}

// Transitional returns field migration name's transitional schema (see
// package transitional). Both of its sides need a schema file.
func Transitional(f *declaration.File, name string) ([]byte, error) {
	d, ok := f.Migrations[name]
	if !ok {
		return nil, fmt.Errorf("no migration %q", name)
	}
	if d.Level() != declaration.LevelField {
		return nil, fmt.Errorf("%s moves the kind: only field moves have a transitional schema", name)
	}
	if d.From.Schema == "" || d.To.Schema == "" {
		return nil, fmt.Errorf("%s: generating needs from.schema and to.schema files", name)
	}
	var moves []transitional.Move
	for _, key := range sortedKeys(d.Spec) {
		fl := d.Spec[key]
		moves = append(moves, transitional.Move{From: fl.SourcePath(key), To: fl.Destinations(key)})
	}
	out, err := transitional.Generate(f.Path(d.From.Schema), f.Path(d.To.Schema), moves)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// Transformer writes a field move's new values into spec from the old value.
// A caller that applies the same transform elsewhere passes its own, so both
// always agree.
type Transformer = field.Transformer

// Engine runs migrations against one cluster. It does not log; what it met
// is in its results.
type Engine struct {
	Kube kubeclient.Interface
	// Funcs are the template functions transforms, metadata and conditions
	// use; nil is KEL's.
	Funcs template.FuncMap
	// Resolver, when set, makes the resolver for each object's data
	// instead of resolver.New with Funcs: for a caller whose functions are
	// bound to the object (Inrun's Blueprint functions).
	Resolver func(data map[string]any) *resolver.Resolver
	// Transformer applies field moves; nil is resolver.ApplyField with the
	// resolver above.
	Transformer Transformer
	// Prefix is the record's annotation prefix; empty is engine.DefaultPrefix.
	Prefix string
	// FieldManager names KOME's writes; empty is "kome".
	FieldManager string
}

// Options are the settings of one run.
type Options struct {
	DryRun bool
	// Strict turns per-object failures into an error.
	Strict bool
	// Force makes server-side apply take ownership on conflict.
	Force    bool
	PageSize int64
	// Scope narrows each migration's own selection.
	Scope engine.Selection
	// NoLiveCheck skips the check before up that the live schema declares
	// every field move's paths, for a caller that checks schemas itself.
	NoLiveCheck bool
	// Progress, when set, is called after each object.
	Progress func(migration string, done, total int)
}

// Up migrates objects to their declared shape.
func (e *Engine) Up(ctx context.Context, ms Migrations, o Options) ([]*engine.Result, error) {
	return e.run(ctx, engine.Up, ms, o)
}

// Down restores migrated objects from their records.
func (e *Engine) Down(ctx context.Context, ms Migrations, o Options) ([]*engine.Result, error) {
	return e.run(ctx, engine.Down, ms, o)
}

// Clean removes the records, making the migrations final.
func (e *Engine) Clean(ctx context.Context, ms Migrations, o Options) ([]*engine.Result, error) {
	return e.run(ctx, engine.Clean, ms, o)
}

// run runs each migration in mode, stopping at the first migration-level
// error; results so far are returned with it.
func (e *Engine) run(ctx context.Context, mode engine.Mode, ms Migrations, o Options) ([]*engine.Result, error) {
	funcs := e.Funcs
	if funcs == nil {
		funcs = kel.FuncMap()
	}
	prefix := e.Prefix
	if prefix == "" {
		prefix = engine.DefaultPrefix
	}
	fm := e.FieldManager
	if fm == "" {
		fm = "kome"
	}
	rsv := e.Resolver
	if rsv == nil {
		rsv = func(data map[string]any) *resolver.Resolver { return resolver.New(data, funcs) }
	}
	r := &engine.Runner{
		Kube: e.Kube, Mode: mode, Annotations: engine.AnnotationsFor(prefix), FieldManager: fm, Resolver: rsv,
		DryRun: o.DryRun, Strict: o.Strict, Force: o.Force, PageSize: o.PageSize, Progress: o.Progress,
	}
	t := e.Transformer
	if t == nil {
		t = applyField{resolver: rsv}
	}

	var results []*engine.Result
	for _, m := range ms {
		res, err := e.runOne(ctx, r, t, m, o)
		if res != nil {
			results = append(results, res)
		}
		if err != nil {
			return results, fmt.Errorf("%s: %w", m.Name, err)
		}
	}
	return results, nil
}

func (e *Engine) runOne(ctx context.Context, r *engine.Runner, t Transformer, m Migration, o Options) (*engine.Result, error) {
	scope := o.Scope
	switch m.Level {
	case declaration.LevelField:
		if err := crdExists(e.Kube, m.Field.GVK); err != nil {
			return nil, err
		}
		if r.Mode == engine.Up && !o.NoLiveCheck {
			if err := e.checkLive(ctx, m.Field); err != nil {
				return nil, err
			}
		}
		return field.Run(ctx, r, t, m.Field, scope)
	case declaration.LevelKind:
		for _, gvk := range []schema.GroupVersionKind{m.Kind.From, m.Kind.To} {
			if err := crdExists(e.Kube, gvk); err != nil {
				return nil, err
			}
		}
		return kind.Run(ctx, r, m.Kind, scope)
	}
	return nil, fmt.Errorf("unknown level %q", m.Level)
}

// crdExists fails early, naming the kind, rather than on the first List.
func crdExists(k kubeclient.Interface, gvk schema.GroupVersionKind) error {
	if _, err := k.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		return fmt.Errorf("%s %s is not installed in the cluster: %w", gvk.GroupKind(), gvk.Version, err)
	}
	return nil
}

// checkLive refuses a field migration whose live schema does not declare
// every from and to path: the API server would drop the writes silently.
func (e *Engine) checkLive(ctx context.Context, m field.Migration) error {
	mapping, err := e.Kube.RESTMapper().RESTMapping(m.GVK.GroupKind(), m.GVK.Version)
	if err != nil {
		return err
	}
	name := mapping.Resource.Resource + "." + m.GVK.Group
	crd, err := kube.GetCRD(ctx, e.Kube.DynamicClient(), name)
	if err != nil {
		return err
	}
	spec := kube.StorageSpecSchema(crd)
	if spec == nil {
		return nil // no schema: nothing is dropped
	}
	var missing []string
	for _, mv := range m.Moves {
		for _, p := range append([]string{mv.From}, mv.Destinations()...) {
			if !kube.SchemaHoldsV1(spec, p) && !slices.Contains(missing, "spec."+p) {
				missing = append(missing, "spec."+p)
			}
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the live schema of %s does not declare %s: apply the transitional schema first (kome transitional)", name, strings.Join(missing, ", "))
	}
	return nil
}

// applyField is the default Transformer: resolver.ApplyField over the spec.
type applyField struct {
	resolver func(map[string]any) *resolver.Resolver
}

func (a applyField) Transform(spec map[string]any, m field.Move, old any) error {
	return a.resolver(spec).ApplyField(spec, m.Key, m.Field, old)
}

func sortedKeys(m map[string]declaration.Field) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
