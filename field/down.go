package field

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/intentd/kome/engine"
	"github.com/intentd/tools/structured"
)

// down restores obj from its record. A move whose values changed since the
// migration is drifted and left alone, with its record kept.
func (mg migrator) down(ctx context.Context, obj *unstructured.Unstructured, resource dynamic.NamespaceableResourceInterface) (restored, driftedMoves []string, after map[string]any, err error) {
	if obj.GetAnnotations()[mg.Annotations.FieldSnapshot] == "" {
		return nil, nil, nil, nil
	}
	all, record, err := mg.record(obj)
	if err != nil {
		return nil, nil, nil, err
	}
	if len(record) == 0 {
		return nil, nil, nil, nil // another migration's record only
	}
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	if spec == nil {
		spec = map[string]any{}
	}
	remaining := make(map[string]any, len(record))
	for k, v := range record {
		remaining[k] = v
	}
	patchSpec := map[string]any{}
	manager := mg.FieldManager
	for _, m := range mg.moves {
		raw, ok := record[m.From]
		if !ok {
			continue
		}
		before, afterFrag, ok := parseEntry(raw)
		if !ok {
			continue
		}
		if drifted(spec, afterFrag) {
			driftedMoves = append(driftedMoves, m.Key)
			continue
		}
		for path, v := range before {
			_ = structured.SetNestedPath(spec, path, v)
			_ = structured.SetNestedPath(patchSpec, path, v)
		}
		for path, v := range before {
			if v == nil {
				removeEmptyParents(spec, patchSpec, path)
			}
		}
		restored = append(restored, m.Key)
		delete(remaining, m.From)
		// Undo as the manager that made the move, even if the file changed since.
		if fm, _ := raw.(map[string]any)["fieldManager"].(string); fm != "" {
			manager = fm
		}
	}
	if len(restored) == 0 {
		return nil, driftedMoves, nil, nil
	}

	ann := map[string]any{}
	rest := mg.withMine(all, remaining)
	if len(rest) == 0 {
		ann[mg.Annotations.FieldSnapshot] = nil
		if _, kindRecord := obj.GetAnnotations()[mg.Annotations.KindSnapshot]; !kindRecord {
			ann[mg.Annotations.Timestamp] = nil
		}
	} else {
		encoded, err := engine.EncodeRecord(rest, mg.Annotations.FieldSnapshot, obj.GetAnnotations())
		if err != nil {
			return nil, driftedMoves, nil, err
		}
		ann[mg.Annotations.FieldSnapshot] = encoded
	}
	patch := map[string]any{
		"apiVersion": obj.GetAPIVersion(),
		"kind":       obj.GetKind(),
		"metadata":   engine.Meta(obj, ann, true),
		"spec":       patchSpec,
	}
	if err := mg.WithFieldManager(manager, mg.Apply).Patch(ctx, resource, obj, patch); err != nil {
		return restored, driftedMoves, spec, fmt.Errorf("apply the restore: %w", err)
	}
	return restored, driftedMoves, spec, nil
}

// removeEmptyParents nulls, in the patch, each parent of a removed path that
// the removal left empty, so a restore leaves no "scaling: {}" behind.
func removeEmptyParents(spec, patch map[string]any, path string) {
	parts := strings.Split(path, ".")
	for i := len(parts) - 1; i > 0; i-- {
		parent := strings.Join(parts[:i], ".")
		v, found := structured.GetNestedPath(spec, parent)
		m, isMap := v.(map[string]any)
		if !found || !isMap || !allNil(m) {
			return
		}
		structured.DeleteNestedPath(spec, parent)
		_ = structured.SetNestedPath(patch, parent, nil)
	}
}

func allNil(m map[string]any) bool {
	for _, v := range m {
		if v != nil {
			return false
		}
	}
	return true
}
