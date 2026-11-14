package rehearse_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/intentd/kome"
	"github.com/intentd/kome/declaration"
	"github.com/intentd/kome/rehearse"
	"github.com/intentd/tools/kube"
	"github.com/intentd/tools/kubeclient"
	"github.com/intentd/tools/kubeclient/kubefake"
	"github.com/intentd/tools/kubeclient/kubefake/object"
)

// fakeCluster is a kubefake client whose mapper learns each CRD ApplyCRD
// installs, with server-side apply and delete preconditions.
type fakeCluster struct {
	*kubefake.Kubeclient
	dyn    *kubefake.Dynamic
	mapper *meta.DefaultRESTMapper
}

var lists = map[schema.GroupVersionResource]string{
	kube.CRDResource: "CustomResourceDefinitionList",
	{Group: "batch.example.com", Version: "v1", Resource: "cronjobs"}:       "CronJobList",
	{Group: "config.example.com", Version: "v1", Resource: "appconfigs"}:    "AppConfigList",
	{Group: "config.example.com", Version: "v1", Resource: "newconfigs"}:    "NewConfigList",
	{Group: "config.example.com", Version: "v1", Resource: "globalconfigs"}: "GlobalConfigList",
}

func newFakeCluster() *fakeCluster {
	dyn := kubefake.NewDynamic(runtime.NewScheme(), lists)
	f := &fakeCluster{Kubeclient: kubefake.New(runtime.NewScheme()), dyn: dyn, mapper: meta.NewDefaultRESTMapper(nil)}
	tracker := dyn.Tracker()
	dyn.PrependReactor("patch", "*", func(a kubefake.Action) (bool, runtime.Object, error) {
		pa := a.(kubefake.PatchActionImpl)
		if pa.GetPatchType() != k8stypes.ApplyPatchType {
			if len(pa.GetPatchOptions().DryRun) > 0 {
				cur, err := tracker.Get(pa.GetResource(), pa.GetNamespace(), pa.GetName())
				return true, cur, err
			}
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
		if cur, err := tracker.Get(gvr, pa.GetNamespace(), pa.GetName()); err == nil {
			u.SetResourceVersion(cur.(metav1.Object).GetResourceVersion())
			return true, u, tracker.Update(gvr, u, pa.GetNamespace())
		}
		return true, u, tracker.Create(gvr, u, pa.GetNamespace())
	})
	dyn.PrependReactor("delete", "*", func(a kubefake.Action) (bool, runtime.Object, error) {
		da := a.(kubefake.DeleteActionImpl)
		opts := da.GetDeleteOptions()
		if p := opts.Preconditions; p != nil && p.ResourceVersion != nil {
			cur, err := tracker.Get(da.GetResource(), da.GetNamespace(), da.GetName())
			if err == nil && cur.(metav1.Object).GetResourceVersion() != *p.ResourceVersion {
				return true, nil, apierrors.NewConflict(da.GetResource().GroupResource(), da.GetName(), fmt.Errorf("changed"))
			}
		}
		return len(opts.DryRun) > 0, nil, nil
	})
	dyn.PrependReactor("create", "*", func(a kubefake.Action) (bool, runtime.Object, error) {
		if len(a.(kubefake.CreateActionImpl).GetCreateOptions().DryRun) > 0 {
			return true, a.(kubefake.CreateActionImpl).GetObject(), nil
		}
		return false, nil, nil
	})
	return f
}

func (f *fakeCluster) DynamicClient() dynamic.Interface { return f.dyn }
func (f *fakeCluster) RESTMapper() meta.RESTMapper      { return f.mapper }

func (f *fakeCluster) Delete(ctx context.Context, obj kubeclient.Object, opts metav1.DeleteOptions) error {
	u := obj.(*unstructured.Unstructured)
	m, err := f.mapper.RESTMapping(u.GroupVersionKind().GroupKind(), u.GroupVersionKind().Version)
	if err != nil {
		return err
	}
	return f.dyn.Resource(m.Resource).Namespace(u.GetNamespace()).Delete(ctx, u.GetName(), opts)
}

// ApplyCRD stores the CRD, marked established, and teaches the mapper.
func (f *fakeCluster) ApplyCRD(ctx context.Context, manifest []byte) error {
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(manifest, &crd); err != nil {
		return err
	}
	scope := meta.RESTScopeNamespace
	if crd.Spec.Scope == apiextensionsv1.ClusterScoped {
		scope = meta.RESTScopeRoot
	}
	v := kube.StorageVersion(&crd)
	f.mapper.Add(schema.GroupVersionKind{Group: crd.Spec.Group, Version: v.Name, Kind: crd.Spec.Names.Kind}, scope)
	u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&crd)
	if err != nil {
		return err
	}
	obj := &unstructured.Unstructured{Object: u}
	obj.SetAPIVersion("apiextensions.k8s.io/v1")
	obj.SetKind("CustomResourceDefinition")
	tr := f.dyn.Tracker()
	if _, err := tr.Get(kube.CRDResource, "", crd.Name); err == nil {
		return tr.Update(kube.CRDResource, obj, "")
	}
	return tr.Create(kube.CRDResource, obj, "")
}

