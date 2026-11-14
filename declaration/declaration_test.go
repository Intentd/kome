package declaration_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/intentd/kome/declaration"
)

// load writes body as a KOME file beside testdata/schemas and loads it.
func load(t *testing.T, body string) *declaration.File {
	t.Helper()
	dir := t.TempDir()
	for _, sub := range []string{"schemas", "typed", "objects"} {
		abs, err := filepath.Abs(filepath.Join("testdata", sub))
		require.NoError(t, err)
		require.NoError(t, os.Symlink(abs, filepath.Join(dir, sub)))
	}
	path := filepath.Join(dir, "kome.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	f, err := declaration.Load(path)
	require.NoError(t, err)
	return f
}

// messages returns the findings as strings, errors and warnings apart.
func messages(r declaration.Report) (errs, warns string) {
	var e, w []string
	for _, f := range r.Errors() {
		e = append(e, f.String())
	}
	for _, f := range r.Warnings() {
		w = append(w, f.String())
	}
	return strings.Join(e, "\n"), strings.Join(w, "\n")
}

func TestLoad_FieldAndKind(t *testing.T) {
	f := load(t, `
migrations:
  cronjob:
    from: {schema: ./schemas/cronjob-old.yaml}
    to: {schema: ./schemas/cronjob.yaml}
    spec:
      replicas: {to: scaling.min}
      schedule:
        values:
          schedule.minute: '{{ cronMinute .value }}'
      timeout: {from: timeoutSeconds, to: timeout, value: '{{ printf "%ds" .value }}'}
  appconfig:
    from: {schema: ./schemas/appconfig.yaml}
    to:
      schema: ./schemas/newconfig.yaml
      copyStatus: true
      metadata:
        labels: {migrated-from: '{{ .metadata.name }}'}
    selection:
      namespaces: [default]
      selector: tier=web
      when:
        - field: status.phase
          equals: Ready
    lifecycle:
      prune: true
      children: reparent
      finalizers: [example.io/cleanup]
`)
	assert.Equal(t, []string{"appconfig", "cronjob"}, f.Names())
	cron, app := f.Migrations["cronjob"], f.Migrations["appconfig"]
	assert.Equal(t, declaration.LevelField, cron.Level())
	assert.Equal(t, declaration.LevelKind, app.Level())

	assert.Equal(t, "replicas", cron.Spec["replicas"].SourcePath("replicas"))
	assert.Equal(t, []string{"scaling.min"}, cron.Spec["replicas"].Destinations("replicas"))
	assert.Equal(t, "timeoutSeconds", cron.Spec["timeout"].SourcePath("timeout"))
	assert.Equal(t, []string{"schedule.minute"}, cron.Spec["schedule"].Destinations("schedule"))
	assert.True(t, app.To.Metadata.Preserve(), "preserveSource defaults to true")
	assert.Equal(t, "Ready", app.Selection.When[0].Equals)

	to, err := f.Identify(app.To.Shape)
	require.NoError(t, err)
	assert.Equal(t, "NewConfig", to.GVK.Kind)
	assert.Equal(t, "newconfigs", to.GVR().Resource)
	assert.True(t, to.Namespaced)

	errs, warns := messages(f.Validate())
	assert.Empty(t, errs)
	assert.Contains(t, warns, "appconfig.lifecycle.prune: deletes each source")
}

func TestLoad_UnknownKeys(t *testing.T) {
	_, err := declaration.Parse([]byte("migrations:\n  a:\n    from: {schema: x}\n    to: {schema: y}\n    prnue: true\n"))
	assert.ErrorContains(t, err, "prnue")
}

func TestLoad_DeclaredIdentity(t *testing.T) {
	f := load(t, `
migrations:
  rename:
    from: {apiVersion: config.example.com/v1, kind: AppConfig}
    to: {apiVersion: config.example.com/v1, kind: NewConfig, plural: newconfigs}
`)
	errs, warns := messages(f.Validate())
	assert.Empty(t, errs)
	assert.Contains(t, warns, `rename.from.plural: inferred as "appconfigs"`)
	assert.NotContains(t, warns, "to.plural")
}

func TestValidate_LevelAndKeys(t *testing.T) {
	f := load(t, `
migrations:
  fields-across-kinds:
    from: {schema: ./schemas/cronjob-old.yaml}
    to: {schema: ./schemas/newconfig.yaml}
    spec: {replicas: {to: scaling.min}}
  conversion:
    from: {schema: ./schemas/cronjob-old.yaml}
    to: {schema: ./schemas/cronjob.yaml}
  field-with-kind-keys:
    from: {schema: ./schemas/cronjob-old.yaml}
    to: {schema: ./schemas/cronjob.yaml, copyStatus: true, namespace: x}
    spec: {replicas: {to: scaling.min}}
    lifecycle: {prune: true}
  kind-with-transitional:
    from: {schema: ./schemas/appconfig.yaml}
    transitional: {schema: ./schemas/appconfig.yaml}
    to: {schema: ./schemas/newconfig.yaml}
  missing-to:
    from: {schema: ./schemas/appconfig.yaml}
    to: {}
  not-a-crd:
    from: {schema: ./kome.yaml}
    to: {schema: ./schemas/newconfig.yaml}
  Bad_Name:
    from: {schema: ./schemas/appconfig.yaml}
    to: {schema: ./schemas/newconfig.yaml}
`)
	errs, _ := messages(f.Validate())
	for _, want := range []string{
		"fields-across-kinds.to: spec moves need the same kind (CronJob → NewConfig): move the kind first",
		"conversion.spec: to is the same kind with no spec moves",
		"field-with-kind-keys.to.copyStatus: applies to kind moves",
		"field-with-kind-keys.to.namespace: applies to kind moves",
		"field-with-kind-keys.lifecycle: applies to kind moves",
		"kind-with-transitional.transitional: applies to field moves",
		"missing-to.to: needs a schema, or apiVersion and kind",
		"not-a-crd.from: schema ./kome.yaml: expected kind: CustomResourceDefinition",
		`Bad_Name: name "Bad_Name"`,
	} {
		assert.Contains(t, errs, want)
	}
}

func TestValidate_Scopes(t *testing.T) {
	f := load(t, `
migrations:
  to-namespaced:
    from: {schema: ./schemas/globalconfig.yaml}
    to: {schema: ./schemas/newconfig.yaml}
  to-cluster:
    from: {schema: ./schemas/appconfig.yaml}
    to: {schema: ./schemas/globalconfig.yaml, namespace: default}
  same-scope:
    from: {schema: ./schemas/appconfig.yaml}
    to: {schema: ./schemas/newconfig.yaml, namespace: default}
`)
	errs, warns := messages(f.Validate())
	assert.Contains(t, errs, "to-namespaced.to.namespace: required: GlobalConfig is cluster-scoped and NewConfig is namespaced")
	assert.Contains(t, warns, "to-cluster.to: GlobalConfig is cluster-scoped: namespaces are dropped")
	assert.Contains(t, warns, "to-cluster.to.namespace: ignored: the destination is cluster-scoped")
	assert.Contains(t, warns, "same-scope.to.namespace: ignored")
}

func TestValidate_FieldsAndSelection(t *testing.T) {
	f := load(t, `
migrations:
  cronjob:
    from: {schema: ./schemas/cronjob-old.yaml}
    to: {schema: ./schemas/cronjob.yaml}
    spec:
      schedule: {to: when}
      scheduleCopy: {from: schedule, to: when2}
      resources: {to: res}
      resourcesCpu: {from: resources.cpu, to: cpu}
      fan: {to: x, values: {a.b: '{{ .value }}'}}
    selection:
      selector: "tier in (web"
      name: Bad_Name
      namespaces: [Bad_NS]
      when:
        - equals: x
        - field: spec.x
          min: "1"
          max: "2"
`)
	errs, warns := messages(f.Validate())
	for _, want := range []string{
		`cronjob.spec.scheduleCopy: reads "schedule", which overlaps "schedule" read by spec.schedule`,
		`cronjob.spec.resourcesCpu: reads "resources.cpu", which overlaps "resources" read by spec.resources`,
		"cronjob.selection.selector:",
		"cronjob.selection.name:",
		`cronjob.selection.namespaces: "Bad_NS"`,
		"cronjob.selection.when[0]: field is required",
		"cronjob.selection.when[1]: declares min and max",
	} {
		assert.Contains(t, errs, want)
	}
	assert.Contains(t, warns, "cronjob.spec.fan: values is set, so to and value are ignored")
}

func TestValidate_Empty(t *testing.T) {
	f := load(t, "migrations: {}\n")
	assert.ErrorContains(t, f.Validate().Err(), "no migrations")
}

func TestSet(t *testing.T) {
	f := load(t, `
migrations:
  appconfig:
    from: {schema: ./schemas/appconfig.yaml}
    to: {schema: ./schemas/newconfig.yaml}
`)
	assert.NoError(t, f.Set("appconfig", "lifecycle.prune=true"))
	assert.NoError(t, f.Set("appconfig", "selection.namespaces=[default, staging]"))
	assert.NoError(t, f.Set("appconfig", "to.copyStatus=true"))
	m := f.Migrations["appconfig"]
	assert.True(t, m.Lifecycle.Prune)
	assert.True(t, m.To.CopyStatus)
	assert.Equal(t, []string{"default", "staging"}, m.Selection.Namespaces)

	assert.ErrorContains(t, f.Set("appconfig", "prnue=true"), "prnue")
	assert.ErrorContains(t, f.Set("appconfig", "lifecycle.prune=maybe"), "lifecycle.prune")
	assert.ErrorContains(t, f.Set("nope", "lifecycle.prune=true"), `no migration "nope"`)
	assert.ErrorContains(t, f.Set("appconfig", "lifecycle.prune"), "want key=value")
}

func TestValidate_Lifecycle(t *testing.T) {
	f := load(t, `
migrations:
  no-prune:
    from: {schema: ./schemas/appconfig.yaml}
    to: {schema: ./schemas/newconfig.yaml}
    lifecycle: {children: reparent, finalizers: [example.io/cleanup]}
  bad-policy:
    from: {schema: ./schemas/appconfig.yaml}
    to: {schema: ./schemas/newconfig.yaml}
    lifecycle: {prune: true, children: adopt}
  system-finalizers:
    from: {schema: ./schemas/appconfig.yaml}
    to: {schema: ./schemas/newconfig.yaml}
    lifecycle:
      prune: true
      finalizers: [kubernetes.io/pvc-protection, foregroundDeletion, orphan, example.io/x, example.io/x, "not a name"]
  fine:
    from: {schema: ./schemas/appconfig.yaml}
    to: {schema: ./schemas/newconfig.yaml}
    lifecycle: {prune: true, children: orphan, finalizers: [example.io/cleanup]}
`)
	errs, warns := messages(f.Validate())
	for _, want := range []string{
		"no-prune.lifecycle.children: applies when pruning; set lifecycle.prune",
		"no-prune.lifecycle.finalizers: applies when pruning; set lifecycle.prune",
		`bad-policy.lifecycle.children: "adopt": one of reparent, orphan, delete`,
		`system-finalizers.lifecycle.finalizers: "kubernetes.io/pvc-protection" belongs to Kubernetes and is never removed`,
		`system-finalizers.lifecycle.finalizers: "foregroundDeletion" belongs to Kubernetes`,
		`system-finalizers.lifecycle.finalizers: "orphan" belongs to Kubernetes`,
		`system-finalizers.lifecycle.finalizers: "example.io/x" is listed twice`,
		`system-finalizers.lifecycle.finalizers: "not a name"`,
	} {
		assert.Contains(t, errs, want)
	}
	assert.NotContains(t, errs, "fine.", "a valid lifecycle has no errors")
	assert.Contains(t, warns, "fine.lifecycle.prune: deletes each source once its destination holds every source field; dry-run lists the children and finalizers")
}

func TestValidate_OverlappingWrites(t *testing.T) {
	f := load(t, `
migrations:
  cronjob:
    from: {schema: ./schemas/cronjob-old.yaml}
    to: {schema: ./schemas/cronjob.yaml}
    spec:
      schedule: {to: timeout}
      timeout: {from: timeoutSeconds, to: timeout}
      a: {to: scaling}
      b: {to: scaling.min}
`)
	errs, _ := messages(f.Validate())
	assert.Contains(t, errs, `cronjob.spec.timeout: writes "timeout", which overlaps "timeout" written by spec.schedule`)
	assert.Contains(t, errs, `cronjob.spec.b: writes "scaling.min", which overlaps "scaling" written by spec.a`)
}

func TestValidate_FieldManager(t *testing.T) {
	f := load(t, `
migrations:
  too-long:
    from: {schema: ./schemas/appconfig.yaml}
    to: {schema: ./schemas/newconfig.yaml}
    fieldManager: `+strings.Repeat("m", 129)+`
  unprintable:
    from: {schema: ./schemas/appconfig.yaml}
    to: {schema: ./schemas/newconfig.yaml}
    fieldManager: "operator\u0007"
  fine:
    from: {schema: ./schemas/appconfig.yaml}
    to: {schema: ./schemas/newconfig.yaml}
    fieldManager: appconfig-operator
`)
	errs, _ := messages(f.Validate())
	assert.Contains(t, errs, "too-long.fieldManager:")
	assert.Contains(t, errs, "unprintable.fieldManager:")
	assert.NotContains(t, errs, "fine.fieldManager")
}

func TestValidate_TemplatesAndCEL(t *testing.T) {
	f := load(t, `
migrations:
  cronjob:
    from: {schema: ./schemas/cronjob-old.yaml}
    to: {schema: ./schemas/cronjob.yaml}
    spec:
      replicas: {to: scaling.min, value: '${ value * 2 }'}
      timeout: {from: timeoutSeconds, to: timeout, value: '{{ printf "%ds" .value }}'}
      image: {value: 'nginx'}
      legacy: {value: '{{ conditionsState .value "x" }}'}
      schedule: {value: '${ conditionsState(value, "x") }'}
  appconfig:
    from: {schema: ./schemas/appconfig.yaml}
    to:
      schema: ./schemas/newconfig.yaml
      metadata:
        labels: {from: '{{ name . }}', bad: '{{ name }}'}
`)
	errs, _ := messages(f.Validate())
	for _, want := range []string{
		"cronjob.spec.legacy.value:",
		"conditionsState takes 1 argument, got 2",
		"cronjob.spec.schedule.value:",
		"appconfig.to.metadata.labels.bad:",
		"name takes 1 argument, got 0",
	} {
		assert.Contains(t, errs, want)
	}
	for _, fine := range []string{"spec.replicas", "spec.timeout:", "spec.image", "labels.from"} {
		assert.NotContains(t, errs, fine)
	}
}

func TestOrderAndNeeds(t *testing.T) {
	f := load(t, `
migrations:
  c: {from: {schema: ./schemas/appconfig.yaml}, to: {schema: ./schemas/newconfig.yaml}, needs: [b]}
  a: {from: {schema: ./schemas/appconfig.yaml}, to: {schema: ./schemas/newconfig.yaml}, needs: [c]}
  b: {from: {schema: ./schemas/appconfig.yaml}, to: {schema: ./schemas/newconfig.yaml}}
  z: {from: {schema: ./schemas/appconfig.yaml}, to: {schema: ./schemas/newconfig.yaml}}
`)
	order, err := f.Order()
	require.NoError(t, err)
	assert.Equal(t, []string{"b", "c", "a", "z"}, order, "each after what it needs; names otherwise")

	f = load(t, `
migrations:
  a: {from: {schema: ./schemas/appconfig.yaml}, to: {schema: ./schemas/newconfig.yaml}, needs: [b]}
  b: {from: {schema: ./schemas/appconfig.yaml}, to: {schema: ./schemas/newconfig.yaml}, needs: [a]}
  self: {from: {schema: ./schemas/appconfig.yaml}, to: {schema: ./schemas/newconfig.yaml}, needs: [self]}
  lost: {from: {schema: ./schemas/appconfig.yaml}, to: {schema: ./schemas/newconfig.yaml}, needs: [nowhere]}
`)
	errs, _ := messages(f.Validate())
	assert.Contains(t, errs, "needs form a cycle: a → b → a")
	assert.Contains(t, errs, "self.needs: a migration can't need itself")
	assert.Contains(t, errs, `lost.needs: "nowhere" is not a migration in the file`)
}
