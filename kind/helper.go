package kind

import (
	"context"
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/intentd/kome/engine"
	"github.com/intentd/tools/kubeclient"
	"github.com/intentd/tools/structured"
)

// unchanged returns delete preconditions that match obj as it was read.
func unchanged(obj *unstructured.Unstructured) *metav1.Preconditions {
	uid, rv := obj.GetUID(), obj.GetResourceVersion()
	p := &metav1.Preconditions{}
	if uid != "" {
		p.UID = &uid
	}
	if rv != "" {
		p.ResourceVersion = &rv
	}
	return p
}

// writeStatus sets obj's status: through the status subresource, or the
// object itself when it has none. Nothing is written when status is empty or
// already there, or in a dry-run.
func (mg *migrator) writeStatus(ctx context.Context, ri dynamic.ResourceInterface, obj *unstructured.Unstructured, status map[string]any) error {
	if len(status) == 0 || mg.DryRun || obj == nil {
		return nil
	}
	if reflect.DeepEqual(structured.JSONNormalize(obj.Object["status"]), structured.JSONNormalize(status)) {
		return nil
	}
	u := obj.DeepCopy()
	u.Object["status"] = runtime.DeepCopyJSONValue(status)
	_, err := ri.UpdateStatus(ctx, u, metav1.UpdateOptions{})
	if apierrors.IsNotFound(err) {
		_, err = ri.Update(ctx, u, metav1.UpdateOptions{})
	}
	return err
}

// ── down ──────────────────────────────────────────────────────────────────

// listMigrated lists the destinations whose record names from as the source
// and whose recorded source is in sel. Labels and namespace are matched on
// the record: the destination's may differ.
func listMigrated(ctx context.Context, r *engine.Runner, sel engine.Selection, from schema.GroupVersionKind, dstResource dynamic.NamespaceableResourceInterface, dstCluster bool) ([]unstructured.Unstructured, error) {
	items, err := kubeclient.ListAllForNamespaces(ctx, dstResource, dstCluster, nil, metav1.ListOptions{Limit: r.PageSize})
	if err != nil {
		return nil, fmt.Errorf("list the destinations: %w", err)
	}
	var out []unstructured.Unstructured
	for _, item := range items {
		raw := item.GetAnnotations()[r.Annotations.KindSnapshot]
		if raw == "" {
			continue
		}
		var rec record
		if err := engine.DecodeRecord(raw, &rec); err != nil {
			out = append(out, item) // the per-object step reports it
			continue
		}
		if !rec.isFrom(from) {
			continue
		}
		src := rec.Source.Metadata
		ok, err := sel.Includes(src.Name, src.Namespace, src.Labels)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, item)
		}
	}
	return out, nil
}

// ── clean ─────────────────────────────────────────────────────────────────
