package declaration

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/intentd/tools/condition"
	"github.com/intentd/tools/kube"
)

// Finding is one problem in a file.
type Finding struct {
	// Migration is the migration it is in; empty for the file.
	Migration string
	// Path is where in the migration, e.g. "to.namespace" or "spec.replicas".
	Path    string
	Message string
	// Warning findings do not stop a run.
	Warning bool
}

func (f Finding) String() string {
	where := f.Migration
	if f.Path != "" {
		where += "." + f.Path
	}
	if where == "" {
		return f.Message
	}
	return where + ": " + f.Message
}

// Report is what Validate found.
type Report struct {
	Findings []Finding
}

// Errors returns the findings that are not warnings.
func (r Report) Errors() []Finding { return r.filter(false) }

// Warnings returns the warnings.
func (r Report) Warnings() []Finding { return r.filter(true) }

func (r Report) filter(warning bool) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Warning == warning {
			out = append(out, f)
		}
	}
	return out
}

// Err joins the errors, nil when there are none.
func (r Report) Err() error {
	var errs []error
	for _, f := range r.Errors() {
		errs = append(errs, errors.New(f.String()))
	}
	return errors.Join(errs...)
}

// checker collects the findings of one migration.
type checker struct {
	name     string
	findings *[]Finding
}

func (c checker) err(path, format string, args ...any) {
	*c.findings = append(*c.findings, Finding{Migration: c.name, Path: path, Message: fmt.Sprintf(format, args...)})
}

func (c checker) warn(path, format string, args ...any) {
	*c.findings = append(*c.findings, Finding{Migration: c.name, Path: path, Message: fmt.Sprintf(format, args...), Warning: true})
}

// Validate runs the offline checks that need no schema contents: the
// identities, the level, the keys each level allows, scopes, field moves,
// selection and metadata.
func (f *File) Validate() Report {
	var findings []Finding
	if len(f.Migrations) == 0 {
		findings = append(findings, Finding{Message: "no migrations"})
	}
	for _, name := range f.Names() {
		c := checker{name: name, findings: &findings}
		if msgs := validation.IsDNS1123Label(name); len(msgs) > 0 {
			c.err("", "name %q: %s", name, strings.Join(msgs, "; "))
		}
		f.check(c, f.Migrations[name])
	}
	return Report{Findings: findings}
}

func (f *File) check(c checker, m Migration) {
	from, fromErr := f.Identify(m.From)
	if fromErr != nil {
		c.err("from", "%v", fromErr)
	}
	to, toErr := f.Identify(m.To.Shape)
	if toErr != nil {
		c.err("to", "%v", toErr)
	}
	for side, id := range map[string]Identity{"from": from, "to": to} {
		if id.PluralInferred {
			c.warn(side+".plural", "inferred as %q from the kind; declare it if that is wrong", id.Plural)
		}
	}

	switch m.Level() {
	case LevelField:
		if fromErr == nil && toErr == nil && !from.SameKind(to) {
			c.err("to", "spec moves need the same kind (%s → %s): move the kind first, in its own migration", from.GVK.Kind, to.GVK.Kind)
		}
		for path, set := range map[string]bool{
			"to.namespace":  m.To.Namespace != "",
			"to.metadata":   m.To.Metadata != nil,
			"to.copyStatus": m.To.CopyStatus,
			"prune":         m.Prune,
		} {
			if set {
				c.err(path, "applies to kind moves; this migration moves fields")
			}
		}
		checkFields(c, m.Spec)
	case LevelKind:
		if fromErr == nil && toErr == nil && from.SameKind(to) {
			c.err("spec", "to is the same kind with no spec moves: a schema change with no moves is a conversion, not a migration")
		}
		if m.Transitional != nil {
			c.err("transitional", "applies to field moves; this migration moves the kind")
		}
		if fromErr == nil && toErr == nil && from.FromFile && to.FromFile {
			checkScopes(c, from, to, m.To.Namespace)
		}
		if m.Prune {
			c.warn("prune", "deletes each source once its destination holds every source field")
		}
		checkMetadata(c, m.To.Metadata)
	}
	checkSelection(c, m.Selection)
	if fromErr == nil && toErr == nil {
		f.checkSchemas(c, m, from, to)
	}
}

