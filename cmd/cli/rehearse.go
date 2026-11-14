package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/intentd/kome"
	"github.com/intentd/kome/pkg/rehearse"
)

func rehearseCmd() *cobra.Command {
	var files []string
	var kind, output, context string
	var useCurrent, keep bool
	cmd := &cobra.Command{
		Use:   "rehearse",
		Short: "Run every migration from start to end in a throwaway cluster",
		Long: `Run each migration's whole lifecycle with its objects: apply the schemas,
seed the objects, check up refuses without the transitional schema, dry-run,
up, edit one field and check down leaves it alone, up again, apply the final
schema, clean. A failing step stops its migration and is named. What an object
would meet for real (a name collision, a destination that drops fields) is a
known issue: the object is set aside, the rehearsal goes on, and every issue
is listed at the end.

By default a kind cluster is created and deleted afterwards. Use --use-current
to rehearse in the current kubeconfig context, or --context to rehearse in a
specific cluster context; neither option creates or deletes a cluster.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
			l, err := load(files, kind, nil, false, errOut)
			if err != nil {
				return err
			}
			ms, err := l.migrations(kind)
			if err != nil {
				return err
			}

			kube, done, err := rehearse.Connect(ctx, rehearse.Where{Context: context, UseCurrent: useCurrent, Keep: keep, Log: errOut})
			if err != nil {
				return err
			}
			defer done()

			var rep rehearse.Report
			var firstErr error
			for _, f := range l.files {
				var fileMs kome.Migrations
				for _, m := range ms {
					if l.owner[m.Name] == f {
						fileMs = append(fileMs, m)
					}
				}
				if len(fileMs) == 0 {
					continue
				}
				k, err := kome.KubeForConfig(kube.RestConfig(), kube.RESTMapper())
				if err != nil {
					return err
				}
				r := &rehearse.Rehearsal{
					File: f, Engine: &kome.Engine{Kube: k}, Cluster: rehearse.KubeCluster{Kube: kube},
				}
				if output != "json" {
					r.Step = func(s rehearse.Step) { fmt.Fprintln(out, s) }
				}
				got, err := r.Run(ctx, fileMs)
				rep.Steps = append(rep.Steps, got.Steps...)
				rep.Issues = append(rep.Issues, got.Issues...)
				if err != nil && firstErr == nil {
					firstErr = err
				}
			}
			if output == "json" {
				if err := rehearse.WriteJSON(out, rep); err != nil {
					return err
				}
			} else {
				rehearse.WriteSummary(out, rep)
			}
			return firstErr
		},
	}
	cmd.Flags().StringSliceVarP(&files, "file", "f", nil, "the KOME file (repeatable; default kome.yaml)")
	cmd.Flags().StringVarP(&kind, "kind", "k", "", "only the migrations whose source is this kind")
	cmd.Flags().StringVarP(&output, "output", "o", "text", "text or json")
	cmd.Flags().BoolVar(&useCurrent, "use-current", false, "rehearse in the current kubeconfig context; no cluster is created or deleted")
	cmd.Flags().StringVarP(&context, "context", "c", "", "rehearse in the given kubeconfig context; no cluster is created or deleted")
	cmd.Flags().BoolVar(&keep, "keep", false, "keep the kind cluster rehearse created")
	return cmd
}
