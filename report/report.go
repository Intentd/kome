// Package report turns a run's results into the JSON report and the text
// summary the CLI prints. Every object ends in one outcome: migrated,
// skipped, excluded, drifted, pruned, kept, too-large or failed.
package report

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/intentd/kome/declaration"
	"github.com/intentd/kome/engine"
)

// Report is a run's results.
type Report struct {
	Command string      `json:"command"`
	DryRun  bool        `json:"dryRun"`
	Kinds   []KindEntry `json:"kinds"`
	Totals  Totals      `json:"totals"`
}

// KindEntry is one migration's results.
type KindEntry struct {
	Migration string        `json:"migration"`
	Kind      string        `json:"kind"`
	To        string        `json:"to,omitempty"`
	Scope     *Scope        `json:"scope,omitempty"`
	CRs       []ObjectEntry `json:"crs"`
	Warnings  []string      `json:"warnings,omitempty"`
	// PruneBlocked gathers what kept sources from being pruned, and the
	// lines to add to the migration so they can be.
	PruneBlocked *PruneBlocked `json:"pruneBlocked,omitempty"`
}

// PruneBlocked is a migration's prune refusals, together.
type PruneBlocked struct {
	// Sources is how many sources were not pruned for these reasons.
	Sources int `json:"sources"`
	// Finalizers counts the sources carrying each finalizer not in
	// lifecycle.finalizers.
	Finalizers map[string]int `json:"finalizers,omitempty"`
	// Children are the sources that own objects while lifecycle.children
	// is unset, each with what it owns ("team/a: ConfigMap team/a-config").
	Children []string `json:"children,omitempty"`
	// CannotOwn are the sources with children the destination can't own.
	CannotOwn []string `json:"cannotOwn,omitempty"`
	// Add is the lifecycle block to put in the migration: the policy for
	// children, and the finalizers KOME may remove, those already declared
	// included. Kubernetes' own finalizers are never suggested.
	Add string `json:"add,omitempty"`

	declared []string // lifecycle.finalizers, for Add
}

// Scope is the selection a migration ran with.
type Scope struct {
	Namespaces []string `json:"namespaces,omitempty"`
	Name       string   `json:"name,omitempty"`
	Selector   string   `json:"selector,omitempty"`
}

// ObjectEntry is one object's outcome.
type ObjectEntry struct {
	Ref           string         `json:"ref"`
	Fields        []string       `json:"fields,omitempty"`
	DriftedFields []string       `json:"driftedFields,omitempty"`
	AllDrifted    bool           `json:"allDrifted,omitempty"`
	Drifted       bool           `json:"drifted,omitempty"`
	Pruned        bool           `json:"pruned,omitempty"`
	Kept          bool           `json:"kept,omitempty"`
	AfterSpec     map[string]any `json:"afterSpec,omitempty"`
	Skipped       bool           `json:"skipped,omitempty"`
	Excluded      bool           `json:"excluded,omitempty"`
	TooLarge      bool           `json:"tooLarge,omitempty"`
	Error         string         `json:"error,omitempty"`
}

// Totals counts the outcomes.
type Totals struct {
	CRs        int `json:"crs"`
	Migrated   int `json:"migrated"`
	Drifted    int `json:"drifted"`
	AllDrifted int `json:"allDrifted"`
	Skipped    int `json:"skipped"`
	Excluded   int `json:"excluded"`
	Pruned     int `json:"pruned"`
	Kept       int `json:"kept"`
	TooLarge   int `json:"tooLarge"`
	Failed     int `json:"failed"`
}

