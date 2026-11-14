// Command kome migrates Kubernetes objects between schemas (see package cli).
package main

import (
	"github.com/intentd/kome/cli"
	"github.com/intentd/tools/term"
)

func main() {
	if err := cli.Command().Execute(); err != nil {
		term.FatalExit("kome", err)
	}
}
