package kind

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/discovery"
	"k8s.io/utils/ptr"

	"github.com/intentd/kome/declaration"
	"github.com/intentd/kome/engine"
	"github.com/intentd/tools/kubeclient"
)

// A prune deletes a source once its destination holds every source field.
// Deleting it naively would garbage-collect everything it owns and could hang
// on a finalizer, so a prune is planned first and refused unless the file
// says what to do:
//
//   - finalizers: only those in lifecycle.finalizers are removed; any other
//     refuses the prune;
//   - children (objects with an ownerReference to the source): with
//     lifecycle.children reparent they are pointed at the destination first,
//     with orphan they are kept without that owner, with delete they go with
//     the source; with no policy a source that has children is not pruned.

// childRef names an object a source owned; reparented ones are recorded so
// down can point them back.
type childRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
}

func (c childRef) String() string {
	if c.Namespace == "" {
		return c.Kind + "/" + c.Name
	}
	return c.Kind + " " + c.Namespace + "/" + c.Name
}

// child is an object that owns-references a source.
type child struct {
	childRef
	gvr schema.GroupVersionResource
}

// prunePlan is what pruning a source takes.
type prunePlan struct {
	children    []child
	reparent    bool
	finalizers  []string // to remove before deleting
	propagation *metav1.DeletionPropagation
}

// reparented is the plan's children as recorded.
func (p *prunePlan) reparented() []childRef {
	if p == nil || !p.reparent {
		return nil
	}
	out := make([]childRef, len(p.children))
	for i, c := range p.children {
		out[i] = c.childRef
	}
	return out
}

// planPrune checks src can be pruned as the file says, and returns how. What
// stands in the way is returned together, as an *engine.PruneBlocked, so a
// dry-run shows everything at once. dstNamespace is the destination's
// namespace.
func (mg *migrator) planPrune(ctx context.Context, src *unstructured.Unstructured, dstNamespace string) (*prunePlan, error) {
	plan := &prunePlan{}
	blocked := &engine.PruneBlocked{DeclaredFinalizers: mg.m.Finalizers}

	declared := map[string]bool{}
	for _, f := range mg.m.Finalizers {
		declared[f] = true
	}
	for _, f := range src.GetFinalizers() {
		if declared[f] {
			plan.finalizers = append(plan.finalizers, f)
		} else {
			blocked.UndeclaredFinalizers = append(blocked.UndeclaredFinalizers, f)
		}
	}

	children, err := mg.childrenOf(ctx, src)
	if err != nil {
		return nil, err
	}
	plan.children = children
	if len(children) > 0 {
		switch mg.m.Children {
		case declaration.ChildrenReparent:
			plan.reparent = true
			for _, c := range children {
				if !mg.dstCluster && (c.Namespace == "" || c.Namespace != dstNamespace) {
					blocked.CannotOwn = append(blocked.CannotOwn, c.String())
				}
			}
		case declaration.ChildrenOrphan:
			plan.propagation = ptr.To(metav1.DeletePropagationOrphan)
		case declaration.ChildrenDelete:
			plan.propagation = ptr.To(metav1.DeletePropagationBackground)
		default:
			for _, c := range children {
				blocked.Children = append(blocked.Children, c.String())
			}
		}
	}
	if !blocked.Empty() {
		return nil, blocked
	}
	return plan, nil
}

// prune runs the plan: children reparented to dst, declared finalizers
// removed, then src deleted with the plan's propagation. Every write carries
// the resourceVersion it read, so a concurrent change fails the prune rather
// than being overwritten; the next run picks it up. In a dry-run nothing is
// written: the plan's checks are the dry-run's answer.
func (mg *migrator) prune(ctx context.Context, src, dst *unstructured.Unstructured, plan *prunePlan) error {
	if plan == nil {
		plan = &prunePlan{}
	}
	if mg.DryRun {
		return mg.deleteSource(ctx, src, plan)
	}
	if plan.reparent {
		to := ownerRef(dst)
		for _, c := range plan.children {
			if err := mg.repoint(ctx, c, src.GetUID(), to); err != nil {
				return fmt.Errorf("reparent %s: %w", c, err)
			}
		}
	}
	current := src
	if len(plan.finalizers) > 0 {
		updated, err := mg.removeFinalizers(ctx, src, plan.finalizers)
		if err != nil {
			return err
		}
		current = updated
	}
	return mg.deleteSource(ctx, current, plan)
}