// Build reports results of command. afterSpec is kept only when verbose.
func Build(command string, dryRun, verbose bool, results []*engine.Result) Report {
	r := Report{Command: command, DryRun: dryRun, Kinds: []KindEntry{}}
	for _, res := range results {
		if res == nil {
			continue
		}
		k := KindEntry{Migration: res.Migration, Kind: res.Kind, To: res.To, CRs: []ObjectEntry{}, Warnings: res.Warnings}
		if s := res.Selection; len(s.Namespaces) > 0 || s.Name != "" || s.Selector != "" {
			k.Scope = &Scope{Namespaces: s.Namespaces, Name: s.Name, Selector: s.Selector}
		}
		for _, o := range res.Objects {
			e := ObjectEntry{
				Ref: o.Ref(), Fields: o.Fields, DriftedFields: o.DriftedFields, AllDrifted: o.AllDrifted,
				Drifted: o.Drifted, Pruned: o.Pruned, Kept: o.Kept, Skipped: o.Skipped, Excluded: o.Excluded, TooLarge: o.TooLarge,
			}
			if verbose {
				e.AfterSpec = o.AfterSpec
			}
			if o.Err != nil {
				e.Error = o.Err.Error()
			}
			var pb *engine.PruneBlocked
			if errors.As(o.Err, &pb) {
				k.PruneBlocked = k.PruneBlocked.add(o.Ref(), pb)
			}
			k.CRs = append(k.CRs, e)
			r.Totals.count(o)
		}
		if k.PruneBlocked != nil {
			k.PruneBlocked.suggest()
		}
		r.Kinds = append(r.Kinds, k)
	}
	return r
}

// add gathers one source's refusal.
func (p *PruneBlocked) add(ref string, b *engine.PruneBlocked) *PruneBlocked {
	if p == nil {
		p = &PruneBlocked{Finalizers: map[string]int{}}
	}
	p.Sources++
	for _, f := range b.UndeclaredFinalizers {
		p.Finalizers[f]++
	}
	if len(b.Children) > 0 {
		p.Children = append(p.Children, ref+": "+engine.Names(b.Children))
	}
	if len(b.CannotOwn) > 0 {
		p.CannotOwn = append(p.CannotOwn, ref+": "+engine.Names(b.CannotOwn))
	}
	p.declared = b.DeclaredFinalizers
	return p
}

