// Package testutil holds what KOME's tests share: an API server (envtest)
// per test package, the CRDs the tests migrate, and a Cluster to seed, read
// and empty. Only tests import it.
//
// The API server has no controllers: no garbage collector, so a delete's
// propagation shows in what the API server itself does.
package testutil

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	"github.com/intentd/kome"
	"github.com/intentd/tools/kubeclient/kubefake/object"
)

var (
	OldGVK = schema.GroupVersionKind{Group: "old.io", Version: "v1", Kind: "Old"}
	NewGVK = schema.GroupVersionKind{Group: "new.io", Version: "v1", Kind: "New"}
	OldGVR = schema.GroupVersionResource{Group: "old.io", Version: "v1", Resource: "olds"}
	NewGVR = schema.GroupVersionResource{Group: "new.io", Version: "v1", Resource: "news"}
	CRDGVR = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	// ConfigMaps stand in for the objects a source owns.
	ConfigMapGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
)

var (
	config *rest.Config
	dyn    dynamic.Interface
	kube   kome.Kube
)

// Run starts the API server with crds installed, runs the package's tests,
// stops it, and returns the exit code: TestMain is
//
//	func TestMain(m *testing.M) { os.Exit(testutil.Run(m, testutil.OldCRD(nil))) }
func Run(m *testing.M, crds ...*unstructured.Unstructured) int {
	assets := os.Getenv("KUBEBUILDER_ASSETS")
	if assets == "" {
		fmt.Fprintln(os.Stderr, "KUBEBUILDER_ASSETS is not set: the tests need envtest's API server (make test sets it)")
		return 1
	}
	env := &envtest.Environment{BinaryAssetsDirectory: assets}
	for _, c := range crds {
		env.CRDs = append(env.CRDs, typed(c))
	}
	var err error
	if config, err = env.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "envtest:", err)
		return 1
	}
	defer env.Stop() //nolint:errcheck
	if dyn, err = dynamic.NewForConfig(config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if kube, err = kome.KubeForConfig(config, nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

// Config is the API server's rest.Config.
func Config() *rest.Config { return config }

// Kube is KOME's clients for the API server.
func Kube() kome.Kube { return kube }

// Dynamic is a dynamic client for the API server.
func Dynamic() dynamic.Interface { return dyn }

// CRD is a namespaced CRD for gvk with spec schema specSchema (nil: any
// fields) and a scale subresource on spec.replicas.
func CRD(gvk schema.GroupVersionKind, plural string, specSchema map[string]any) *unstructured.Unstructured {
	if specSchema == nil {
		specSchema = map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true}
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": plural + "." + gvk.Group},
		"spec": map[string]any{
			"group": gvk.Group, "scope": "Namespaced",
			"names": map[string]any{"kind": gvk.Kind, "plural": plural, "singular": plural[:len(plural)-1], "listKind": gvk.Kind + "List"},
			"versions": []any{map[string]any{
				"name": gvk.Version, "served": true, "storage": true,
				"schema": map[string]any{"openAPIV3Schema": map[string]any{
					"type": "object", "properties": map[string]any{
						"spec":   specSchema,
						"status": map[string]any{"type": "object", "x-kubernetes-preserve-unknown-fields": true},
					},
				}},
				"subresources": map[string]any{"scale": map[string]any{
					"specReplicasPath": ".spec.replicas", "statusReplicasPath": ".status.replicas",
				}},
			}},
		},
	}}
}

// OldCRD is the CRD of olds.old.io with spec schema specSchema (nil: any
// fields).
func OldCRD(specSchema map[string]any) *unstructured.Unstructured {
	return CRD(OldGVK, "olds", specSchema)
}

// NewCRD is the CRD of news.new.io, any fields.
func NewCRD() *unstructured.Unstructured { return CRD(NewGVK, "news", nil) }

func typed(u *unstructured.Unstructured) *apiextensionsv1.CustomResourceDefinition {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &crd); err != nil {
		panic(err)
	}
	return &crd
}

// OldObj is an Old in namespace team, labelled app: demo.
func OldObj(name string, spec map[string]any) *unstructured.Unstructured {
	return object.New(OldGVK.GroupVersion().String(), OldGVK.Kind, name, object.InNamespace("team"),
		object.WithSpec(spec), object.WithLabels(map[string]string{"app": "demo"}))
}

// Cluster is the API server as one test sees it: no Olds, News or ConfigMaps
// in team when the test starts, and none left when it ends.
type Cluster struct{ t *testing.T }

// NewCluster empties the cluster and creates objs, as the field manager
// "seed".
func NewCluster(t *testing.T, objs ...*unstructured.Unstructured) *Cluster {
	t.Helper()
	c := &Cluster{t: t}
	ns := object.New("v1", "Namespace", "team")
	_, err := dyn.Resource(schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}).Create(context.Background(), ns, metav1.CreateOptions{})
	if !apierrors.IsAlreadyExists(err) {
		require.NoError(t, err)
	}
	c.reset()
	t.Cleanup(c.reset)
	for _, o := range objs {
		c.Create(o)
	}
	return c
}

// gvrOf is the resource of each kind the tests use.
func gvrOf(u *unstructured.Unstructured) schema.GroupVersionResource {
	switch u.GetKind() {
	case OldGVK.Kind:
		return OldGVR
	case NewGVK.Kind:
		return NewGVR
	case "ConfigMap":
		return ConfigMapGVR
	}
	panic("no resource for " + u.GetKind())
}

