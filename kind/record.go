package kind

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/intentd/kome/engine"
	"github.com/intentd/tools/structured"
)

// record is what a destination carries about its move: the source's
// identity, metadata, spec and status (to recreate it if it was pruned), the
// destination's identity (to delete it), whether the source was pruned, and
// whether the destination existed before (then down keeps it).
type record struct {
	Source             recordSource `json:"source"`
	Destination        recordTarget `json:"destination"`
	Pruned             bool         `json:"pruned"`
	DestinationExisted bool         `json:"destinationExisted,omitempty"`
}

type recordSource struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   recordMeta     `json:"metadata"`
	Spec       map[string]any `json:"spec"`
	Status     map[string]any `json:"status,omitempty"`
}

type recordMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

type recordTarget struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
}

// encodeRecord records src moving to dst, encoded for a destination whose
// other annotations are ann (see engine.EncodeRecord). Tooling annotations
// are left out of the record: restoring them would claim a provenance the
// restore did not produce.
func (mg *migrator) encodeRecord(src, dst *unstructured.Unstructured, pruned, existed bool, ann map[string]string) (string, error) {
	rec := record{
		Source: recordSource{
			APIVersion: src.GetAPIVersion(),
			Kind:       src.GetKind(),
			Metadata: recordMeta{
				Name:        src.GetName(),
				Namespace:   src.GetNamespace(),
				Labels:      structured.CopyStringMap(src.GetLabels()),
				Annotations: mg.userAnnotations(src.GetAnnotations()),
			},
			Spec:   structured.CopySpec(src.Object["spec"]),
			Status: structured.CopySpec(src.Object["status"]),
		},
		Destination: recordTarget{APIVersion: dst.GetAPIVersion(), Kind: dst.GetKind()},
		Pruned:      pruned, DestinationExisted: existed,
	}
	rec.Destination.Metadata.Name, rec.Destination.Metadata.Namespace = dst.GetName(), dst.GetNamespace()
	return engine.EncodeRecord(rec, mg.Annotations.KindSnapshot, ann)
}

func (mg *migrator) userAnnotations(ann map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range ann {
		if !mg.Annotations.IsTooling(k) {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// recordedSource returns the source recorded on a destination.
func (mg *migrator) recordedSource(dst *unstructured.Unstructured) (recordSource, bool) {
	raw := dst.GetAnnotations()[mg.Annotations.KindSnapshot]
	if raw == "" {
		return recordSource{}, false
	}
	var rec record
	if err := engine.DecodeRecord(raw, &rec); err != nil {
		return recordSource{}, false
	}
	return rec.Source, true
}

func (s recordSource) is(obj *unstructured.Unstructured) bool {
	return s.APIVersion == obj.GetAPIVersion() && s.Kind == obj.GetKind() &&
		s.Metadata.Namespace == obj.GetNamespace() && s.Metadata.Name == obj.GetName()
}

func (s recordSource) ref() string {
	return s.Kind + " " + engine.Ref(s.Metadata.Namespace, s.Metadata.Name)
}

// isFrom reports whether the record was written by a move from gvk.
func (r record) isFrom(gvk schema.GroupVersionKind) bool {
	return r.Source.APIVersion == gvk.GroupVersion().String() && r.Source.Kind == gvk.Kind
}

// sourceObject rebuilds the recorded source.
func (r record) sourceObject() *unstructured.Unstructured {
	src := &unstructured.Unstructured{Object: map[string]any{"apiVersion": r.Source.APIVersion, "kind": r.Source.Kind}}
	meta := r.Source.Metadata
	src.SetName(meta.Name)
	if meta.Namespace != "" {
		src.SetNamespace(meta.Namespace)
	}
	if len(meta.Labels) > 0 {
		src.SetLabels(meta.Labels)
	}
	if len(meta.Annotations) > 0 {
		src.SetAnnotations(meta.Annotations)
	}
	if r.Source.Spec != nil {
		src.Object["spec"] = runtime.DeepCopyJSONValue(r.Source.Spec)
	}
	if r.Source.Status != nil {
		src.Object["status"] = runtime.DeepCopyJSONValue(r.Source.Status)
	}
	return src
}
