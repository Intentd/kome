package kind

import (
	"context"
	"encoding/json"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/intentd/kome/engine"
	"github.com/intentd/tools/kubeclient"
	"github.com/intentd/tools/structured"
)

type downOutcome int

const (
	downNone     downOutcome = iota // no record
	downRestored                    // source back, destination deleted
	downKept                        // source back, destination kept: the migration did not create it
	downDrifted                     // destination edited after the migration; left alone
)

func (mg *migrator) processDown(ctx context.Context, dst *unstructured.Unstructured) (engine.ObjectResult, string) {
	// Report the source being restored: after a scope change the two differ.
	ref := dst
	if src, ok := mg.recordedSource(dst); ok {
		ref = &unstructured.Unstructured{}
		ref.SetNamespace(src.Metadata.Namespace)
		ref.SetName(src.Metadata.Name)
	}
	outcome, after, err := mg.down(ctx, dst)
	if err != nil {
		return mg.Fail(ref, err)
	}
	res := engine.ObjectResult{Namespace: ref.GetNamespace(), Name: ref.GetName()}
	switch outcome {
	case downRestored:
		res.Fields, res.AfterSpec = []string{"spec"}, after
	case downKept:
		res.Fields, res.AfterSpec, res.Kept = []string{"spec"}, after, true
	case downDrifted:
		res.AllDrifted = true
	default:
		res.Skipped = true
	}
	return res, ""
}

// down undoes a move from the destination's record. The source is recreated
// before the destination is deleted, so a failed create leaves both the
// destination and its record.
func (mg *migrator) down(ctx context.Context, dst *unstructured.Unstructured) (downOutcome, map[string]any, error) {
	raw := dst.GetAnnotations()[mg.Annotations.KindSnapshot]
	if raw == "" {
		return downNone, nil, nil
	}
	var rec record
	if err := engine.DecodeRecord(raw, &rec); err != nil {
		return downNone, nil, err
	}
	current, _, _ := unstructured.NestedMap(dst.Object, "spec")
	if len(structured.MissingPaths(rec.Source.Spec, current)) > 0 {
		return downDrifted, nil, nil
	}
	// Always: a later run may have pruned the source after the record was
	// written. recreate skips a source that still exists.
	if err := mg.recreate(ctx, &rec); err != nil {
		return downNone, nil, fmt.Errorf("recreate the source: %w", err)
	}
	// The migration did not create this destination: drop its record from it
	// and leave it as it was.
	if rec.DestinationExisted {
		if err := mg.dropRecord(ctx, dst); err != nil {
			return downNone, nil, fmt.Errorf("keep the destination: %w", err)
		}
		return downKept, rec.Source.Spec, nil
	}
	if err := mg.Kube.Delete(ctx, dst, metav1.DeleteOptions{DryRun: mg.DryRunOpt(), Preconditions: unchanged(dst)}); err != nil {
		return downNone, nil, fmt.Errorf("delete the destination: %w", err)
	}
	return downRestored, rec.Source.Spec, nil
}

// recreate creates the source from the record; one that still exists is
// left as it is.
func (mg *migrator) recreate(ctx context.Context, rec *record) error {
	gvk := schema.FromAPIVersionAndKind(rec.Source.APIVersion, rec.Source.Kind)
	gvr, err := kubeclient.GVRForGVK(mg.Kube, gvk)
	if err != nil {
		return fmt.Errorf("resolve the source's resource: %w", err)
	}
	meta := rec.Source.Metadata
	var ri dynamic.ResourceInterface = mg.Kube.DynamicClient().Resource(gvr)
	if meta.Namespace != "" {
		ri = mg.Kube.DynamicClient().Resource(gvr).Namespace(meta.Namespace)
	}
	_, err = ri.Get(ctx, meta.Name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get the source: %w", err)
	}
	created, err := ri.Create(ctx, rec.sourceObject(), metav1.CreateOptions{DryRun: mg.DryRunOpt()})
	if err != nil {
		return fmt.Errorf("create the source: %w", err)
	}
	if err := mg.writeStatus(ctx, ri, created, rec.Source.Status); err != nil {
		return fmt.Errorf("restore the source's status: %w", err)
	}
	return nil
}

// dropRecord removes the record from a destination it keeps, guarded by the
// resourceVersion.
func (mg *migrator) dropRecord(ctx context.Context, dst *unstructured.Unstructured) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": dst.GetResourceVersion(),
		"annotations":     map[string]any{mg.Annotations.KindSnapshot: nil, mg.Annotations.Timestamp: nil},
	}})
	if err != nil {
		return err
	}
	_, err = mg.dstResource.Namespace(dst.GetNamespace()).Patch(ctx, dst.GetName(), k8stypes.MergePatchType, patch,
		metav1.PatchOptions{DryRun: mg.DryRunOpt()})
	return err
}