func (f *fakeCluster) cluster() rehearse.Cluster {
	return testCluster{KubeCluster: rehearse.KubeCluster{Kube: f}, f: f}
}

// testCluster is KubeCluster with the fake's ApplyCRD.
type testCluster struct {
	rehearse.KubeCluster
	f *fakeCluster
}

func (c testCluster) ApplyCRD(ctx context.Context, manifest []byte) error {
	return c.f.ApplyCRD(ctx, manifest)
}

// fileWith writes a KOME file and its objects next to the typed schemas.
func fileWith(t *testing.T, body string, objects map[string]string) *declaration.File {
	t.Helper()
	dir := t.TempDir()
	abs, err := filepath.Abs("../declaration/testdata/typed")
	require.NoError(t, err)
	require.NoError(t, os.Symlink(abs, filepath.Join(dir, "typed")))
	for name, content := range objects {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	}
	path := filepath.Join(dir, "kome.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	f, err := declaration.Load(path)
	require.NoError(t, err)
	return f
}

func rehearsal(t *testing.T, f *declaration.File) (rehearse.Report, error) {
	t.Helper()
	ms, err := kome.FromFile(f)
	require.NoError(t, err)
	fc := newFakeCluster()
	r := &rehearse.Rehearsal{File: f, Engine: &kome.Engine{Kube: fc}, Cluster: fc.cluster()}
	return r.Run(context.Background(), ms)
}

func names(steps []rehearse.Step) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.String())
	}
	return out
}

func TestRehearse_Field(t *testing.T) {
	f := fileWith(t, `
migrations:
  cronjob:
    from:
      schema: ./typed/cronjob-old.yaml
      objects: [./old.yaml]
    to: {schema: ./typed/cronjob.yaml}
    spec:
      replicas: {to: scaling.min}
      timeout: {from: timeoutSeconds, to: timeout, value: '{{ printf "%vs" .value }}'}
`, map[string]string{"old.yaml": `
apiVersion: batch.example.com/v1
kind: CronJob
metadata: {name: a, namespace: default}
spec: {replicas: 2, timeoutSeconds: 30, image: nginx}
---
apiVersion: batch.example.com/v1
kind: CronJob
metadata: {name: b, namespace: default}
spec: {replicas: 3, timeoutSeconds: 60, image: busybox}
`})
	rep, err := rehearsal(t, f)
	steps := rep.Steps
	require.NoError(t, err, "%v", names(steps))
	require.Len(t, steps, 9, "%v", names(steps))
	for _, s := range steps {
		assert.True(t, s.Passed, s.String())
	}
}

func TestRehearse_Kind(t *testing.T) {
	f := fileWith(t, `
migrations:
  appconfig:
    from:
      schema: ./typed/appconfig.yaml
      objects: [./src.yaml]
    to:
      schema: ./typed/appconfig-new.yaml
      objects: [./dst.yaml]
`, map[string]string{
		"src.yaml": `
apiVersion: config.example.com/v1
kind: AppConfig
metadata: {name: a, namespace: default}
spec: {image: nginx, replicas: 2, extra: x}
---
apiVersion: config.example.com/v1
kind: AppConfig
metadata: {name: b, namespace: default}
spec: {image: busybox, replicas: 1, extra: y}
---
apiVersion: config.example.com/v1
kind: AppConfig
metadata: {name: c, namespace: default}
spec: {image: redis, replicas: 1, extra: z}
`,
		"dst.yaml": `
apiVersion: config.example.com/v1
kind: NewConfig
metadata: {name: c, namespace: default}
spec: {image: redis, replicas: 1, extra: z}
`,
	})
	rep, err := rehearsal(t, f)
	steps := rep.Steps
	require.NoError(t, err, "%v", names(steps))
	require.Len(t, steps, 7, "%v", names(steps))
	for _, s := range steps {
		assert.True(t, s.Passed, s.String())
	}
}