func checkScopes(c checker, from, to Identity, namespace string) {
	switch {
	case !from.Namespaced && to.Namespaced && namespace == "":
		c.err("to.namespace", "required: %s is cluster-scoped and %s is namespaced", from.GVK.Kind, to.GVK.Kind)
	case from.Namespaced && !to.Namespaced:
		c.warn("to", "%s is cluster-scoped: namespaces are dropped, and sources with the same name in different namespaces collide", to.GVK.Kind)
		if namespace != "" {
			c.warn("to.namespace", "ignored: the destination is cluster-scoped")
		}
	case from.Namespaced == to.Namespaced && namespace != "":
		c.warn("to.namespace", "ignored: it applies only to a cluster-scoped source moving to a namespaced destination")
	}
	if namespace != "" {
		if msgs := validation.IsDNS1123Label(namespace); len(msgs) > 0 {
			c.err("to.namespace", "%q: %s", namespace, strings.Join(msgs, "; "))
		}
	}
}

// checkFields rejects two moves reading the same path, or one under another.
func checkFields(c checker, spec map[string]Field) {
	keys := make([]string, 0, len(spec))
	for k := range spec {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	sources := map[string]string{}
	type write struct{ key, path string }
	var writes []write
	for _, key := range keys {
		fl := spec[key]
		at := "spec." + key
		if len(fl.Values) > 0 && (fl.To != "" || fl.Value != "") {
			c.warn(at, "values is set, so to and value are ignored")
		}
		src := fl.SourcePath(key)
		for other, otherSrc := range sources {
			if src == otherSrc || strings.HasPrefix(src, otherSrc+".") || strings.HasPrefix(otherSrc, src+".") {
				c.err(at, "reads %q, which overlaps %q read by spec.%s", src, otherSrc, other)
			}
		}
		sources[key] = src
		for _, dst := range fl.Destinations(key) {
			for _, w := range writes {
				if dst == w.path || strings.HasPrefix(dst, w.path+".") || strings.HasPrefix(w.path, dst+".") {
					c.err(at, "writes %q, which overlaps %q written by spec.%s", dst, w.path, w.key)
				}
			}
		}
		for _, dst := range fl.Destinations(key) {
			writes = append(writes, write{key, dst})
		}
	}
}

func checkSelection(c checker, s Selection) {
	if s.Selector != "" {
		if _, err := labels.Parse(s.Selector); err != nil {
			c.err("selection.selector", "%v", err)
		}
	}
	if s.Name != "" {
		if err := kube.ValidKubernetesName(s.Name); err != nil {
			c.err("selection.name", "%v", err)
		}
	}
	for _, ns := range s.Namespaces {
		if msgs := validation.IsDNS1123Label(ns); len(msgs) > 0 {
			c.err("selection.namespaces", "%q: %s", ns, strings.Join(msgs, "; "))
		}
	}
	for list, cs := range map[string][]condition.Condition{"when": s.When, "or": s.Or} {
		for i, cond := range cs {
			at := fmt.Sprintf("selection.%s[%d]", list, i)
			if cond.Field == "" && cond.Time == nil && cond.DayOfWeek == nil && cond.Cron == "" {
				c.err(at, "field is required")
			}
			if err := cond.CheckComparison(); err != nil {
				c.err(at, "%v", err)
			}
		}
	}
}

func checkMetadata(c checker, m *Metadata) {
	if m == nil {
		return
	}
	if err := kube.ValidateAnnotationKeys(m.Labels); err != nil {
		c.err("to.metadata.labels", "%v", err)
	}
	if err := kube.ValidateAnnotationKeys(m.Annotations); err != nil {
		c.err("to.metadata.annotations", "%v", err)
	}
}
