package kome_test

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/intentd/tools/condition"
	"github.com/intentd/tools/kubeclient"
	"github.com/intentd/tools/kubeclient/kubefake"
	"github.com/intentd/tools/kubeclient/kubefake/object"
)

var (
	oldGVK = schema.GroupVersionKind{Group: "old.io", Version: "v1", Kind: "Old"}
	newGVK = schema.GroupVersionKind{Group: "new.io", Version: "v1", Kind: "New"}
	oldGVR = schema.GroupVersionResource{Group: "old.io", Version: "v1", Resource: "olds"}
	newGVR = schema.GroupVersionResource{Group: "new.io", Version: "v1", Resource: "news"}
	crdGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
)

// crd is the CRD of olds.old.io with spec schema specSchema (nil: preserve
// unknown fields).
func crd(specSchema map[string]any) *unstructured.Unstructured {
	if specSchema == nil {
		specSchema = map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "olds.old.io"},
		"spec": map[string]any{
			"group": "old.io", "scope": "Namespaced",
			"names": map[string]any{"kind": "Old", "plural": "olds"},
			"versions": []any{map[string]any{
				"name": "v1", "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"type": "object", "properties": map[string]any{"spec": specSchema},
				}},
			}},
		},
	}}
}

// fakeKube is kubefake's client with a dynamic client and mapper that know
// Old and New, server-side apply, and delete preconditions.
type fakeKube struct {
	*kubefake.Kubeclient
	dyn    *kubefake.Dynamic
	mapper meta.RESTMapper
}

// newFakeKube seeds objs, and a permissive CRD for Old unless objs has one.
func newFakeKube(objs ...*unstructured.Unstructured) *fakeKube {
	seed := make([]runtime.Object, 0, len(objs)+1)
	hasCRD := false
	for _, o := range objs {
		seed = append(seed, o)
		hasCRD = hasCRD || o.GetKind() == "CustomResourceDefinition"
	}
	if !hasCRD {
		seed = append(seed, crd(nil))
	}
	dyn := kubefake.NewDynamic(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{oldGVR: "OldList", newGVR: "NewList", crdGVR: "CustomResourceDefinitionList"}, seed...)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(oldGVK, meta.RESTScopeNamespace)
	mapper.Add(newGVK, meta.RESTScopeNamespace)
	f := &fakeKube{Kubeclient: kubefake.New(runtime.NewScheme()), dyn: dyn, mapper: mapper}
	tracker := dyn.Tracker()

	dyn.PrependReactor("patch", "*", func(a kubefake.Action) (bool, runtime.Object, error) {
		pa := a.(kubefake.PatchActionImpl)
		if pa.GetPatchType() != k8stypes.ApplyPatchType {
			return false, nil, nil
		}
		u := object.Empty()
		if err := u.UnmarshalJSON(pa.GetPatch()); err != nil {
			return true, nil, err
		}
		if len(pa.GetPatchOptions().DryRun) > 0 {
			return true, u, nil
		}
		gvr := pa.GetResource()
		if _, err := tracker.Get(gvr, pa.GetNamespace(), pa.GetName()); apierrors.IsNotFound(err) {
			return true, u, tracker.Create(gvr, u, pa.GetNamespace())
		}
		return true, u, tracker.Update(gvr, u, pa.GetNamespace())
	})
	// A dry-run merge patch returns the object without storing the change.
	dyn.PrependReactor("patch", "*", func(a kubefake.Action) (bool, runtime.Object, error) {
		pa := a.(kubefake.PatchActionImpl)
		if pa.GetPatchType() == k8stypes.ApplyPatchType || len(pa.GetPatchOptions().DryRun) == 0 {
			return false, nil, nil
		}
		cur, err := tracker.Get(pa.GetResource(), pa.GetNamespace(), pa.GetName())
		return true, cur, err
	})
	dyn.PrependReactor("delete", "*", func(a kubefake.Action) (bool, runtime.Object, error) {
		da := a.(kubefake.DeleteActionImpl)
		opts := da.GetDeleteOptions()
		if p := opts.Preconditions; p != nil && p.ResourceVersion != nil {
			cur, err := tracker.Get(da.GetResource(), da.GetNamespace(), da.GetName())
			if err == nil && cur.(metav1.Object).GetResourceVersion() != *p.ResourceVersion {
				return true, nil, apierrors.NewConflict(da.GetResource().GroupResource(), da.GetName(), fmt.Errorf("resourceVersion changed"))
			}
		}
		return len(opts.DryRun) > 0, nil, nil
	})
	return f
}

func (f *fakeKube) DynamicClient() dynamic.Interface { return f.dyn }
func (f *fakeKube) RESTMapper() meta.RESTMapper      { return f.mapper }

func (f *fakeKube) Delete(ctx context.Context, obj kubeclient.Object, opts metav1.DeleteOptions) error {
	u := obj.(*unstructured.Unstructured)
	m, err := f.mapper.RESTMapping(u.GroupVersionKind().GroupKind(), u.GroupVersionKind().Version)
	if err != nil {
		return err
	}
	return f.dyn.Resource(m.Resource).Namespace(u.GetNamespace()).Delete(ctx, u.GetName(), opts)
}

func (f *fakeKube) get(gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	u, err := f.dyn.Resource(gvr).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return u
}

func oldObj(name string, spec map[string]any) *unstructured.Unstructured {
	return object.New(oldGVK.GroupVersion().String(), oldGVK.Kind, name, object.InNamespace("team"),
		object.WithSpec(spec), object.WithLabels(map[string]string{"app": "demo"}))
}

func metav1UpdateOptions() metav1.UpdateOptions { return metav1.UpdateOptions{} }

func whenEquals(path, value string) condition.Conditions {
	return condition.Conditions{{Field: path, Equals: value}}
}
