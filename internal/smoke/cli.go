package smoke

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/paulgmiller/infestation/internal/executil"
	"io"
)

// CLI is the standalone checker: Kubernetes credentials are its only cluster
// input. Provider state and cloud credentials are deliberately not accepted.
func CLI(ctx context.Context, args []string, out io.Writer) error {
	f := flag.NewFlagSet("infestation-smoke", flag.ContinueOnError)
	f.SetOutput(out)
	f.Usage = func() {
		fmt.Fprintln(out, "Usage: infestation-smoke [flags]\n\nDiscovers all nodes through Kubernetes and creates DNS/HTTP workload checks.\nRequires kubectl and permissions to list nodes and create/read test resources.\nTest pods/services remain in infestation-smoke for inspection; Finished Jobs expire after one hour.\nOptional Tailscale latency jobs require privileged pods and host socket access.\n\nFlags:")
		f.PrintDefaults()
	}
	kube := f.String("kubeconfig", "", "kubeconfig file; default: kubectl's KUBECONFIG/current context")
	dir := f.String("out-dir", "smoke-results", "local test manifest/report directory")
	latency := f.Bool("tailnet-latency", false, "measure directed host Tailscale latency via privileged Kubernetes Jobs")
	if err := f.Parse(args); errors.Is(err, flag.ErrHelp) {
		return nil
	} else if err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *dir == "" {
		return fmt.Errorf("--out-dir must not be empty")
	}
	return Run(ctx, executil.OSRunner{Output: out}, *dir, *kube, Options{TailnetLatency: *latency, Log: out})
}
