package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
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
