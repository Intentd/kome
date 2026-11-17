// Package kome migrates Kubernetes objects between schemas: field moves
// within a kind, and kind moves to another kind, group or scope, each
// reversible from a record on the object until it is made final.
//
// declaration reads the KOME file; Engine runs it against one cluster:
//
//	f, _ := declaration.Load("kome.yaml")
//	ms, _ := kome.FromFile(f)
//	kube, err := kome.KubeForConfig(cfg, nil)
//	results, err := (&kome.Engine{Kube: kube}).Up(ctx, ms, kome.Options{})
//
// KubeForConfig builds the cluster's clients from a rest.Config.
package kome

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"text/template"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"

	"github.com/intentd/kel"

	"github.com/intentd/kome/declaration"
	"github.com/intentd/kome/engine"
	"github.com/intentd/kome/field"
	"github.com/intentd/kome/kind"
	"github.com/intentd/kome/transitional"
	"github.com/intentd/tools/kube"
	"github.com/intentd/tools/resolver"
)

// Migration is one migration of the file, ready to run: a field migration
// or a kind migration.
type Migration struct {
	Name  string
	Level declaration.Level
	Field field.Migration
	Kind  kind.Migration
	// FieldManager is the file's fieldManager: the migration's writes are
	// attributed to it, with plain create and patch. Engine.FieldManager,
	// when set, wins.
	FieldManager string
	// Needs are the migrations that must be done first; NeededBy those that
	// need this one. Up checks the first are not pending, down that the
	// second are not applied (when they are not part of the same run).
	Needs, NeededBy []string
}

// Migrations are the migrations of a file, each after the ones it needs.
type Migrations []Migration

// Named returns the migration called name.
func (ms Migrations) Named(name string) (Migration, bool) {
	for _, m := range ms {
		if m.Name == name {
			return m, true
		}
	}
	return Migration{}, false
}

// names is the set of the migrations' names.
func (ms Migrations) names() map[string]bool {
	out := make(map[string]bool, len(ms))
	for _, m := range ms {
		out[m.Name] = true
	}
	return out
}

