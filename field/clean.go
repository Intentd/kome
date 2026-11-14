package field

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/intentd/kome/engine"
)

// clean removes the record without touching spec. The timestamp is shared
// with kind moves; it stays while a kind record needs it.
func (mg migrator) clean(ctx context.Context, obj *unstructured.Unstructured, resource dynamic.NamespaceableResourceInterface) ([]string, error) {
	a := obj.GetAnnotations()
	_, hasField := a[mg.Annotations.FieldSnapshot]
	_, hasKind := a[mg.Annotations.KindSnapshot]
	_, hasTime := a[mg.Annotations.Timestamp]
	if !hasField && (!hasTime || hasKind) {
		return nil, nil
	}
	remove := map[string]any{mg.Annotations.FieldSnapshot: nil}
	if !hasKind {
		remove[mg.Annotations.Timestamp] = nil
	}
	patch := map[string]any{"apiVersion": obj.GetAPIVersion(), "kind": obj.GetKind(), "metadata": engine.Meta(obj, remove, false)}
	if err := mg.Patch(ctx, resource, obj, patch); err != nil {
		return []string{"annotations"}, fmt.Errorf("remove the record: %w", err)
	}
	return []string{"annotations"}, nil
}
