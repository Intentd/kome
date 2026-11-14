// Package rehearse runs each migration from start to end in a cluster, with
// the file that will run in production and its objects: schemas applied,
// objects seeded, up refused without the transitional schema, dry-run, up,
// an edit that down must leave alone, up again, the final schema, clean. It
// checks KOME's own results and the objects read back; it has no assertion
// language of its own.
package rehearse

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/intentd/kome"
	"github.com/intentd/kome/pkg/declaration"
	"github.com/intentd/kome/pkg/engine"
	"github.com/intentd/tools/fileio"
	"github.com/intentd/tools/kube"
	"github.com/intentd/tools/structured"
)

// Step is one checked step of a migration's rehearsal.
type Step struct {
	Migration string `json:"migration"`
	Number    int    `json:"step"`
	Name      string `json:"name"`
	Passed    bool   `json:"passed"`
	Detail    string `json:"detail,omitempty"`
	// Issues counts the known issues this step found (see Issue).
	Issues int `json:"issues,omitempty"`
}

func (s Step) String() string {
	mark := "ok"
	if !s.Passed {
		mark = "FAILED"
	}
	line := fmt.Sprintf("%s %d. %s: %s", s.Migration, s.Number, s.Name, mark)
	if s.Detail != "" {
		line += " (" + s.Detail + ")"
	}

	issues := s.Issues
	if issues > 0 {
		issueTxt := "issue"
		if issues > 1 {
			issueTxt += "s"
		}
		line += fmt.Sprintf(" ⚠ %d known %s", s.Issues, issueTxt)
	}
	return line
}

// Issue is what the migration meets on one object: a name collision, a
// destination that drops fields, any error KOME reports for the object. It
// is a finding, not a failure: the object is set aside and the rehearsal
// goes on with the others. Running the migration for real meets it too.
type Issue struct {
	Migration string `json:"migration"`
	Step      int    `json:"step"`
	Object    string `json:"object"`
	Message   string `json:"message"`
}

func (i Issue) String() string {
	return fmt.Sprintf("%s step %d: %s: %s", i.Migration, i.Step, i.Object, i.Message)
}

// Report is a rehearsal's steps and the known issues they found.
type Report struct {
	Steps  []Step  `json:"steps"`
	Issues []Issue `json:"issues,omitempty"`
}

// Passed reports whether every step passed; known issues do not fail it.
func (r Report) Passed() bool {
	for _, s := range r.Steps {
		if !s.Passed {
			return false
		}
	}
	return true
}

// Rehearsal rehearses a file's migrations.
type Rehearsal struct {
	File    *declaration.File
	Engine  *kome.Engine
	Cluster Cluster
	// Step, when set, is called as each step ends.
	Step func(Step)
}

// Run rehearses each migration in ms, in order. A failing step stops its
// migration; the others still run. Known issues do not stop anything. It
// returns every step and issue, and an error naming the first failure.
func (r *Rehearsal) Run(ctx context.Context, ms kome.Migrations) (Report, error) {
	var rep Report
	var first error
	for _, m := range ms {
		run := &migrationRun{Rehearsal: r, m: m, ann: engine.AnnotationsFor(r.prefix()), setAside: map[string]bool{}}
		var err error
		if m.Level == declaration.LevelField {
			err = run.field(ctx)
		} else {
			err = run.kind(ctx)
		}
		rep.Steps = append(rep.Steps, run.steps...)
		rep.Issues = append(rep.Issues, run.issues...)
		if err != nil && first == nil {
			first = err
		}
	}
	return rep, first
}

func (r *Rehearsal) prefix() string {
	if r.Engine.Prefix != "" {
		return r.Engine.Prefix
	}
	return engine.DefaultPrefix
}

// migrationRun is one migration's rehearsal.
type migrationRun struct {
	*Rehearsal
	m      kome.Migration
	ann    engine.Annotations
	steps  []Step
	issues []Issue
	// setAside are the objects with a known issue ("namespace/name"); the
	// checks leave them out.
	setAside map[string]bool
}

// absorb records each object error in res as a known issue of step n, once,
// and sets the object aside.
func (mr *migrationRun) absorb(n int, res []*engine.Result) {
	for _, r := range res {
		for _, o := range r.Objects {
			if o.Err == nil {
				continue
			}
			key := o.Ref()
			if !mr.setAside[key] {
				mr.issues = append(mr.issues, Issue{Migration: mr.m.Name, Step: n, Object: key, Message: o.Err.Error()})
			}
			mr.setAside[key] = true
		}
	}
}

// keep returns objs without those set aside.
func (mr *migrationRun) keep(objs []*unstructured.Unstructured) []*unstructured.Unstructured {
	var out []*unstructured.Unstructured
	for _, o := range objs {
		if !mr.setAside[engine.Ref(o.GetNamespace(), o.GetName())] {
			out = append(out, o)
		}
	}
	return out
}

