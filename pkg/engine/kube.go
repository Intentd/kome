package engine

import (
	"context"
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
)

// Kube is what KOME needs from a cluster: client-go's standard clients. KOME
// reads and writes objects through the dynamic client, maps kinds to
// resources with the REST mapper, and asks discovery which kinds exist when
// it looks for a pruned source's children. ForConfig builds it.
type Kube struct {
	dynamic   dynamic.Interface
	mapper    meta.RESTMapper
	discovery discovery.DiscoveryInterface
}

// ForConfig builds a Kube from cfg. mapper is the caller's REST mapper (a
// controller's mgr.GetRESTMapper()); nil builds one that discovers each API
// group when first asked, and again when it meets a kind it does not know, so
// a CRD installed during a run is found.
func ForConfig(cfg *rest.Config, mapper meta.RESTMapper) (Kube, error) {
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return Kube{}, fmt.Errorf("dynamic client: %w", err)
	}
	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return Kube{}, fmt.Errorf("discovery client: %w", err)
	}
	if mapper == nil {
		httpClient, err := rest.HTTPClientFor(cfg)
		if err != nil {
			return Kube{}, fmt.Errorf("http client: %w", err)
		}
		if mapper, err = apiutil.NewDynamicRESTMapper(cfg, httpClient); err != nil {
			return Kube{}, fmt.Errorf("REST mapper: %w", err)
		}
	}
	return Kube{dynamic: dyn, mapper: mapper, discovery: disco}, nil
}

// Check reports a Kube not built by ForConfig.
func (k Kube) Check() error {
	if k.dynamic == nil || k.mapper == nil || k.discovery == nil {
		return errors.New("kube: not set; build it with ForConfig")
	}
	return nil
}

// Dynamic is the dynamic client.
func (k Kube) Dynamic() dynamic.Interface { return k.dynamic }

// Discovery is the discovery client.
func (k Kube) Discovery() discovery.DiscoveryInterface { return k.discovery }

// Mapping is the REST mapping of gvk.
func (k Kube) Mapping(gvk schema.GroupVersionKind) (*meta.RESTMapping, error) {
	return k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
}

// Resource is gvk's resource, and whether it is cluster-scoped.
func (k Kube) Resource(gvk schema.GroupVersionKind) (dynamic.NamespaceableResourceInterface, bool, error) {
	m, err := k.Mapping(gvk)
	if err != nil {
		return nil, false, err
	}
	return k.dynamic.Resource(m.Resource), m.Scope.Name() == meta.RESTScopeNameRoot, nil
}

// ResourceIn is gvk's resource in namespace; namespace is ignored for a
// cluster-scoped kind.
func (k Kube) ResourceIn(gvk schema.GroupVersionKind, namespace string) (dynamic.ResourceInterface, error) {
	r, cluster, err := k.Resource(gvk)
	if err != nil {
		return nil, err
	}
	if cluster {
		return r, nil
	}
	return r.Namespace(namespace), nil
}

// ResourceFor is obj's resource at its scope.
func (k Kube) ResourceFor(obj *unstructured.Unstructured) (dynamic.ResourceInterface, error) {
	return k.ResourceIn(obj.GroupVersionKind(), obj.GetNamespace())
}

// Delete deletes obj.
func (k Kube) Delete(ctx context.Context, obj *unstructured.Unstructured, opts metav1.DeleteOptions) error {
	r, err := k.ResourceFor(obj)
	if err != nil {
		return err
	}
	return r.Delete(ctx, obj.GetName(), opts)
}
