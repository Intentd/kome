package declaration_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSchemas_FieldPaths(t *testing.T) {
	f := load(t, `
migrations:
  cronjob:
    from: {schema: ./typed/cronjob-old.yaml}
    to: {schema: ./typed/cronjob.yaml}
    spec:
      replicas: {to: scaling.min}
      timeout: {from: timeoutSeconds, to: timeout, value: '{{ printf "%ds" .value }}'}
      missing: {to: scaling.max}
`)
	errs, _ := messages(f.Validate())
	assert.Contains(t, errs, "cronjob.spec.missing: from path spec.missing is not in ./typed/cronjob-old.yaml")
	assert.Contains(t, errs, "cronjob.spec.missing: to path spec.scaling.max is not in ./typed/cronjob.yaml")
	assert.Contains(t, errs, "cronjob.spec: ./typed/cronjob.yaml drops fields no move covers")
	assert.Contains(t, errs, "spec.legacy: missing in destination")
	assert.Contains(t, errs, "spec.schedule: missing in destination")
	assert.NotContains(t, errs, "spec.replicas: missing", "a covered path is not reported")
	assert.NotContains(t, errs, "spec.image", "a path both schemas hold is fine")
}

func TestSchemas_KindCompare(t *testing.T) {
	f := load(t, `
migrations:
  appconfig:
    from: {schema: ./typed/appconfig.yaml}
    to: {schema: ./typed/newconfig.yaml}
`)
	errs, _ := messages(f.Validate())
	assert.Contains(t, errs, "appconfig.to: NewConfig cannot hold every source field")
	assert.Contains(t, errs, "spec.extra: missing in destination")
	assert.Contains(t, errs, "spec.replicas: ")
	assert.NotContains(t, errs, "spec.image")
}

func TestSchemas_PreserveUnknownHoldsAnything(t *testing.T) {
	f := load(t, `
migrations:
  appconfig:
    from: {schema: ./typed/appconfig.yaml}
    to: {schema: ./schemas/newconfig.yaml}
`)
	errs, _ := messages(f.Validate())
	assert.Empty(t, errs)
}

func TestSchemas_Transitional(t *testing.T) {
	f := load(t, `
migrations:
  ok:
    from: {schema: ./typed/cronjob-old.yaml}
    transitional: {schema: ./typed/cronjob-transitional.yaml}
    to: {schema: ./schemas/cronjob.yaml}
    spec:
      replicas: {to: scaling.min}
  wrong:
    from: {schema: ./typed/cronjob-old.yaml}
    transitional: {schema: ./typed/cronjob-transitional.yaml}
    to: {schema: ./typed/cronjob.yaml}
    spec:
      timeout: {from: timeoutSeconds, to: timeout}
  version:
    from: {schema: ./schemas/cronjob-old.yaml}
    transitional: {schema: ./typed/cronjob-transitional-v2.yaml}
    to: {schema: ./schemas/cronjob.yaml}
    spec:
      replicas: {to: scaling.min}
  empty:
    from: {schema: ./schemas/cronjob-old.yaml}
    transitional: {}
    to: {schema: ./schemas/cronjob.yaml}
    spec:
      replicas: {to: scaling.min}
`)
	errs, _ := messages(f.Validate())
	assert.NotContains(t, errs, "ok.transitional")
	assert.Contains(t, errs, "wrong.transitional: spec.timeoutSeconds is string in ./typed/cronjob-transitional.yaml but integer in the from schema")
	assert.Contains(t, errs, "wrong.transitional: ./typed/cronjob-transitional.yaml does not hold to path spec.timeout")
	assert.Contains(t, errs, "version.transitional: ./typed/cronjob-transitional-v2.yaml stores v2 but ./schemas/cronjob.yaml stores v1")
	assert.Contains(t, errs, "empty.transitional.schema: is required")
}

func TestSchemas_Objects(t *testing.T) {
	f := load(t, `
migrations:
  cronjob:
    from:
      schema: ./typed/cronjob-old.yaml
      objects: [./objects/cronjobs.yaml, ./objects/none.yaml]
    to: {schema: ./schemas/cronjob.yaml}
    spec:
      replicas: {to: scaling.min}
`)
	errs, _ := messages(f.Validate())
	assert.Contains(t, errs, "cronjob.from.objects: ./objects/cronjobs.yaml default/nightly has fields ./typed/cronjob-old.yaml does not declare: spec.typo")
	assert.Contains(t, errs, "cronjob.from.objects: ./objects/cronjobs.yaml default/nightly is also in ./objects/cronjobs.yaml")
	assert.Contains(t, errs, "cronjob.from.objects: ./objects/cronjobs.yaml default/wrong is config.example.com/v1 AppConfig, not CronJob.batch.example.com")
	assert.Contains(t, errs, "cronjob.from.objects: ./objects/none.yaml:")
}
