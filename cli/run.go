package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/intentd/kome"
	"github.com/intentd/kome/engine"
	"github.com/intentd/kome/report"
	"github.com/intentd/tools/cluster"
	"github.com/intentd/tools/term"
)

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
		"up":     "Migrate the selected objects, after checking the live schema",
		"down":   "Restore migrated objects from their records, leaving drift alone",
		"clean":  "Remove the records: the migrations become final",
		"status": "Show, per migration, how many objects are pending, migrated and drifted",
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
	if command != "status" {
		f.BoolVar(&fl.dryRun, "dry-run", false, "server-side dry run: everything runs and is reported, nothing is written")
		f.BoolVar(&fl.strict, "strict", false, "a failed object fails the run")
		f.BoolVar(&fl.verbose, "verbose", false, "show each object's spec after the change")
		f.StringVar(&fl.fieldManager, "field-manager", "", "the field manager of KOME's writes, over each migration's fieldManager (default: the file's, else kome)")
	}
	f.StringVarP(&fl.output, "output", "o", "text", "text or json")
	f.Int64Var(&fl.pageSize, "page-size", 0, "objects per List call (0: the API server decides)")
	f.StringVar(&fl.kubeContext, "context", "", "the kubeconfig context (default: the current one)")
	f.StringVar(&fl.prefix, "prefix", engine.DefaultPrefix, "the record's annotation prefix")
	if command == "up" {
		f.BoolVarP(&fl.yes, "yes", "y", false, "don't ask before pruning sources")
		f.BoolVar(&fl.force, "force", false, "take ownership of conflicting fields when applying destinations")
		f.StringArrayVar(&fl.sets, "set", nil, "change a migration for this run, e.g. lifecycle.prune=true (needs --kind or --all)")
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
	k, err := kome.KubeForConfig(kube.RestConfig(), kube.RESTMapper())
	if err != nil {
		return err
	}
	e := &kome.Engine{Kube: k, Prefix: fl.prefix, FieldManager: fl.fieldManager}
	all, err := l.migrations("")
	if err != nil {
		return err
	}
	o := kome.Options{
		DryRun: fl.dryRun, Strict: fl.strict, Force: fl.force, PageSize: fl.pageSize,
		Scope: engine.Selection{Namespaces: fl.namespaces, Name: fl.name, Selector: fl.selector},
		All:   all,
	}
	if command == "status" {
		return status(ctx, e, ms, o, fl.output, out)
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
