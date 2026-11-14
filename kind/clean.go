package kind

import (
	"context"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/intentd/kome/engine"
)

func (mg *migrator) processClean(ctx context.Context, dst *unstructured.Unstructured) (engine.ObjectResult, string) {
	ann := dst.GetAnnotations()
	if _, ok := ann[mg.Annotations.KindSnapshot]; !ok {
		return engine.ObjectResult{Namespace: dst.GetNamespace(), Name: dst.GetName(), Skipped: true}, ""
	}
	remove := map[string]any{mg.Annotations.KindSnapshot: nil}
	if _, hasField := ann[mg.Annotations.FieldSnapshot]; !hasField {
		remove[mg.Annotations.Timestamp] = nil
	}
	if err := mg.Patch(ctx, mg.dstResource, dst, map[string]any{"metadata": engine.Meta(dst, remove, false)}); err != nil {
		return mg.Fail(dst, err)
	}
	return engine.ObjectResult{Namespace: dst.GetNamespace(), Name: dst.GetName(), Fields: []string{"annotations"}}, ""
}
