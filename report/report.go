// Package report turns a run's results into the JSON report and the text
// summary the CLI prints. Every object ends in one outcome: migrated,
// skipped, excluded, drifted, pruned, kept or failed.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

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
				Drifted: o.Drifted, Pruned: o.Pruned, Kept: o.Kept, Skipped: o.Skipped, Excluded: o.Excluded,
			}
			if verbose {
				e.AfterSpec = o.AfterSpec
			}
			if o.Err != nil {
				e.Error = o.Err.Error()
			}
			k.CRs = append(k.CRs, e)
			r.Totals.count(o)
		}
		r.Kinds = append(r.Kinds, k)
	}
	return r
}

func (t *Totals) count(o engine.ObjectResult) {
	t.CRs++
	switch {
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
	}
	t := r.Totals
	fmt.Fprintf(w, "\n%s: %d objects: %d %s, %d skipped, %d excluded, %d drifted, %d pruned, %d kept, %d failed\n",
		r.Command, t.CRs, t.Migrated, done, t.Skipped, t.Excluded, t.Drifted+t.AllDrifted, t.Pruned, t.Kept, t.Failed)
}

// Failed reports whether any object failed.
func (r Report) Failed() bool { return r.Totals.Failed > 0 }
