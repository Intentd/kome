package kind

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/intentd/kome/pkg/engine"
	"github.com/intentd/tools/resolver"
	"github.com/intentd/tools/structured"
)

func (mg *migrator) processUp(ctx context.Context, src *unstructured.Unstructured) (engine.ObjectResult, string) {
	res := engine.ObjectResult{Namespace: src.GetNamespace(), Name: src.GetName()}
	if !mg.Admits(mg.sel, src) {
		res.Skipped, res.Excluded = true, true
		return res, ""
	}
	dst, err := mg.destination(src, nil)
	if err != nil {
		return mg.Fail(src, err)
	}
	srcSpec, _, _ := unstructured.NestedMap(src.Object, "spec")
	if owner, ok := mg.claimed[engine.ObjRef(dst)]; ok && owner != engine.ObjRef(src) {
		return mg.Fail(src, fmt.Errorf("destination %s is also the target of %s in this run", engine.ObjRef(dst), owner))
	}

	existing, err := mg.get(ctx, dst)
	if err != nil {
		return mg.Fail(src, err)
	}
	if existing != nil {
		owner, migrated := mg.recordedSource(existing)
		if migrated && !owner.is(src) {
			return mg.Fail(src, fmt.Errorf("destination %s was already migrated from %s", engine.ObjRef(existing), owner.ref()))
		}
		mg.claim(dst, src)
		live, _, _ := unstructured.NestedMap(existing.Object, "spec")
		if len(structured.MissingPaths(srcSpec, live)) > 0 {
			res.Drifted = true
			return res, ""
		}
		// Already migrated, or written by someone else first: finish a prune
		// an earlier run did not.
		if mg.m.Prune {
			plan, err := mg.planPrune(ctx, src, existing.GetNamespace())
			if err != nil {
				return mg.Fail(src, err)
			}
			// Record the plan before acting on it, so down can undo what the
			// prune does. A destination the migration did not create has no
			// record yet: the source is recorded on it, marked as existing.
			rec := mg.newRecord(src, existing, true, true)
			if migrated {
				if rec, err = mg.recordOf(existing); err != nil {
					return mg.Fail(src, err)
				}
				rec.Pruned = true
			}
			if err := mg.recordOn(ctx, existing, rec.withPlan(mg.m.Children, plan)); err != nil {
				return mg.Fail(src, err)
			}
			if err := mg.prune(ctx, src, existing, plan); err != nil {
				return mg.Fail(src, err)
			}
			res.Pruned = true
			return res, ""
		}
		res.Skipped = true
		return res, ""
	}

	mg.claim(dst, src)
	// A prune is planned before anything is written, and its plan goes into
	// the destination's record with the destination itself.
	var plan *prunePlan
	if mg.m.Prune {
		if plan, err = mg.planPrune(ctx, src, dst.GetNamespace()); err != nil {
			return mg.Fail(src, err)
		}
		if dst, err = mg.destination(src, plan); err != nil {
			return mg.Fail(src, err)
		}
	}
	// Dry-run first: the server applies defaults and drops unknown fields,
	// so this shows what the destination would really hold.
	preview, err := mg.apply(ctx, dst, true)
	if err != nil {
		return mg.Fail(src, err)
	}
	previewSpec, _, _ := unstructured.NestedMap(preview.Object, "spec")
	if missing := structured.MissingPaths(srcSpec, previewSpec); len(missing) > 0 {
		return mg.Fail(src, fmt.Errorf("the destination schema drops or changes fields: %s", strings.Join(missing, ", ")))
	}
	res.Fields, res.AfterSpec = []string{"spec"}, previewSpec
	if mg.DryRun {
		res.Pruned = mg.m.Prune
		return res, ""
	}
	applied, err := mg.apply(ctx, dst, false)
	if err != nil {
		return mg.Fail(src, err)
	}
	if mg.m.CopyStatus {
		status, _, _ := unstructured.NestedMap(src.Object, "status")
		if err := mg.writeStatus(ctx, mg.dstResource.Namespace(dst.GetNamespace()), applied, status); err != nil {
			return mg.Fail(src, fmt.Errorf("copy status: %w", err))
		}
	}
	if mg.m.Prune {
		if err := mg.prune(ctx, src, applied, plan); err != nil {
			return mg.Fail(src, err)
		}
		res.Pruned = true
	}
	return res, ""
}

