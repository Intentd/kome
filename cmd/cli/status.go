package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/intentd/kome"
)

// status prints where each migration stands, from the objects themselves.
func status(ctx context.Context, e *kome.Engine, ms kome.Migrations, o kome.Options, output string, out io.Writer) error {
	st, err := e.Status(ctx, ms, o)
	if output == "json" {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if jerr := enc.Encode(st); jerr != nil {
			return jerr
		}
		return err
	}
	fmt.Fprintf(out, "%-24s %8s %9s %8s  %s\n", "MIGRATION", "PENDING", "MIGRATED", "DRIFTED", "NEEDS")
	for _, s := range st {
		var needs []string
		if m, ok := ms.Named(s.Migration); ok {
			needs = m.Needs
		}
		fmt.Fprintf(out, "%-24s %8d %9d %8d  %s\n", s.Migration, s.Pending, s.Migrated, s.Drifted, strings.Join(needs, ", "))
	}
	return err
}
