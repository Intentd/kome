package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/intentd/tools/version"
)

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
