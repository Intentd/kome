// Package kind moves objects of one kind to another: each source is rebuilt
// as a destination object and server-side applied, with a record on the
// destination that makes the move reversible. down recreates sources and
// removes the destinations the migration created; clean removes the records.
package kind

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/intentd/kome/engine"
)

// Migration is a kind-level migration.
type Migration struct {
	Name     string
	From, To schema.GroupVersionKind
	// Namespace receives objects of a cluster-scoped source moving to a
	// namespaced destination.
	Namespace string
	// Labels and Annotations are declared on the destination (templates);
	// the source's are copied first when PreserveSource.
	Labels, Annotations map[string]string
	PreserveSource      bool
	CopyStatus          bool
	Prune               bool
	Selection           engine.Selection
}

// Run runs m in r's mode, scoped by scope.
func Run(ctx context.Context, r *engine.Runner, m Migration, scope engine.Selection) (*engine.Result, error) {
	to := m.To.Kind
	if to == m.From.Kind {
		to += "." + m.To.Group // same kind: the group is what changes
	}
	result := &engine.Result{Migration: m.Name, Kind: m.From.Kind, To: to}
	warn := func(w string) { result.Warnings = append(result.Warnings, w) }

	sel, err := m.Selection.Narrow(scope)
	if err != nil {
		return result, err
	}
	result.Selection = sel
	srcResource, srcCluster, err := r.Resource(m.From)
	if err != nil {
		return result, err
	}
	dstResource, dstCluster, err := r.Resource(m.To)
	if err != nil {
		return result, err
	}
	if srcCluster && !dstCluster && m.Namespace == "" {
		return result, fmt.Errorf("to.namespace is required: %s is cluster-scoped and %s is namespaced", m.From.Kind, m.To.Kind)
	}
	if !srcCluster && dstCluster {
		warn(fmt.Sprintf("%s is cluster-scoped: namespaces are dropped", m.To.Kind))
	}

	// up reads sources; down and clean read the destinations recording them.
	var items []unstructured.Unstructured
	if r.Mode == engine.Up {
		items, err = r.List(ctx, sel, m.From.Kind, srcResource, srcCluster, warn)
	} else {
		items, err = listMigrated(ctx, r, sel, m.From, dstResource, dstCluster)
	}
	if err != nil {
		return result, err
	}

	mg := &migrator{Runner: r, m: m, sel: sel, dstResource: dstResource, dstCluster: dstCluster, claimed: map[string]string{}}
	var strictErrs []string
	for i := range items {
		r.Report(m.Name, i+1, len(items))
		res, strictErr := mg.process(ctx, &items[i])
		result.Objects = append(result.Objects, res)
		if strictErr != "" {
			strictErrs = append(strictErrs, strictErr)
		}
	}
	return result, engine.StrictError(strictErrs)
}

type migrator struct {
	*engine.Runner
	m           Migration
	sel         engine.Selection
	dstResource dynamic.NamespaceableResourceInterface
	dstCluster  bool
	// claimed maps each destination this run writes (or would) to its
	// source, so two sources landing on one name fail even in a dry-run.
	claimed map[string]string
}

func (mg *migrator) process(ctx context.Context, obj *unstructured.Unstructured) (engine.ObjectResult, string) {
	if obj.GetDeletionTimestamp() != nil {
		return engine.ObjectResult{Namespace: obj.GetNamespace(), Name: obj.GetName(), Skipped: true}, ""
	}
	switch mg.Mode {
	case engine.Clean:
		return mg.processClean(ctx, obj)
	case engine.Down:
		return mg.processDown(ctx, obj)
	default:
		return mg.processUp(ctx, obj)
	}
}

// ── up ────────────────────────────────────────────────────────────────────