func (mg *migrator) deleteSource(ctx context.Context, src *unstructured.Unstructured, plan *prunePlan) error {
	err := mg.Kube.Delete(ctx, src, metav1.DeleteOptions{
		DryRun: mg.DryRunOpt(), Preconditions: unchanged(src), PropagationPolicy: plan.propagation,
	})
	if apierrors.IsConflict(err) {
		return fmt.Errorf("the source changed during the migration; not pruned, run again to review")
	}
	if err != nil {
		return fmt.Errorf("destination applied; prune the source: %w", err)
	}
	return nil
}

// removeFinalizers takes the declared finalizers off src and returns it as
// written, so the delete's precondition matches it.
func (mg *migrator) removeFinalizers(ctx context.Context, src *unstructured.Unstructured, remove []string) (*unstructured.Unstructured, error) {
	drop := map[string]bool{}
	for _, f := range remove {
		drop[f] = true
	}
	keep := []string{}
	for _, f := range src.GetFinalizers() {
		if !drop[f] {
			keep = append(keep, f)
		}
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": src.GetResourceVersion(),
		"finalizers":      keep,
	}})
	if err != nil {
		return nil, err
	}
	ri, err := mg.Kube.ResourceFor(src)
	if err != nil {
		return nil, err
	}
	out, err := ri.Patch(ctx, src.GetName(), k8stypes.MergePatchType, patch, metav1.PatchOptions{FieldManager: mg.FieldManager})
	if apierrors.IsConflict(err) {
		return nil, fmt.Errorf("the source changed during the migration; not pruned, run again to review")
	}
	if err != nil {
		return nil, fmt.Errorf("remove finalizers %s: %w", strings.Join(remove, ", "), err)
	}
	return out, nil
}

// repoint replaces the ownerReference to fromUID on c with to, keeping its
// controller and blockOwnerDeletion settings. A child that no longer
// references fromUID is left alone (already moved, or no longer a child).
func (mg *migrator) repoint(ctx context.Context, c child, fromUID k8stypes.UID, to metav1.OwnerReference) error {
	ri := mg.Kube.Dynamic().Resource(c.gvr).Namespace(c.Namespace)
	obj, err := ri.Get(ctx, c.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	refs := obj.GetOwnerReferences()
	changed := false
	for i, r := range refs {
		if r.UID != fromUID {
			continue
		}
		next := to
		next.Controller, next.BlockOwnerDeletion = r.Controller, r.BlockOwnerDeletion
		refs[i], changed = next, true
	}
	if !changed {
		return nil
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"resourceVersion": obj.GetResourceVersion(),
		"ownerReferences": refs,
	}})
	if err != nil {
		return err
	}
	_, err = ri.Patch(ctx, c.Name, k8stypes.MergePatchType, patch, metav1.PatchOptions{FieldManager: mg.FieldManager})
	if apierrors.IsConflict(err) {
		return fmt.Errorf("it changed during the migration; run again to review")
	}
	return err
}

// repointBack is down's half of a reparent: each recorded child that the
// destination owns is pointed at the restored source.
func (mg *migrator) repointBack(ctx context.Context, rec *record, dst, src *unstructured.Unstructured) error {
	if mg.DryRun || len(rec.Reparented) == 0 {
		return nil
	}
	to := ownerRef(src)
	for _, r := range rec.Reparented {
		gvk := schema.FromAPIVersionAndKind(r.APIVersion, r.Kind)
		m, err := mg.Kube.Mapping(gvk)
		if err != nil {
			return fmt.Errorf("resolve %s: %w", r, err)
		}
		if err := mg.repoint(ctx, child{childRef: r, gvr: m.Resource}, dst.GetUID(), to); err != nil {
			return fmt.Errorf("point %s back at the source: %w", r, err)
		}
	}
	return nil
}

func ownerRef(obj *unstructured.Unstructured) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: obj.GetAPIVersion(), Kind: obj.GetKind(), Name: obj.GetName(), UID: obj.GetUID()}
}

// ── Finding children ────────────────────────────────────────────────────────

