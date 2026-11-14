// Package field migrates objects in place: each field move reads the old
// value, transforms it, and writes it at its new paths in one guarded merge
// patch, with a record annotation that makes it reversible. down restores
// from the record, leaving fields edited since alone; clean removes the
// record.
package field

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/intentd/kome/engine"
	"github.com/intentd/tools/resolver"
)

// Move is one field move. Paths are relative to spec.
type Move struct {
	// Key names the move in results.
	Key string
	// From is the stored path read.
	From string
	// Field is where and how the value is written: Path (default Key),
	// Value, or Values.
	Field resolver.Field
}

type Moves []Move

// writesUnder reports whether the move writes its old path, or below it (a
// fanout into the value's own key): then the old path is not nulled.
func (m Move) writesUnder() bool {
	for _, d := range m.Destinations() {
		if d == m.From || strings.HasPrefix(d, m.From+".") {
			return true
		}
	}
	return false
}

// Destinations returns the paths the move writes.
func (m Move) Destinations() []string {
	if len(m.Field.Values) > 0 {
		out := make([]string, 0, len(m.Field.Values))
		for p := range m.Field.Values {
			out = append(out, p)
		}
		slices.Sort(out)
		return out
	}
	if m.Field.Path != "" {
		return []string{m.Field.Path}
	}
	return []string{m.Key}
}

// Transformer writes a move's new values into spec from the old value. The
// default is resolver.ApplyField with the runner's functions.
type Transformer interface {
	Transform(spec map[string]any, m Move, old any) error
}

// Migration is a field-level migration of one kind.
type Migration struct {
	Name      string
	GVK       schema.GroupVersionKind
	Moves     Moves
	Selection engine.Selection
}

// Run runs m in r's mode, scoped by scope.
func Run(ctx context.Context, r *engine.Runner, t Transformer, m Migration, scope engine.Selection) (*engine.Result, error) {
	result := &engine.Result{Migration: m.Name, Kind: m.GVK.Kind}
	warn := func(w string) { result.Warnings = append(result.Warnings, w) }
	if len(m.Moves) == 0 {
		return result, nil
	}
	resource, clusterScoped, err := r.Resource(m.GVK)
	if err != nil {
		return result, err
	}
	sel, err := m.Selection.Narrow(scope)
	if err != nil {
		return result, err
	}
	result.Selection = sel

	items, err := r.List(ctx, sel, m.GVK.Kind, resource, clusterScoped, warn)
	if err != nil {
		return result, err
	}
	mg := migrator{Runner: r, t: t, moves: m.Moves}
	var strictErrs []string
	for i := range items {
		r.Report(m.Name, i+1, len(items))
		obj := &items[i]
		if r.Mode == engine.Up && !r.Admits(sel, obj) {
			result.Objects = append(result.Objects, engine.ObjectResult{Namespace: obj.GetNamespace(), Name: obj.GetName(), Skipped: true, Excluded: true})
			continue
		}
		res, strictErr := mg.process(ctx, obj, resource)
		result.Objects = append(result.Objects, res)
		if strictErr != "" {
			strictErrs = append(strictErrs, strictErr)
		}
	}
	return result, engine.StrictError(strictErrs)
}

type migrator struct {
	*engine.Runner
	t     Transformer
	moves Moves
}

func (mg migrator) process(ctx context.Context, obj *unstructured.Unstructured, resource dynamic.NamespaceableResourceInterface) (engine.ObjectResult, string) {
	if obj.GetDeletionTimestamp() != nil {
		return engine.ObjectResult{Namespace: obj.GetNamespace(), Name: obj.GetName(), Skipped: true}, ""
	}
	var changed, drifted []string
	var after map[string]any
	var err error
	switch mg.Mode {
	case engine.Clean:
		changed, err = mg.clean(ctx, obj, resource)
	case engine.Down:
		changed, drifted, after, err = mg.down(ctx, obj, resource)
	default:
		changed, after, err = mg.up(ctx, obj, resource)
	}
	res := engine.ObjectResult{
		Namespace:     obj.GetNamespace(),
		Name:          obj.GetName(),
		DriftedFields: drifted,
		Skipped:       len(changed) == 0 && len(drifted) == 0 && err == nil,
		AllDrifted:    mg.Mode == engine.Down && len(changed) == 0 && len(drifted) > 0 && err == nil,
	}
	if len(changed) > 0 {
		res.Fields, res.AfterSpec = changed, after
	}
	if err != nil {
		res.Err = err
		if mg.Strict {
			return res, fmt.Sprintf("%s: %s", engine.ObjRef(obj), err)
		}
	}
	return res, ""
}
