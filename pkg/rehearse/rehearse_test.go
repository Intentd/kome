package rehearse_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/intentd/kome"
	"github.com/intentd/kome/pkg/declaration"
	"github.com/intentd/kome/pkg/rehearse"
	"github.com/intentd/kome/testutil"
	"github.com/intentd/tools/kubeclient"
)

func TestMain(m *testing.M) {
	os.Exit(testutil.Run(m))
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
	ctx := context.Background()
	testutil.RemoveCRDs(t)
	kc, err := kubeclient.NewFromConfig(ctx, testutil.Config(), nil)
	require.NoError(t, err)
	r := &rehearse.Rehearsal{File: f, Engine: &kome.Engine{Kube: testutil.Kube()}, Cluster: rehearse.KubeCluster{Kube: kc}}
	return r.Run(ctx, ms)
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
spec: {image: nginx, replicas: 2, extra: one}
---
apiVersion: config.example.com/v1
kind: AppConfig
metadata: {name: b, namespace: default}
spec: {image: busybox, replicas: 1, extra: two}
---
apiVersion: config.example.com/v1
kind: AppConfig
metadata: {name: c, namespace: default}
spec: {image: redis, replicas: 1, extra: three}
`,
		"dst.yaml": `
apiVersion: config.example.com/v1
kind: NewConfig
metadata: {name: c, namespace: default}
spec: {image: redis, replicas: 1, extra: three}
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
spec: {image: a, replicas: 1, extra: one}
---
apiVersion: config.example.com/v1
kind: AppConfig
metadata: {name: payments, namespace: team-c}
spec: {image: c, replicas: 2, extra: two}
---
apiVersion: config.example.com/v1
kind: AppConfig
metadata: {name: billing, namespace: team-b}
spec: {image: b, replicas: 1, extra: three}
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
