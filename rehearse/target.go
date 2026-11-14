package rehearse

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/intentd/tools/cluster"
	"github.com/intentd/tools/kubeclient"
)

// KindCluster is the kind cluster Connect creates when given no context.
const KindCluster = "kome-rehearse"

// Where says which cluster to rehearse in.
type Where struct {
	// Context is a kubeconfig context to use; no cluster is created.
	Context string
	// UseCurrent uses the current kubeconfig context; no cluster is created.
	UseCurrent bool
	// Keep keeps the kind cluster Connect created.
	Keep bool
	// Kind configures the kind cluster Connect creates.
	Kind cluster.KindOptions
	// Log, when set, is told what Connect and its cleanup do.
	Log io.Writer
}

// Connect returns a client for the cluster w names: a kubeconfig context, the
// current one, or else a kind cluster it creates. Call done when the
// rehearsal ends; it deletes a cluster Connect created unless Keep.
func Connect(ctx context.Context, w Where) (kube kubeclient.Interface, done func(), err error) {
	logf := func(format string, args ...any) {
		if w.Log != nil {
			fmt.Fprintf(w.Log, format+"\n", args...)
		}
	}
	done = func() {}
	kubeContext := w.Context
	if kubeContext == "" && !w.UseCurrent {
		logf("creating kind cluster %s", KindCluster)
		if err := cluster.EnsureKind(ctx, KindCluster, w.Kind); err != nil {
			return nil, done, err
		}
		kubeContext = "kind-" + KindCluster
		if !w.Keep {
			done = func() {
				logf("deleting kind cluster %s", KindCluster)
				_ = cluster.DeleteKind(ctx, KindCluster, w.Kind)
			}
		}
	}
	kube, err = cluster.LocalClient(ctx, kubeContext)
	if err != nil {
		done()
		return nil, func() {}, err
	}
	return kube, done, nil
}

// WriteJSON writes the report as {"passed": …, "steps": […], "issues": […]}.
func WriteJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(map[string]any{"passed": r.Passed(), "steps": r.Steps, "issues": r.Issues})
}

// WriteSummary writes the known issues at the end of a run, so a user sees
// at once what the migration would meet.
func WriteSummary(w io.Writer, r Report) {
	if len(r.Issues) == 0 {
		return
	}
	fmt.Fprintf(w, "\nknown issues (%d): the migration meets these when it runs for real\n", len(r.Issues))
	for _, i := range r.Issues {
		fmt.Fprintf(w, "  ⚠ %s\n", i)
	}
}
