package field

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/intentd/kome/engine"
)

// clean removes this migration's entries from the record without touching
// spec; other migrations' entries stay. The annotation goes with the last
// entry, and the timestamp, shared with kind moves, while no kind record
// needs it.
func (mg migrator) clean(ctx context.Context, obj *unstructured.Unstructured, resource dynamic.NamespaceableResourceInterface) ([]string, error) {
	a := obj.GetAnnotations()
	if a[mg.Annotations.FieldSnapshot] == "" {
		return nil, nil
	}
	all, mine, err := mg.record(obj)
	if err != nil {
		return nil, err
	}
	if len(mine) == 0 {
		return nil, nil
	}
	_, hasKind := a[mg.Annotations.KindSnapshot]
	change := map[string]any{}
	if rest := mg.withMine(all, nil); len(rest) > 0 {
		encoded, err := engine.EncodeRecord(rest, mg.Annotations.FieldSnapshot, a)
		if err != nil {
			return nil, err
		}
		change[mg.Annotations.FieldSnapshot] = encoded
	} else {
		change[mg.Annotations.FieldSnapshot] = nil
		if !hasKind {
			change[mg.Annotations.Timestamp] = nil
		}
	}
	patch := map[string]any{"apiVersion": obj.GetAPIVersion(), "kind": obj.GetKind(), "metadata": engine.Meta(obj, change, false)}
	if err := mg.Patch(ctx, resource, obj, patch); err != nil {
		return []string{"annotations"}, fmt.Errorf("remove the record: %w", err)
	}
	return []string{"annotations"}, nil
}
