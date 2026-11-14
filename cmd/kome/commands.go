package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/intentd/kome"
	"github.com/intentd/kome/engine"
	"github.com/intentd/kome/report"
	"github.com/intentd/tools/cluster"
	"github.com/intentd/tools/term"
	"github.com/intentd/tools/version"
)

func validateCmd() *cobra.Command {
	var files []string
	var kind, output string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Check the file offline",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if output == "json" {
				return validateJSON(files, out)
			}
			l, err := load(files, kind, nil, false, out)
			if err != nil {
				return err
			}
			ms, err := l.migrations(kind)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "valid: %d migration(s)\n", len(ms))
			return nil
		},
	}
	cmd.Flags().StringSliceVarP(&files, "file", "f", nil, "the KOME file (repeatable; default kome.yaml)")
	cmd.Flags().StringVarP(&kind, "kind", "k", "", "only the migrations whose source is this kind")
	cmd.Flags().StringVarP(&output, "output", "o", "text", "text or json")
	return cmd
}

// validateJSON prints each file's findings as JSON.
func validateJSON(paths []string, w io.Writer) error {
	if len(paths) == 0 {
		paths = []string{"kome.yaml"}
	}
	type finding struct {
		File, Migration, Path, Message string
		Warning                        bool
	}
	var all []finding
	failed := false
	for _, p := range paths {
		f, err := loadFile(p)
		if err != nil {
			return err
		}
		for _, x := range f.Validate().Findings {
			all = append(all, finding{p, x.Migration, x.Path, x.Message, x.Warning})
			failed = failed || !x.Warning
		}
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(map[string]any{"valid": !failed, "findings": all}); err != nil {
		return err
	}
	if failed {
		return fmt.Errorf("the file has errors")
	}
	return nil
}

func transitionalCmd() *cobra.Command {
	var files []string
	var kind, dir string
	cmd := &cobra.Command{
		Use:   "transitional",
		Short: "Write each field migration's transitional schema",
		Long: `Write the schema each field migration runs under: the destination schema,
plus every old path it lacks, with the new paths optional. Apply it before
kome up, and the destination schema after.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := load(files, kind, nil, false, cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			ms, err := l.migrations(kind)
			if err != nil {
				return err
			}
			for _, m := range ms {
				f := l.owner[m.Name]
				if d := f.Migrations[m.Name]; d.Level() != "field" || d.Transitional != nil {
					continue
				}
				out, err := kome.Transitional(f, m.Name)
				if err != nil {
					return err
				}
				if dir == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "# %s\n---\n%s", m.Name, out)
					continue
				}
				if err := os.MkdirAll(dir, 0o755); err != nil {
					return err
				}
				path := filepath.Join(dir, m.Name+"-transitional.yaml")
				if err := os.WriteFile(path, out, 0o644); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVarP(&files, "file", "f", nil, "the KOME file (repeatable; default kome.yaml)")
	cmd.Flags().StringVarP(&kind, "kind", "k", "", "only the migrations whose source is this kind")
	cmd.Flags().StringVarP(&dir, "output", "o", "", "write the files to this directory (default: print them)")
	return cmd
}

// runFlags are the flags of up, down and clean.
type runFlags struct {
	files                []string
	kind, name, selector string
	namespaces           []string
	dryRun, strict       bool
	output               string
	pageSize             int64
	verbose              bool
	kubeContext          string
	yes, force, all      bool
	sets                 []string
	prefix, fieldManager string
}

func runCmd(command string) *cobra.Command {
	var fl runFlags
	short := map[string]string{
		"up":    "Migrate the selected objects, after checking the live schema",
		"down":  "Restore migrated objects from their records, leaving drift alone",
		"clean": "Remove the records: the migrations become final",
	}[command]
	cmd := &cobra.Command{
		Use:   command,
		Short: short,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), command, fl, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	f := cmd.Flags()
	f.StringSliceVarP(&fl.files, "file", "f", nil, "the KOME file (repeatable; default kome.yaml)")
	f.StringVarP(&fl.kind, "kind", "k", "", "only the migrations whose source is this kind")
	f.StringVar(&fl.name, "name", "", "only the object with this name")
	f.StringVar(&fl.selector, "selector", "", "only objects matching this label selector, with the declared one")
	f.StringSliceVarP(&fl.namespaces, "namespace", "n", nil, "only these namespaces, within the declared ones")
	f.BoolVar(&fl.dryRun, "dry-run", false, "server-side dry run: everything runs and is reported, nothing is written")
	f.BoolVar(&fl.strict, "strict", false, "a failed object fails the run")
	f.StringVarP(&fl.output, "output", "o", "text", "text or json")
	f.Int64Var(&fl.pageSize, "page-size", 0, "objects per List call (0: the API server decides)")
	f.BoolVar(&fl.verbose, "verbose", false, "show each object's spec after the change")
	f.StringVar(&fl.kubeContext, "context", "", "the kubeconfig context (default: the current one)")
	f.StringVar(&fl.prefix, "prefix", engine.DefaultPrefix, "the record's annotation prefix")
	f.StringVar(&fl.fieldManager, "field-manager", "kome", "the field manager of KOME's writes")
	if command == "up" {
		f.BoolVarP(&fl.yes, "yes", "y", false, "don't ask before pruning sources")
		f.BoolVar(&fl.force, "force", false, "take ownership of conflicting fields when applying destinations")
		f.StringArrayVar(&fl.sets, "set", nil, "change a migration for this run, e.g. prune=true (needs --kind or --all)")
		f.BoolVarP(&fl.all, "all", "A", false, "apply --set to every migration")
	}
	return cmd
}

func run(ctx context.Context, command string, fl runFlags, out, errOut io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	l, err := load(fl.files, fl.kind, fl.sets, fl.all, errOut)
	if err != nil {
		return err
	}
	ms, err := l.migrations(fl.kind)
	if err != nil {
		return err
	}
	kube, err := cluster.LocalClient(ctx, fl.kubeContext)
	if err != nil {
		return err
	}
	e := &kome.Engine{Kube: kube, Prefix: fl.prefix, FieldManager: fl.fieldManager}
	o := kome.Options{
		DryRun: fl.dryRun, Strict: fl.strict, Force: fl.force, PageSize: fl.pageSize,
		Scope: engine.Selection{Namespaces: fl.namespaces, Name: fl.name, Selector: fl.selector},
	}
	if fl.output != "json" {
		o.Progress = progress(errOut)
	}

	if command == "up" && !fl.dryRun && !fl.yes && prunes(ms) {
		if err := confirmPrune(ctx, e, ms, o, errOut); err != nil {
			return err
		}
	}

	var results []*engine.Result
	switch command {
	case "up":
		results, err = e.Up(ctx, ms, o)
	case "down":
		results, err = e.Down(ctx, ms, o)
	default:
		results, err = e.Clean(ctx, ms, o)
	}
	rep := report.Build(command, fl.dryRun, fl.verbose, results)
	if fl.output == "json" {
		if jerr := rep.JSON(out); jerr != nil {
			return jerr
		}
	} else {
		rep.Text(out)
	}
	if err != nil {
		return err
	}
	if fl.strict && rep.Failed() {
		return fmt.Errorf("%d object(s) failed", rep.Totals.Failed)
	}
	return nil
}

func prunes(ms kome.Migrations) bool {
	for _, m := range ms {
		if m.Kind.Prune {
			return true
		}
	}
	return false
}

// confirmPrune dry-runs the migrations, says how many sources each would
// delete, and asks. Without a terminal it fails and asks for --yes.
func confirmPrune(ctx context.Context, e *kome.Engine, ms kome.Migrations, o kome.Options, w io.Writer) error {
	dry := o
	dry.DryRun, dry.Progress = true, nil
	results, err := e.Up(ctx, ms, dry)
	if err != nil {
		return err
	}
	total := 0
	for _, r := range results {
		n := 0
		for _, obj := range r.Objects {
			if obj.Pruned && obj.Err == nil {
				n++
			}
		}
		if n > 0 {
			fmt.Fprintf(w, "%s: %d %s source(s) would be deleted after their destinations are written\n", r.Migration, n, r.Kind)
		}
		total += n
	}
	if total == 0 {
		return nil
	}
	if !term.IsTerminal() {
		return fmt.Errorf("up would delete %d source(s); rerun with --yes to confirm", total)
	}
	ok, err := term.ConfirmFrom(fmt.Sprintf("Delete %d source(s)?", total), os.Stdin, w)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("cancelled: nothing was written")
	}
	return nil
}

// progress prints a running count per migration on w.
func progress(w io.Writer) func(string, int, int) {
	return func(migration string, done, total int) {
		fmt.Fprintf(w, "\r%s: %d/%d", migration, done, total)
		if done == total {
			fmt.Fprintln(w)
		}
	}
}

func versionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			verbose, _ := cmd.Flags().GetBool("verbose")
			short, _ := cmd.Flags().GetBool("short")

			if verbose {
				fmt.Fprintln(out, "KOME")
				fmt.Fprintf(out, "%-12s %s\n", "Version:", version.Short())
				fmt.Fprintf(out, "%-12s %s\n", "Commit:", version.Commit)
				fmt.Fprintf(out, "%-12s %s\n", "Built:", version.Date)
				return nil
			}

			if short {
				fmt.Fprintln(out, "kome", version.Short())
				return nil
			}

			fmt.Fprintf(
				out,
				"kome %s (commit %s, built %s)\n",
				version.Short(),
				version.Commit,
				version.Date,
			)
			return nil
		},
	}

	cmd.Flags().BoolP("verbose", "v", false, "show detailed version information")
	cmd.Flags().BoolP("short", "s", false, "print only the version")

	return cmd
}
