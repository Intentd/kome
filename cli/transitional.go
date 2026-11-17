package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/intentd/kome"
)

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