// check records step n; a failed one ends the migration with an error.
func (mr *migrationRun) check(n int, name string, err error, detail string) error {
	s := Step{Migration: mr.m.Name, Number: n, Name: name, Passed: err == nil, Detail: detail}
	for _, i := range mr.issues {
		if i.Step == n {
			s.Issues++
		}
	}
	if err != nil {
		s.Detail = err.Error()
	}
	mr.steps = append(mr.steps, s)
	if mr.Step != nil {
		mr.Step(s)
	}
	if err != nil {
		return fmt.Errorf("%s step %d (%s): %w", mr.m.Name, n, name, err)
	}
	return nil
}

func (mr *migrationRun) decl() declaration.Migration { return mr.File.Migrations[mr.m.Name] }

func (mr *migrationRun) ms() kome.Migrations { return kome.Migrations{mr.m} }

// objects reads a shape's object files.
func (mr *migrationRun) objects(s declaration.Shape) ([]*unstructured.Unstructured, error) {
	var out []*unstructured.Unstructured
	for _, file := range s.Objects {
		objs, err := kube.ReadObjects(mr.File.Path(file))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		for _, o := range objs {
			u := &unstructured.Unstructured{Object: o}
			out = append(out, u)
		}
	}
	sort.Slice(out, func(i, j int) bool { return ref(out[i]) < ref(out[j]) })
	return out, nil
}

func (mr *migrationRun) schema(file string) ([]byte, error) {
	if file == "" {
		return nil, fmt.Errorf("rehearsal needs schema files: the identity alone is not enough")
	}
	return fileio.ReadLocal(mr.File.Path(file))
}

// specs reads back objs' specs, keyed by ref; a missing object maps to nil.
func (mr *migrationRun) specs(ctx context.Context, objs []*unstructured.Unstructured) (map[string]map[string]any, error) {
	out := map[string]map[string]any{}
	for _, o := range objs {
		live, err := mr.Cluster.Get(ctx, o)
		if err != nil {
			return nil, err
		}
		if live == nil {
			out[ref(o)] = nil
			continue
		}
		s, _, _ := unstructured.NestedMap(live.Object, "spec")
		out[ref(o)] = s
	}
	return out, nil
}

// unchanged compares two readings of the same objects.
func unchanged(before, after map[string]map[string]any) error {
	var changed []string
	for k, b := range before {
		if !reflect.DeepEqual(structured.JSONNormalize(b), structured.JSONNormalize(after[k])) {
			changed = append(changed, k)
		}
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		return fmt.Errorf("changed: %s", strings.Join(changed, ", "))
	}
	return nil
}

func ref(o *unstructured.Unstructured) string {
	return o.GetKind() + " " + engine.Ref(o.GetNamespace(), o.GetName())
}

// objectsOf returns a result's object outcomes by "namespace/name".
func objectsOf(res []*engine.Result) map[string]engine.ObjectResult {
	out := map[string]engine.ObjectResult{}
	for _, r := range res {
		for _, o := range r.Objects {
			out[o.Ref()] = o
		}
	}
	return out
}

// selected splits objs into those the run's results include and do not
// exclude, and the rest: objects outside the migration's selection.
func selected(res []*engine.Result, objs []*unstructured.Unstructured) (in, out []*unstructured.Unstructured) {
	got := objectsOf(res)
	for _, o := range objs {
		if r, ok := got[engine.Ref(o.GetNamespace(), o.GetName())]; ok && !r.Excluded {
			in = append(in, o)
		} else {
			out = append(out, o)
		}
	}
	return in, out
}

// edited returns v changed in a way schema s still accepts: another enum
// value, a number moved within its bounds, a string with a suffix, a bool
// flipped. A nil s puts no limit on the change.
func edited(v any, s *apiextensionsv1.JSONSchemaProps) any {
	if s != nil && len(s.Enum) > 0 {
		for _, e := range s.Enum {
			var opt any
			if json.Unmarshal(e.Raw, &opt) == nil && !reflect.DeepEqual(structured.JSONNormalize(opt), structured.JSONNormalize(v)) {
				return opt
			}
		}
		return v
	}
	switch x := v.(type) {
	case string:
		return x + "-edited"
	case bool:
		return !x
	case int64:
		if s != nil && s.Maximum != nil && float64(x+1) > *s.Maximum {
			return x - 1
		}
		return x + 1
	case float64:
		if s != nil && s.Maximum != nil && x+1 > *s.Maximum {
			return x - 1
		}
		return x + 1
	default:
		return v
	}
}

// liveSchema returns the live CRD's spec schema at path for gvk's kind, nil
// when it cannot be read.
func (mr *migrationRun) liveSchema(ctx context.Context, gvk schema.GroupVersionKind, path string) *apiextensionsv1.JSONSchemaProps {
	kc := mr.Engine.Kube
	m, err := kc.Mapping(gvk)
	if err != nil {
		return nil
	}
	crd, err := kube.GetCRD(ctx, kc.Dynamic(), m.Resource.Resource+"."+gvk.Group)
	if err != nil {
		return nil
	}
	return kube.SchemaAtV1(kube.StorageSpecSchema(crd), path)
}

func isNotFound(err error) bool { return apierrors.IsNotFound(err) }

func sortStrings(s []string) { sort.Strings(s) }