// Create stores obj and returns it as the API server wrote it, uid and all.
func (c *Cluster) Create(obj *unstructured.Unstructured) *unstructured.Unstructured {
	c.t.Helper()
	obj = obj.DeepCopy()
	obj.SetResourceVersion("")
	obj.SetUID("")
	out, err := dyn.Resource(gvrOf(obj)).Namespace(obj.GetNamespace()).Create(context.Background(), obj, metav1.CreateOptions{FieldManager: "seed"})
	require.NoError(c.t, err)
	return out
}

// ApplyAs server-side applies obj as manager.
func (c *Cluster) ApplyAs(manager string, obj *unstructured.Unstructured) {
	c.t.Helper()
	_, err := dyn.Resource(gvrOf(obj)).Namespace(obj.GetNamespace()).Apply(context.Background(), obj.GetName(), obj, metav1.ApplyOptions{FieldManager: manager})
	require.NoError(c.t, err)
}

// ScaleAs sets an Old's spec.replicas through the scale subresource, as
// manager.
func (c *Cluster) ScaleAs(manager, ns, name string, replicas int64) {
	c.t.Helper()
	body := fmt.Sprintf(`{"apiVersion":"autoscaling/v1","kind":"Scale","metadata":{"name":%q,"namespace":%q},"spec":{"replicas":%d}}`, name, ns, replicas)
	_, err := dyn.Resource(OldGVR).Namespace(ns).Patch(context.Background(), name, k8stypes.ApplyPatchType, []byte(body),
		metav1.PatchOptions{FieldManager: manager}, "scale")
	require.NoError(c.t, err)
}

// Update writes obj as it is, as "seed".
func (c *Cluster) Update(obj *unstructured.Unstructured) {
	c.t.Helper()
	_, err := dyn.Resource(gvrOf(obj)).Namespace(obj.GetNamespace()).Update(context.Background(), obj, metav1.UpdateOptions{FieldManager: "seed"})
	require.NoError(c.t, err)
}

// Get is the object, or nil when there is none.
func (c *Cluster) Get(gvr schema.GroupVersionResource, ns, name string) *unstructured.Unstructured {
	c.t.Helper()
	u, err := dyn.Resource(gvr).Namespace(ns).Get(context.Background(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(c.t, err)
	return u
}

// Schema replaces Old's spec schema for the rest of the test.
func (c *Cluster) Schema(specSchema map[string]any) {
	c.t.Helper()
	c.setOldCRD(OldCRD(specSchema))
	c.t.Cleanup(func() { c.setOldCRD(OldCRD(nil)) })
}

func (c *Cluster) setOldCRD(want *unstructured.Unstructured) {
	c.t.Helper()
	ctx := context.Background()
	cur, err := dyn.Resource(CRDGVR).Get(ctx, want.GetName(), metav1.GetOptions{})
	require.NoError(c.t, err)
	cur.Object["spec"] = want.Object["spec"]
	_, err = dyn.Resource(CRDGVR).Update(ctx, cur, metav1.UpdateOptions{})
	require.NoError(c.t, err)
}

// reset deletes every Old and New, and the ConfigMaps in team, finalizers
// and all, and waits until they are gone. A kind not installed is skipped.
func (c *Cluster) reset() {
	c.t.Helper()
	ctx := context.Background()
	scopes := []struct {
		gvr schema.GroupVersionResource
		ns  string
	}{{OldGVR, ""}, {NewGVR, ""}, {ConfigMapGVR, "team"}}
	list := func(gvr schema.GroupVersionResource, ns string) ([]unstructured.Unstructured, error) {
		l, err := dyn.Resource(gvr).Namespace(ns).List(ctx, metav1.ListOptions{})
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		return l.Items, nil
	}
	for _, s := range scopes {
		items, err := list(s.gvr, s.ns)
		require.NoError(c.t, err)
		for _, it := range items {
			ri := dyn.Resource(s.gvr).Namespace(it.GetNamespace())
			if len(it.GetFinalizers()) > 0 {
				_, err := ri.Patch(ctx, it.GetName(), k8stypes.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`), metav1.PatchOptions{})
				if !apierrors.IsNotFound(err) {
					require.NoError(c.t, err)
				}
			}
			if err := ri.Delete(ctx, it.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				require.NoError(c.t, err)
			}
		}
	}
	require.NoError(c.t, wait.PollUntilContextTimeout(ctx, 50*time.Millisecond, 30*time.Second, true, func(context.Context) (bool, error) {
		for _, s := range scopes {
			items, err := list(s.gvr, s.ns)
			if err != nil || len(items) > 0 {
				return false, err
			}
		}
		return true, nil
	}), "emptying the cluster")
}

// RemoveCRDs deletes every CRD, the objects of each with it, when the test
// ends, and waits until they are gone: for tests that install their own.
func RemoveCRDs(t *testing.T) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		l, err := dyn.Resource(CRDGVR).List(ctx, metav1.ListOptions{})
		require.NoError(t, err)
		for _, crd := range l.Items {
			if err := dyn.Resource(CRDGVR).Delete(ctx, crd.GetName(), metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
				require.NoError(t, err)
			}
		}
		require.NoError(t, wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 60*time.Second, true, func(ctx context.Context) (bool, error) {
			l, err := dyn.Resource(CRDGVR).List(ctx, metav1.ListOptions{})
			return err == nil && len(l.Items) == 0, err
		}), "removing the CRDs")
	})
}

// Managers lists obj's field managers of the main resource as "operation
// manager": "Apply kome" for a server-side apply, "Update operator" for a
// create, update or patch.
func Managers(obj *unstructured.Unstructured) []string {
	var out []string
	for _, mf := range obj.GetManagedFields() {
		if mf.Subresource == "" {
			out = append(out, string(mf.Operation)+" "+mf.Manager)
		}
	}
	sort.Strings(out)
	return out
}