// ownerIndex maps owner UIDs to the objects that reference them, built per
// namespace on first use; all covers every namespace and the cluster-scoped
// resources, for cluster-scoped owners.
type ownerIndex struct {
	types  []resourceType
	loaded map[string]bool          // namespace, or allNamespaces
	owners map[k8stypes.UID][]child // owner UID → children
}

const allNamespaces = "\x00all"

type resourceType struct {
	gvr        schema.GroupVersionResource
	gvk        schema.GroupVersionKind
	namespaced bool
}

// childrenOf returns the objects with an ownerReference to src. A namespaced
// owner can only own objects in its namespace; a cluster-scoped one can own
// objects anywhere.
func (mg *migrator) childrenOf(ctx context.Context, src *unstructured.Unstructured) ([]child, error) {
	if src.GetUID() == "" {
		return nil, nil
	}
	if mg.owned == nil {
		types, err := mg.resourceTypes()
		if err != nil {
			return nil, err
		}
		mg.owned = &ownerIndex{types: types, loaded: map[string]bool{}, owners: map[k8stypes.UID][]child{}}
	}
	scope := src.GetNamespace()
	if scope == "" {
		scope = allNamespaces
	}
	if err := mg.owned.load(ctx, mg, scope); err != nil {
		return nil, err
	}
	out := append([]child(nil), mg.owned.owners[src.GetUID()]...)
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

func (ix *ownerIndex) load(ctx context.Context, mg *migrator, scope string) error {
	if ix.loaded[scope] || ix.loaded[allNamespaces] {
		return nil
	}
	for _, t := range ix.types {
		var namespaces []string
		switch {
		case scope == allNamespaces:
		case !t.namespaced:
			continue // a namespaced owner can't own cluster-scoped objects
		default:
			namespaces = []string{scope}
		}
		items, err := kubeclient.ListAllForNamespaces(ctx, mg.Kube.Dynamic().Resource(t.gvr), !t.namespaced, namespaces,
			metav1.ListOptions{Limit: mg.PageSize})
		if err != nil {
			return fmt.Errorf("not pruned: list %s to look for children: %w", t.gvr.GroupResource(), err)
		}
		for i := range items {
			item := &items[i]
			for _, r := range item.GetOwnerReferences() {
				ix.owners[r.UID] = append(ix.owners[r.UID], child{
					childRef: childRef{APIVersion: t.gvk.GroupVersion().String(), Kind: t.gvk.Kind, Namespace: item.GetNamespace(), Name: item.GetName()},
					gvr:      t.gvr,
				})
			}
		}
	}
	ix.loaded[scope] = true
	return nil
}

// resourceTypes lists the listable resources at their preferred versions.
// Events are left out: they are many, and owning one is no reason to keep a
// source. A group the server can't describe (an unavailable aggregated API)
// is reported and skipped: its objects are not stored by the API server.
func (mg *migrator) resourceTypes() ([]resourceType, error) {
	groups, lists, err := mg.Kube.Discovery().ServerGroupsAndResources()
	if err != nil {
		if !discovery.IsGroupDiscoveryFailedError(err) {
			return nil, fmt.Errorf("not pruned: discover resources to look for children: %w", err)
		}
		mg.warn(fmt.Sprintf("looking for children: %v", err))
	}
	preferred := map[string]string{}
	for _, g := range groups {
		preferred[g.Name] = g.PreferredVersion.GroupVersion
	}
	var out []resourceType
	seen := map[schema.GroupResource]bool{}
	for _, l := range lists {
		gv, err := schema.ParseGroupVersion(l.GroupVersion)
		if err != nil {
			continue
		}
		if p, ok := preferred[gv.Group]; ok && p != "" && p != l.GroupVersion {
			continue
		}
		for _, r := range l.APIResources {
			gr := schema.GroupResource{Group: gv.Group, Resource: r.Name}
			if strings.Contains(r.Name, "/") || r.Kind == "Event" || seen[gr] || !hasVerb(r.Verbs, "list") {
				continue
			}
			seen[gr] = true
			out = append(out, resourceType{
				gvr:        gv.WithResource(r.Name),
				gvk:        gv.WithKind(r.Kind),
				namespaced: r.Namespaced,
			})
		}
	}
	return out, nil
}

func hasVerb(verbs []string, want string) bool {
	for _, v := range verbs {
		if v == want {
			return true
		}
	}
	return false
}
