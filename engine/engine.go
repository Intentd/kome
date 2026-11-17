// Package engine holds what field-level and kind-level migrations share:
// the run settings, the record's annotation keys, selection, results, and
// the guarded writes. It does not log: what a run met goes into its results.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/intentd/tools/kubeclient"
	"github.com/intentd/tools/resolver"
)

// Mode is the operation a run performs.
type Mode int

const (
	// Up migrates objects to the declared shape.
	Up Mode = iota
	// Down restores objects from their records.
	Down
	// Clean removes the records without touching spec, making the migration
	// final.
	Clean
)

// String returns the command name of the mode.
func (m Mode) String() string {
	switch m {
	case Up:
		return "up"
	case Down:
		return "down"
	case Clean:
		return "clean"
	default:
		return fmt.Sprintf("Mode(%d)", int(m))
	}
}

// Annotations are the keys of the record each migrated object carries.
// They are permanent: down and clean find records by them.
type Annotations struct {
	Prefix        string
	FieldSnapshot string
	KindSnapshot  string
	Timestamp     string
}

// DefaultPrefix is the annotation prefix KOME writes.
const DefaultPrefix = "kome.intentd.io"

// AnnotationsFor returns the record's keys under prefix.
func AnnotationsFor(prefix string) Annotations {
	return Annotations{
		Prefix:        prefix,
		FieldSnapshot: prefix + "/migrate-field-snapshot",
		KindSnapshot:  prefix + "/migrate-kind-snapshot",
		Timestamp:     prefix + "/migrate-timestamp",
	}
}

// IsTooling reports whether an annotation or label key is written by a tool
// (kubectl, or the record's own prefix) rather than declared by a user. Such
// keys are left out of records and copied metadata.
func (a Annotations) IsTooling(key string) bool {
	return strings.HasPrefix(key, "kubectl.kubernetes.io/") || strings.HasPrefix(key, a.Prefix+"/")
}

// Runner carries the settings of one run.
type Runner struct {
	Kube         Kube
	Mode         Mode
	Annotations  Annotations
	FieldManager string
	// Resolver returns the resolver templates over data are evaluated with:
	// selection conditions, kind-move metadata, field transforms.
	Resolver func(data map[string]any) *resolver.Resolver

	DryRun bool
	// Strict turns per-object failures into a migration-level error.
	Strict bool
	// Force makes server-side apply take ownership on conflict.
	Force bool
	// Apply is whether FieldManager is KOME's own, so destinations may be
	// server-side applied under it. A manager declared in the file belongs
	// to an operator: applying as it would make the operator's next apply
	// remove what KOME wrote, so writes are plain create and patch.
	Apply bool
	// PageSize is the List page size; 0 lets the API server decide.
	PageSize int64
	// Progress, when set, is called after each object.
	Progress func(migration string, done, total int)
}

// WithFieldManager returns a copy of r writing as fm; own says whether fm is
// KOME's own (see Apply).
func (r *Runner) WithFieldManager(fm string, own bool) *Runner {
	out := *r
	out.FieldManager, out.Apply = fm, own
	return &out
}

// Report calls Progress when it is set.
func (r *Runner) Report(migration string, done, total int) {
	if r.Progress != nil {
		r.Progress(migration, done, total)
	}
}

// DryRunOpt returns the server-side dry-run option when DryRun is set.
func (r *Runner) DryRunOpt() []string {
	if r.DryRun {
		return []string{metav1.DryRunAll}
	}
	return nil
}

// Patch writes a JSON merge patch. Merge patch is used because it does not
// validate the live object, which may still be in the old shape.
func (r *Runner) Patch(ctx context.Context, resource dynamic.NamespaceableResourceInterface, obj *unstructured.Unstructured, patch any) error {
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("marshal patch: %w", err)
	}
	_, err = kubeclient.PatchUnstructured(ctx, resource, obj, k8stypes.MergePatchType, body,
		metav1.PatchOptions{FieldManager: r.FieldManager, DryRun: r.DryRunOpt()})
	return err
}

// Resource resolves gvk's dynamic resource and whether it is cluster-scoped.
func (r *Runner) Resource(gvk schema.GroupVersionKind) (dynamic.NamespaceableResourceInterface, bool, error) {
	resource, clusterScoped, err := r.Kube.Resource(gvk)
	if err != nil {
		return nil, false, fmt.Errorf("resolve the resource of %s: %w", gvk.Kind, err)
	}
	return resource, clusterScoped, nil
}

// ObjectResult is the outcome for one object.
type ObjectResult struct {
	Namespace string
	Name      string
	// Fields are the fields migrated or restored.
	Fields []string
	// DriftedFields were left alone: they changed after the migration.
	DriftedFields []string
	// AllDrifted: a record was there, everything drifted, nothing restored.
	AllDrifted bool
	// Drifted (kind level): the destination exists and differs.
	Drifted bool
	// Pruned (kind level): the source was deleted, or would be in a dry-run.
	Pruned bool
	// Kept (kind down): the source is back and the destination kept, because
	// the migration did not create it.
	Kept bool
	// AfterSpec is spec after the change; nil if skipped or failed.
	AfterSpec map[string]any
	// Skipped: nothing to do.
	Skipped bool
	// Excluded: the object failed the selection's conditions; also Skipped.
	Excluded bool
	// TooLarge: its record would not fit in its annotations even compressed,
	// so it was not migrated (Err says by how much).
	TooLarge bool
	Err      error
}

// Ref returns "namespace/name", or "name" when cluster-scoped.
func (r ObjectResult) Ref() string { return Ref(r.Namespace, r.Name) }

// Result is the outcome of one migration.
type Result struct {
	Migration string
	Kind      string
	// To is the destination kind of a kind move; empty for a field move.
	To string
	// Selection is the scope the run used: the migration's merged with the
	// caller's.
	Selection Selection
	Objects   []ObjectResult
	// Warnings are things the run met that did not stop it.
	Warnings []string
}

// Fail builds the result of a failed object, and the line to collect in
// strict mode.
func (r *Runner) Fail(obj *unstructured.Unstructured, err error) (ObjectResult, string) {
	res := ObjectResult{Namespace: obj.GetNamespace(), Name: obj.GetName(), Err: err, TooLarge: errors.Is(err, ErrTooLarge)}
	if r.Strict {
		return res, fmt.Sprintf("%s: %s", ObjRef(obj), err)
	}
	return res, ""
}

// StrictError joins the collected strict-mode errors, or returns nil.
func StrictError(errs []string) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("migration errors:\n%s", strings.Join(errs, "\n"))
}

// Ref formats "namespace/name", or "name" when cluster-scoped.
func Ref(namespace, name string) string {
	if namespace != "" {
		return namespace + "/" + name
	}
	return name
}

// ObjRef formats an object as Ref does.
func ObjRef(obj *unstructured.Unstructured) string { return Ref(obj.GetNamespace(), obj.GetName()) }

// Meta is the metadata of a merge patch for obj: its name and namespace,
// and its resourceVersion when guarded, so the server rejects the patch
// (409) if obj changed since it was read.
func Meta(obj *unstructured.Unstructured, annotations map[string]any, guarded bool) map[string]any {
	meta := map[string]any{"name": obj.GetName(), "annotations": annotations}
	if ns := obj.GetNamespace(); ns != "" {
		meta["namespace"] = ns
	}
	if rv := obj.GetResourceVersion(); guarded && rv != "" {
		meta["resourceVersion"] = rv
	}
	return meta
}