// Two namespaced sources with one name collide once cluster-scoped: the
// rehearsal reports it as a known issue and rehearses the rest.
func TestRehearse_KnownIssuesDoNotFail(t *testing.T) {
	f := fileWith(t, `
migrations:
  rescope:
    from:
      schema: ./typed/appconfig.yaml
      objects: [./src.yaml]
    to: {schema: ./typed/appconfig-global.yaml}
`, map[string]string{"src.yaml": `
apiVersion: config.example.com/v1
kind: AppConfig
metadata: {name: payments, namespace: team-a}
spec: {image: a, replicas: 1, extra: x}
---
apiVersion: config.example.com/v1
kind: AppConfig
metadata: {name: payments, namespace: team-c}
spec: {image: c, replicas: 2, extra: y}
---
apiVersion: config.example.com/v1
kind: AppConfig
metadata: {name: billing, namespace: team-b}
spec: {image: b, replicas: 1, extra: z}
`})
	rep, err := rehearsal(t, f)
	require.NoError(t, err, "%v", names(rep.Steps))
	assert.True(t, rep.Passed())
	require.Len(t, rep.Steps, 7, "%v", names(rep.Steps))
	require.Len(t, rep.Issues, 1, "the collision is reported once, not at every step")
	assert.Equal(t, "team-c/payments", rep.Issues[0].Object)
	assert.Contains(t, rep.Issues[0].Message, "also the target of team-a/payments")
	assert.Equal(t, 2, rep.Issues[0].Step, "found by the dry run")
	assert.Equal(t, 1, rep.Steps[1].Issues)
	assert.Contains(t, rep.Steps[1].String(), "⚠ 1 known issue")

	var buf bytes.Buffer
	rehearse.WriteSummary(&buf, rep)
	assert.Contains(t, buf.String(), "known issues (1)")
	assert.Contains(t, buf.String(), "⚠ rescope step 2: team-c/payments:")
}

// An object with no room left for its record is a known issue, found by the
// dry run; the rest rehearse.
func TestRehearse_RecordTooLargeIsKnownIssue(t *testing.T) {
	f := fileWith(t, `
migrations:
  cronjob:
    from:
      schema: ./typed/cronjob-old.yaml
      objects: [./old.yaml]
    to: {schema: ./typed/cronjob.yaml}
    spec:
      replicas: {to: scaling.min}
`, map[string]string{"old.yaml": `
apiVersion: batch.example.com/v1
kind: CronJob
metadata: {name: a, namespace: default}
spec: {replicas: 2, image: nginx}
---
apiVersion: batch.example.com/v1
kind: CronJob
metadata:
  name: big
  namespace: default
  annotations: {kubectl.kubernetes.io/last-applied-configuration: ` + strings.Repeat("x", 256*1024-100) + `}
spec: {replicas: 3, image: busybox}
`})
	rep, err := rehearsal(t, f)
	require.NoError(t, err, "%v", names(rep.Steps))
	assert.True(t, rep.Passed())
	require.Len(t, rep.Issues, 1, "%v", rep.Issues)
	assert.Equal(t, "default/big", rep.Issues[0].Object)
	assert.Equal(t, 4, rep.Issues[0].Step, "found by the dry run")
	assert.Contains(t, rep.Issues[0].Message, "the record is too large")
}

func TestRehearse_FailingStepIsNamed(t *testing.T) {
	f := fileWith(t, `
migrations:
  cronjob:
    from:
      schema: ./typed/cronjob-old.yaml
      objects: [./missing.yaml]
    to: {schema: ./typed/cronjob.yaml}
    spec:
      replicas: {to: scaling.min}
`, nil)
	rep, err := rehearsal(t, f)
	assert.ErrorContains(t, err, "cronjob step 1")
	require.Len(t, rep.Steps, 1)
	assert.False(t, rep.Steps[0].Passed)
}