// suggest writes Add: the lifecycle lines that let the refused sources be
// pruned.
func (p *PruneBlocked) suggest() {
	var lines []string
	if len(p.Children) > 0 {
		lines = append(lines, "  children: reparent   # or orphan, delete")
	}
	finalizers := append([]string(nil), p.declared...)
	added := false
	for _, f := range sortedKeys(p.Finalizers) {
		if !declaration.IsSystemFinalizer(f) && !contains(finalizers, f) {
			finalizers, added = append(finalizers, f), true
		}
	}
	if added {
		lines = append(lines, "  finalizers: ["+strings.Join(finalizers, ", ")+"]")
	}
	if len(lines) > 0 {
		p.Add = "lifecycle:\n" + strings.Join(lines, "\n")
	}
	if len(p.Finalizers) == 0 {
		p.Finalizers = nil
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func (t *Totals) count(o engine.ObjectResult) {
	t.CRs++
	switch {
	case o.TooLarge:
		t.TooLarge++
	case o.Err != nil:
		t.Failed++
	case o.Excluded:
		t.Excluded++
	case o.Skipped:
		t.Skipped++
	case o.AllDrifted:
		t.AllDrifted++
	case o.Drifted:
		t.Drifted++
	default:
		if len(o.Fields) > 0 {
			t.Migrated++
		}
		if len(o.DriftedFields) > 0 {
			t.Drifted++
		}
	}
	if o.Err == nil && o.Pruned {
		t.Pruned++
	}
	if o.Err == nil && o.Kept {
		t.Kept++
	}
}

// Outcome names an object's outcome.
func Outcome(e ObjectEntry) string {
	switch {
	case e.TooLarge:
		return "too-large"
	case e.Error != "":
		return "failed"
	case e.Excluded:
		return "excluded"
	case e.Skipped:
		return "skipped"
	case e.AllDrifted, e.Drifted:
		return "drifted"
	case e.Kept:
		return "kept"
	case e.Pruned && len(e.Fields) == 0:
		return "pruned"
	default:
		return "migrated"
	}
}

// JSON writes r as indented JSON.
func (r Report) JSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// Text writes r for a terminal: each migration, its objects' outcomes, and
// the totals.
func (r Report) Text(w io.Writer) {
	done := map[string]string{"up": "migrated", "down": "restored", "clean": "cleaned"}[r.Command]
	if r.DryRun {
		fmt.Fprintf(w, "dry run: nothing written\n")
	}
	for _, k := range r.Kinds {
		title := k.Kind
		if k.To != "" {
			title += " → " + k.To
		}
		fmt.Fprintf(w, "\n%s (%s)\n", k.Migration, title)
		for _, warn := range k.Warnings {
			fmt.Fprintf(w, "  ! %s\n", warn)
		}
		if len(k.CRs) == 0 {
			fmt.Fprintf(w, "  no objects selected\n")
		}
		for _, o := range k.CRs {
			line := fmt.Sprintf("  %-9s %s", Outcome(o), o.Ref)
			switch {
			case o.Error != "":
				line += ": " + o.Error
			case len(o.DriftedFields) > 0:
				line += fmt.Sprintf(" (drifted: %s)", strings.Join(o.DriftedFields, ", "))
			case len(o.Fields) > 0 && o.Fields[0] != "spec":
				line += " (" + strings.Join(o.Fields, ", ") + ")"
			}
			if o.Pruned && len(o.Fields) > 0 {
				line += ", source pruned"
			}
			fmt.Fprintln(w, line)
		}
		if p := k.PruneBlocked; p != nil {
			p.text(w)
		}
	}
	t := r.Totals
	fmt.Fprintf(w, "\n%s: %d objects: %d %s, %d skipped, %d excluded, %d drifted, %d pruned, %d kept, %d too-large, %d failed\n",
		r.Command, t.CRs, t.Migrated, done, t.Skipped, t.Excluded, t.Drifted+t.AllDrifted, t.Pruned, t.Kept, t.TooLarge, t.Failed)
}

// Failed reports whether any object failed or was too large to migrate:
// either way the migration did not finish.
func (r Report) Failed() bool { return r.Totals.Failed > 0 || r.Totals.TooLarge > 0 }

// text writes the refusals and the lines to add.
func (p *PruneBlocked) text(w io.Writer) {
	fmt.Fprintf(w, "\n  ⚠ %s not pruned\n", plural(p.Sources, "source"))
	if len(p.Finalizers) > 0 {
		fmt.Fprintf(w, "    finalizers not in lifecycle.finalizers:\n")
		for _, f := range sortedKeys(p.Finalizers) {
			note := ""
			if declaration.IsSystemFinalizer(f) {
				note = "  (Kubernetes': never removed; these wait until it is gone)"
			}
			fmt.Fprintf(w, "      %-36s %s%s\n", f, plural(p.Finalizers[f], "source"), note)
		}
	}
	if len(p.Children) > 0 {
		fmt.Fprintf(w, "    own objects, and lifecycle.children is not set:\n")
		writeSome(w, p.Children)
	}
	if len(p.CannotOwn) > 0 {
		fmt.Fprintf(w, "    have children the destination can't own (a namespaced owner owns only its own namespace):\n")
		writeSome(w, p.CannotOwn)
	}
	if p.Add != "" {
		fmt.Fprintf(w, "    to prune them, add to the migration:\n")
		for _, l := range strings.Split(p.Add, "\n") {
			fmt.Fprintf(w, "      %s\n", l)
		}
	}
}

// writeSome writes up to five lines, then how many more.
func writeSome(w io.Writer, lines []string) {
	for i, l := range lines {
		if i == 5 {
			fmt.Fprintf(w, "      and %d more\n", len(lines)-5)
			return
		}
		fmt.Fprintf(w, "      %s\n", l)
	}
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