// FromFile turns the file's migrations into runs, each after the ones it
// needs (name order where needs leave a choice). It resolves identities but
// does not validate; run f.Validate first.
func FromFile(f *declaration.File) (Migrations, error) {
	order, err := f.Order()
	if err != nil {
		return nil, err
	}
	neededBy := map[string][]string{}
	for _, name := range order {
		for _, n := range f.Migrations[name].Needs {
			neededBy[n] = append(neededBy[n], name)
		}
	}
	var out Migrations
	for _, name := range order {
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
		m := Migration{Name: name, Level: d.Level(), FieldManager: d.FieldManager, Needs: d.Needs, NeededBy: neededBy[name]}
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
				PreserveSource: d.To.Metadata.Preserve(), CopyStatus: d.To.CopyStatus, Selection: sel,
			}
			if lc := d.Lifecycle; lc != nil {
				km.Prune, km.Children, km.Finalizers = lc.Prune, lc.Children, lc.Finalizers
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
// package transitional). When name needs other field migrations of the same
// kind, the schema covers the whole chain: every path any of them reads or
// writes, so objects not yet moved by an earlier step keep their values
// while the later schema is in place. Each step needs a from schema file,
// and name a to schema file.
func Transitional(f *declaration.File, name string) ([]byte, error) {
	d, ok := f.Migrations[name]
	if !ok {
		return nil, fmt.Errorf("no migration %q", name)
	}
	if d.Level() != declaration.LevelField {
		return nil, fmt.Errorf("%s moves the kind: only field moves have a transitional schema", name)
	}
	if d.To.Schema == "" {
		return nil, fmt.Errorf("%s: generating needs a to.schema file", name)
	}
	chain, err := fieldChain(f, name)
	if err != nil {
		return nil, err
	}
	var steps []transitional.Step
	for _, n := range chain {
		s := f.Migrations[n]
		if s.From.Schema == "" {
			return nil, fmt.Errorf("%s: generating needs a from.schema file", n)
		}
		var moves []transitional.Move
		for _, key := range sortedKeys(s.Spec) {
			fl := s.Spec[key]
			moves = append(moves, transitional.Move{From: fl.SourcePath(key), To: fl.Destinations(key)})
		}
		steps = append(steps, transitional.Step{FromFile: f.Path(s.From.Schema), Moves: moves})
	}
	out, err := transitional.GenerateChain(f.Path(d.To.Schema), steps...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// fieldChain is name and the field migrations of the same kind it needs,
// directly or through each other, in the order they run.
func fieldChain(f *declaration.File, name string) ([]string, error) {
	order, err := f.Order()
	if err != nil {
		return nil, err
	}
	kindOf := func(n string) (string, bool) {
		d := f.Migrations[n]
		id, err := f.Identify(d.From)
		if err != nil || d.Level() != declaration.LevelField {
			return "", false
		}
		return id.GVK.GroupKind().String(), true
	}
	want, _ := kindOf(name)
	in := map[string]bool{name: true}
	var walk func(n string)
	walk = func(n string) {
		for _, dep := range f.Migrations[n].Needs {
			if k, ok := kindOf(dep); ok && k == want && !in[dep] {
				in[dep] = true
				walk(dep)
			}
		}
	}
	walk(name)
	var out []string
	for _, n := range order {
		if in[n] {
			out = append(out, n)
		}
	}
	return out, nil
}

// Transformer writes a field move's new values into spec from the old value.
// A caller that applies the same transform elsewhere passes its own, so both
// always agree.
type Transformer = field.Transformer

// Kube is the cluster KOME works on: client-go's dynamic client, REST mapper
// and discovery client. KubeForConfig builds it.
type Kube = engine.Kube

// KubeForConfig builds a Kube from cfg. mapper is the caller's REST mapper;
// nil builds one that rediscovers kinds it does not know (see
// engine.ForConfig).
func KubeForConfig(cfg *rest.Config, mapper meta.RESTMapper) (Kube, error) {
	return engine.ForConfig(cfg, mapper)
}

// Engine runs migrations against one cluster. It does not log; what it met
// is in its results.
type Engine struct {
	// Kube is the cluster: client-go's dynamic client, REST mapper and
	// discovery, from KubeForConfig.
	Kube Kube
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
	// FieldManager names KOME's writes for every migration, over the file's
	// fieldManager; empty leaves each migration's own, else "kome". A name
	// given here is the caller's own, so destinations are server-side
	// applied under it.
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
	// All is every migration of the file, so needs reaching outside the
	// migrations being run can be checked: up refuses while a migration it
	// needs is pending, down while one that needs it is applied.
	All Migrations
}

// Up migrates objects to their declared shape, each migration after the ones
// it needs. A needed migration that is not part of ms must be done already.
func (e *Engine) Up(ctx context.Context, ms Migrations, o Options) ([]*engine.Result, error) {
	for _, m := range ms {
		for _, n := range m.Needs {
			if ms.names()[n] {
				continue
			}
			nm, err := o.lookup(n, m.Name)
			if err != nil {
				return nil, err
			}
			pending, err := e.count(ctx, engine.Up, nm, o)
			if err != nil {
				return nil, fmt.Errorf("%s needs %s: %w", m.Name, n, err)
			}
			if pending > 0 {
				return nil, fmt.Errorf("%s needs %s, which is still pending on %s: run it first, or both together",
					m.Name, n, engine.Objects(pending, "object"))
			}
		}
	}
	return e.run(ctx, engine.Up, ms, o)
}

// Down restores migrated objects from their records, latest migration first.
// A migration that needs one of ms and is not part of it must be undone
// already.
func (e *Engine) Down(ctx context.Context, ms Migrations, o Options) ([]*engine.Result, error) {
	for _, m := range ms {
		for _, d := range m.NeededBy {
			if ms.names()[d] {
				continue
			}
			dm, err := o.lookup(d, m.Name)
			if err != nil {
				return nil, err
			}
			applied, err := e.count(ctx, engine.Down, dm, o)
			if err != nil {
				return nil, fmt.Errorf("%s is needed by %s: %w", m.Name, d, err)
			}
			if applied > 0 {
				return nil, fmt.Errorf("%s is needed by %s, which is still applied on %s: undo it first, or both together",
					m.Name, d, engine.Objects(applied, "object"))
			}
		}
	}
	reversed := make(Migrations, len(ms))
	for i, m := range ms {
		reversed[len(ms)-1-i] = m
	}
	return e.run(ctx, engine.Down, reversed, o)
}

// lookup finds a migration named by another's needs in o.All.
func (o Options) lookup(name, by string) (Migration, error) {
	m, ok := o.All.Named(name)
	if !ok {
		return Migration{}, fmt.Errorf("%s names %s in its needs, which is not among the migrations given (Options.All)", by, name)
	}
	return m, nil
}

// count dry-runs m in mode and returns how many objects it would act on: in
// up, those still to migrate (pending); in down, those carrying its record
// (applied), drifted ones included.
func (e *Engine) count(ctx context.Context, mode engine.Mode, m Migration, o Options) (int, error) {
	o.DryRun, o.Strict, o.Progress = true, false, nil
	res, err := e.run(ctx, mode, Migrations{m}, o)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range res {
		for _, obj := range r.Objects {
			if len(obj.Fields) > 0 || len(obj.DriftedFields) > 0 || obj.AllDrifted || (mode == engine.Up && obj.Err != nil && !obj.Excluded) {
				n++
			}
		}
	}
	return n, nil
}

// Status is where a migration stands on the cluster.
type Status struct {
	Migration string `json:"migration"`
	// Pending objects still have the source shape.
	Pending int `json:"pending"`
	// Migrated objects carry the migration's record and can be undone.
	Migrated int `json:"migrated"`
	// Drifted objects carry the record but were edited since; down leaves
	// what changed alone.
	Drifted int `json:"drifted"`
}

// Status reports, for each migration, how many objects are pending, migrated
// and drifted, from the objects themselves: KOME keeps no history.
func (e *Engine) Status(ctx context.Context, ms Migrations, o Options) ([]Status, error) {
	o.DryRun, o.Strict, o.Progress = true, false, nil
	out := make([]Status, 0, len(ms))
	for _, m := range ms {
		s := Status{Migration: m.Name}
		up, err := e.run(ctx, engine.Up, Migrations{m}, o)
		if err != nil {
			return out, fmt.Errorf("%s: %w", m.Name, err)
		}
		for _, r := range up {
			for _, obj := range r.Objects {
				if len(obj.Fields) > 0 {
					s.Pending++
				}
			}
		}
		down, err := e.run(ctx, engine.Down, Migrations{m}, o)
		if err != nil {
			return out, fmt.Errorf("%s: %w", m.Name, err)
		}
		for _, r := range down {
			for _, obj := range r.Objects {
				switch {
				case obj.AllDrifted || len(obj.DriftedFields) > 0:
					s.Drifted++
				case len(obj.Fields) > 0:
					s.Migrated++
				}
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// Clean removes the records, making the migrations final.
func (e *Engine) Clean(ctx context.Context, ms Migrations, o Options) ([]*engine.Result, error) {
	return e.run(ctx, engine.Clean, ms, o)
}

// run runs each migration in mode, stopping at the first migration-level
// error; results so far are returned with it.
func (e *Engine) run(ctx context.Context, mode engine.Mode, ms Migrations, o Options) ([]*engine.Result, error) {
	if err := e.Kube.Check(); err != nil {
		return nil, err
	}
	funcs := e.Funcs
	if funcs == nil {
		funcs = kel.FuncMap()
	}
	prefix := e.Prefix
	if prefix == "" {
		prefix = engine.DefaultPrefix
	}
	rsv := e.Resolver
	if rsv == nil {
		rsv = func(data map[string]any) *resolver.Resolver { return resolver.New(data, funcs) }
	}
	r := &engine.Runner{
		Kube: e.Kube, Mode: mode, Annotations: engine.AnnotationsFor(prefix), Resolver: rsv,
		DryRun: o.DryRun, Strict: o.Strict, Force: o.Force, PageSize: o.PageSize, Progress: o.Progress,
	}
	t := e.Transformer
	if t == nil {
		t = applyField{resolver: rsv}
	}

	var results []*engine.Result
	for _, m := range ms {
		r := r.WithFieldManager(e.fieldManager(m))
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

// fieldManager is the manager migration m writes as, and whether it is KOME's
// own (the caller's, or the default "kome"), which may server-side apply. A
// manager taken from the file is someone else's.
func (e *Engine) fieldManager(m Migration) (string, bool) {
	switch {
	case e.FieldManager != "":
		return e.FieldManager, true
	case m.FieldManager != "":
		return m.FieldManager, false
	default:
		return "kome", true
	}
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
func crdExists(k Kube, gvk schema.GroupVersionKind) error {
	if _, err := k.Mapping(gvk); err != nil {
		return fmt.Errorf("%s %s is not installed in the cluster: %w", gvk.GroupKind(), gvk.Version, err)
	}
	return nil
}

// checkLive refuses a field migration whose live schema does not declare
// every from and to path: the API server would drop the writes silently.
func (e *Engine) checkLive(ctx context.Context, m field.Migration) error {
	mapping, err := e.Kube.Mapping(m.GVK)
	if err != nil {
		return err
	}
	name := mapping.Resource.Resource + "." + m.GVK.Group
	crd, err := kube.GetCRD(ctx, e.Kube.Dynamic(), name)
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
