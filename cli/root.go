package cli

import "github.com/spf13/cobra"

// Command returns the kome command: validate, transitional, up, down, clean,
// status, rehearse and version.
func Command() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "kome",
		Short:         "Migrate Kubernetes objects between schemas, reversibly",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(validateCmd(), transitionalCmd(), runCmd("up"), runCmd("down"), runCmd("clean"), runCmd("status"), rehearseCmd(), versionCmd())
	return cmd
}
