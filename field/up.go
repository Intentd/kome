package field

import (
	"context"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/intentd/kome/engine"
	"github.com/intentd/tools/structured"
)

// up migrates obj: for each move whose old value is there and not yet
// migrated, transform it, then write every change and the record in one
// merge patch guarded by the resourceVersion.
func (mg migrator) up(ctx context.Context, obj *unstructured.Unstructured, resource dynamic.NamespaceableResourceInterface) ([]string, map[string]any, error) {
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	if spec == nil {
		spec = map[string]any{}
	}
	all, record, err := mg.record(obj)
	if err != nil {
		return nil, nil, err
	}

	var migrated Moves
	entries := map[string]any{}
	for _, m := range mg.moves {
		old, ok := pending(spec, m, record)
		if !ok {
			continue
		}
		structured.DeleteNestedPath(spec, m.From)
		if err := mg.t.Transform(spec, m, old); err != nil {
			return nil, nil, fmt.Errorf("transform %q: %w", m.Key, err)
		}
		// A merge patch only sets the keys it holds: null the old path
		// unless the move writes it.
		if !m.writesUnder() {
			_ = structured.SetNestedPath(spec, m.From, nil)
		}
		migrated = append(migrated, m)
		entries[m.From] = snapshotEntry(spec, m, old, mg.FieldManager)
	}
	if len(migrated) == 0 {
		return nil, nil, nil
	}

	// Encode the record before the patch: a patch without it would leave the
	// object changed with no way back.
	for k, v := range entries {
		record[k] = v
	}
	now := time.Now().UTC().Format(time.RFC3339)
	ann := map[string]string{}
	for k, v := range obj.GetAnnotations() {
		ann[k] = v
	}
	ann[mg.Annotations.Timestamp] = now
	encoded, err := engine.EncodeRecord(mg.withMine(all, record), mg.Annotations.FieldSnapshot, ann)
	if err != nil {
		return nil, nil, err
	}
	patch := map[string]any{
		"apiVersion": obj.GetAPIVersion(),
		"kind":       obj.GetKind(),
		"metadata": engine.Meta(obj, map[string]any{
			mg.Annotations.FieldSnapshot: encoded,
			mg.Annotations.Timestamp:     now,
		}, true),
		"spec": sparse(spec, migrated),
	}
	names := keys(migrated)
	if err := mg.Patch(ctx, resource, obj, patch); err != nil {
		return names, spec, fmt.Errorf("apply the migration: %w", err)
	}
	return names, spec, nil
}

// pending returns m's old value when it still needs migrating: the value is
// there, and it is not already this move's output (its record's after still
// holds, or a fanout has already turned it into a structure).
func pending(spec map[string]any, m Move, record map[string]any) (any, bool) {
	old, found := structured.GetNestedPath(spec, m.From)
	if !found {
		return nil, false
	}
	if raw, ok := record[m.From]; ok {
		if _, after, ok := parseEntry(raw); ok && !drifted(spec, after) {
			return nil, false
		}
	}
	if m.writesUnder() {
		switch old.(type) {
		case map[string]any, []any:
			return nil, false
		}
	}
	return old, true
}

// snapshotEntry is the record of one move: the before and after values of
// every path it touched, as flat maps (nil: absent).
func snapshotEntry(spec map[string]any, m Move, old any, fieldManager string) map[string]any {
	before := map[string]any{m.From: old}
	after := map[string]any{}
	for _, dst := range m.Destinations() {
		after[dst], _ = structured.GetNestedPath(spec, dst)
		// Restoring the old path covers a destination under it.
		if dst != m.From && !strings.HasPrefix(dst, m.From+".") {
			before[dst] = nil
		}
	}
	if _, written := after[m.From]; !written {
		after[m.From] = nil
	}
	return map[string]any{"before": before, "after": after, "fieldManager": fieldManager}
}

// sparse is the spec of the merge patch: each migrated move's destinations,
// and its old path nulled when it moved away.
func sparse(spec map[string]any, moves Moves) map[string]any {
	out := map[string]any{}
	for _, m := range moves {
		for _, dst := range m.Destinations() {
			v, _ := structured.GetNestedPath(spec, dst)
			_ = structured.SetNestedPath(out, dst, v)
		}
		if !m.writesUnder() {
			_ = structured.SetNestedPath(out, m.From, nil)
		}
	}
	return out
}

func keys(moves Moves) []string {
	out := make([]string, len(moves))
	for i, m := range moves {
		out[i] = m.Key
	}
	return out
}
