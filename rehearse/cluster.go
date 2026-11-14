package rehearse

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/intentd/tools/kube"
	"github.com/intentd/tools/kubeclient"
)

// fieldManager names the rehearsal's own writes (schemas, seeds, edits).
const fieldManager = "kome-rehearse"

// Cluster is what a rehearsal does to the cluster besides migrating.
type Cluster interface {
	// ApplyCRD applies a CRD manifest and waits until it is established.
	ApplyCRD(ctx context.Context, manifest []byte) error
	// Apply server-side applies objects.
	Apply(ctx context.Context, objs []*unstructured.Unstructured) error
	// Get reads obj back, nil when it does not exist.
	Get(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error)
	// Update writes obj as it is.
	Update(ctx context.Context, obj *unstructured.Unstructured) error
}

// KubeCluster is a Cluster on a client.
type KubeCluster struct{ Kube kubeclient.Interface }

func (c KubeCluster) ApplyCRD(ctx context.Context, manifest []byte) error {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(manifest, &crd); err != nil {
		return fmt.Errorf("decode the CRD: %w", err)
	}
	crd.APIVersion, crd.Kind = "apiextensions.k8s.io/v1", "CustomResourceDefinition"
	crd.ResourceVersion = ""
	body, err := json.Marshal(crd)
	if err != nil {
		return err
	}
	force := true
	if _, err := c.Kube.DynamicClient().Resource(kube.CRDResource).Patch(ctx, crd.Name, k8stypes.ApplyPatchType, body,
		metav1.PatchOptions{FieldManager: fieldManager, Force: &force}); err != nil {
		return fmt.Errorf("apply the CRD %s: %w", crd.Name, err)
	}
	if err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, 60*time.Second, true, func(ctx context.Context) (bool, error) {
		live, err := kube.GetCRD(ctx, c.Kube.DynamicClient(), crd.Name)
		if err != nil {
			return false, nil
		}
		for _, cond := range live.Status.Conditions {
			if cond.Type == apiextensionsv1.Established && cond.Status == apiextensionsv1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		return fmt.Errorf("the CRD %s is not established: %w", crd.Name, err)
	}
	if r, ok := c.Kube.RESTMapper().(meta.ResettableRESTMapper); ok {
		r.Reset()
	}
	return nil
}

func (c KubeCluster) resource(obj *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	gvk := obj.GroupVersionKind()
	m, err := c.Kube.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("%s is not installed: %w", gvk.GroupKind(), err)
	}
	r := c.Kube.DynamicClient().Resource(m.Resource)
	if m.Scope.Name() == meta.RESTScopeNameNamespace {
		ns := obj.GetNamespace()
		if ns == "" {
			ns = "default"
		}
		return r.Namespace(ns), nil
	}
	return r, nil
}

func (c KubeCluster) Apply(ctx context.Context, objs []*unstructured.Unstructured) error {
	force := true
	for _, obj := range objs {
		if err := c.ensureNamespace(ctx, obj); err != nil {
			return err
		}
		ri, err := c.resource(obj)
		if err != nil {
			return err
		}
		body, err := json.Marshal(obj.Object)
		if err != nil {
			return err
		}
		if _, err := ri.Patch(ctx, obj.GetName(), k8stypes.ApplyPatchType, body,
			metav1.PatchOptions{FieldManager: fieldManager, Force: &force}); err != nil {
			return fmt.Errorf("apply %s %s: %w", obj.GetKind(), obj.GetName(), err)
		}
	}
	return nil
}

// ensureNamespace creates a namespaced object's namespace when it is missing:
// the rehearsal seeds objects into a cluster that may not have it yet.
func (c KubeCluster) ensureNamespace(ctx context.Context, obj *unstructured.Unstructured) error {
	gvk := obj.GroupVersionKind()
	m, err := c.Kube.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil || m.Scope.Name() != meta.RESTScopeNameNamespace || obj.GetNamespace() == "" {
		return nil // resource reports the mapping error
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: obj.GetNamespace()}}
	if _, err := c.Kube.Clientset().CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{FieldManager: fieldManager}); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create namespace %s: %w", obj.GetNamespace(), err)
	}
	return nil
}

func (c KubeCluster) Get(ctx context.Context, obj *unstructured.Unstructured) (*unstructured.Unstructured, error) {
	ri, err := c.resource(obj)
	if err != nil {
		return nil, err
	}
	u, err := ri.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if err != nil {
		if isNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return u, nil
}

func (c KubeCluster) Update(ctx context.Context, obj *unstructured.Unstructured) error {
	ri, err := c.resource(obj)
	if err != nil {
		return err
	}
	_, err = ri.Update(ctx, obj, metav1.UpdateOptions{FieldManager: fieldManager})
	return err
}