// destination builds the destination of src with its record (including the
// prune's plan, when there is one); it is not applied. It keeps the source's
// name and spec, takes its namespace by scope, and its labels and annotations
// per the migration.
func (mg *migrator) destination(src *unstructured.Unstructured, plan *prunePlan) (*unstructured.Unstructured, error) {
	dst := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": mg.m.To.GroupVersion().String(),
		"kind":       mg.m.To.Kind,
	}}
	dst.SetName(src.GetName())
	switch {
	case mg.dstCluster:
	case src.GetNamespace() == "":
		dst.SetNamespace(mg.m.Namespace)
	default:
		dst.SetNamespace(src.GetNamespace())
	}
	if spec, ok := src.Object["spec"]; ok {
		dst.Object["spec"] = runtime.DeepCopyJSONValue(spec)
	}

	r := mg.Resolver(src.Object)
	labels, err := mg.meta("label", mg.m.Labels, src.GetLabels(), r)
	if err != nil {
		return nil, err
	}
	ann, err := mg.meta("annotation", mg.m.Annotations, src.GetAnnotations(), r)
	if err != nil {
		return nil, err
	}
	if len(labels) > 0 {
		dst.SetLabels(labels)
	}

	ann[mg.Annotations.Timestamp] = time.Now().UTC().Format(time.RFC3339)
	rec, err := mg.encodeRecord(src, dst, mg.m.Prune, false, plan, ann)
	if err != nil {
		return nil, err
	}
	ann[mg.Annotations.KindSnapshot] = rec
	dst.SetAnnotations(ann)
	return dst, nil
}

// meta is declared merged over source (when preserved, tooling keys left
// out), each declared value resolved against the source.
func (mg *migrator) meta(what string, declared, source map[string]string, r *resolver.Resolver) (map[string]string, error) {
	out := make(map[string]string, len(declared)+len(source))
	if mg.m.PreserveSource {
		for k, v := range source {
			if !mg.Annotations.IsTooling(k) {
				out[k] = v
			}
		}
	}
	resolved, err := r.ResolveMap(declared)
	if err != nil {
		return nil, fmt.Errorf("resolve %s %w", what, err)
	}
	for k, v := range resolved {
		out[k] = v
	}
	return out, nil
}

// get returns the live destination, or nil when it does not exist.
func (mg *migrator) get(ctx context.Context, dst *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	existing, err := mg.dstResource.Namespace(dst.GetNamespace()).Get(ctx, dst.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get the destination: %w", err)
	}
	return existing, nil
}

// apply writes the new destination and returns what the server produced:
// server-side applied under KOME's own manager, created under an operator's
// (see engine.Runner.Apply), so the operator's next apply does not remove it.
func (mg *migrator) apply(ctx context.Context, dst *unstructured.Unstructured, dryRun bool) (*unstructured.Unstructured, error) {
	if !mg.Apply {
		opts := metav1.CreateOptions{FieldManager: mg.FieldManager}
		if dryRun {
			opts.DryRun = []string{metav1.DryRunAll}
		}
		out, err := mg.dstResource.Namespace(dst.GetNamespace()).Create(ctx, dst, opts)
		if err != nil {
			return nil, fmt.Errorf("create the destination: %w", err)
		}
		return out, nil
	}
	body, err := json.Marshal(dst.Object)
	if err != nil {
		return nil, fmt.Errorf("encode the destination: %w", err)
	}
	opts := metav1.PatchOptions{FieldManager: mg.FieldManager, Force: ptr.To(mg.Force)}
	if dryRun {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	out, err := mg.dstResource.Namespace(dst.GetNamespace()).Patch(ctx, dst.GetName(), k8stypes.ApplyPatchType, body, opts)
	if err != nil {
		return nil, fmt.Errorf("apply the destination: %w", err)
	}
	return out, nil
}

// recordOn writes rec onto an existing destination before its source is
// pruned. It carries the destination's resourceVersion, so it fails rather
// than write over an edit.
func (mg *migrator) recordOn(ctx context.Context, existing *unstructured.Unstructured, rec record) error {
	ann := map[string]string{}
	for k, v := range existing.GetAnnotations() {
		ann[k] = v
	}
	ann[mg.Annotations.Timestamp] = time.Now().UTC().Format(time.RFC3339)
	raw, err := engine.EncodeRecord(rec, mg.Annotations.KindSnapshot, ann)
	if err != nil {
		return err
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": existing.GetResourceVersion(),
		"annotations": map[string]string{
			mg.Annotations.KindSnapshot: raw,
			mg.Annotations.Timestamp:    ann[mg.Annotations.Timestamp],
		},
	}})
	if err != nil {
		return err
	}
	_, err = mg.dstResource.Namespace(existing.GetNamespace()).Patch(ctx, existing.GetName(),
		k8stypes.MergePatchType, patch, metav1.PatchOptions{DryRun: mg.DryRunOpt(), FieldManager: mg.FieldManager})
	if apierrors.IsConflict(err) {
		return fmt.Errorf("the destination changed during the migration; source not pruned, run again to review")
	}
	if err != nil {
		return fmt.Errorf("record the source on the existing destination: %w", err)
	}
	return nil
}

func (mg *migrator) claim(dst, src *unstructured.Unstructured) {
	mg.claimed[engine.ObjRef(dst)] = engine.ObjRef(src)
}
