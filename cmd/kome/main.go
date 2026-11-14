// Command kome migrates Kubernetes objects between schemas: validate a KOME
// file, write its transitional schemas, migrate up, restore down, make a
// migration final with clean, and rehearse the whole lifecycle.
package main

import (
	"github.com/intentd/tools/term"
	"github.com/spf13/cobra"
)

func main() {
	if err := root().Execute(); err != nil {
		term.FatalExit("kome", err)
	}
}

func root() *cobra.Command {
	cmd := &cobra.Command{
		Use:           "kome",
		Short:         "Migrate Kubernetes objects between schemas, reversibly",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.AddCommand(validateCmd(), transitionalCmd(), runCmd("up"), runCmd("down"), runCmd("clean"), rehearseCmd(), versionCmd())
	return cmd
}
