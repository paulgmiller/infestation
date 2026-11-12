// Package cli exposes the provider lifecycle and cluster connectivity checks.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"infestation/internal/azureconfig"
	"infestation/internal/ignition"
	"infestation/internal/vm"
)

const usage = `Infestation: Flatcar + CockroachDB + K3s

Usage: infestation <vm|azure> <create|start|stop|status|wait|kubeconfig> [flags]

create runs CockroachDB and K3s on every node. --agent omits the API server.
Use --init on exactly one founding server. Run a command with --help for flags.`

func Run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(out, usage)
		return nil
	}
	provider := args[0]
	if provider != "vm" && provider != "azure" {
		return fmt.Errorf("unknown command %q; run infestation --help", provider)
	}
	if len(args) == 1 || args[1] == "--help" {
		fmt.Fprintf(out, "Usage: infestation %s <create|start|stop|status|wait|kubeconfig> [flags]\n", provider)
		return nil
	}
	action := args[1]
	// Private subprocess entry point for the loopback-only executable server.
	if provider == "vm" && action == "artifact-server" {
		f := flag.NewFlagSet("artifact-server", flag.ContinueOnError)
		f.SetOutput(out)
		state := f.String("state-dir", "", "owned private state")
		if err := parse(f, args[2:]); err != nil {
			return help(err)
		}
		if *state == "" {
			return fmt.Errorf("--state-dir is required")
		}
		return vm.ServeArtifacts(ctx, *state)
	}
	switch action {
	case "create", "start", "stop", "status", "wait", "kubeconfig":
	default:
		return fmt.Errorf("unknown %s command %q; run infestation %s --help", provider, action, provider)
	}
	f := flag.NewFlagSet(provider+" "+action, flag.ContinueOnError)
	f.SetOutput(out)
	f.Usage = func() {
		fmt.Fprintf(out, "Usage: infestation %s %s [flags]\n\n", provider, action)
		switch action {
		case "create":
			fmt.Fprintln(out, "Creates CockroachDB and K3s nodes and saves private provider state.")
			if provider == "vm" {
				fmt.Fprintln(out, "Run on the QEMU host that will hold the VM disks.")
			} else {
				fmt.Fprintln(out, "Requires Azure CLI login; saves resource identifiers and deployment inputs locally.")
			}
		case "start", "stop", "status":
			if provider == "vm" {
				fmt.Fprintln(out, "Run on the original QEMU host with its saved provider state, VM disks and local control sockets.")
			} else {
				fmt.Fprintln(out, "Requires saved provider state and Azure CLI login. Another machine can manage the VM only if that state is available there; resources are not discovered by name.")
			}
			if action == "start" && provider == "azure" {
				fmt.Fprintln(out, "Azure start currently ignores --wait; use azure wait separately for cluster readiness.")
			}
		case "wait", "kubeconfig":
			fmt.Fprintln(out, "Requires saved provider inventory and workstation tailnet access. Can run elsewhere with that inventory; Azure login is not required.")
		}
		fmt.Fprintln(out, "Use the same --name and --state-dir as create. State contents: docs/local-state.md.\n\nFlags:")
		f.PrintDefaults()
	}
	o := vm.ClusterOptions{Provider: "qemu"}
	cloud := provider == "azure"
	if cloud {
		o.Provider = "azure"
	}
	f.StringVar(&o.Name, "name", "", "provider group name (required; distinct in the tailnet)")
	f.StringVar(&o.StateDir, "state-dir", vm.DefaultStateDir(), "private local provider state")
	if action == "create" {
		f.IntVar(&o.Size, "size", 1, "number of nodes created on this provider")
		f.BoolVar(&o.Agent, "agent", false, "run a K3s agent and CockroachDB, without an API server")
		f.BoolVar(&o.Init, "init", false, "authorize the first server in this group to initialize the database")
		f.StringVar(&o.Domain, "tailnet-domain", "", "tailnet MagicDNS domain, such as example.ts.net")
		f.StringVar(&o.AuthKey, "tailscale-auth-key", "", "tag:roach auth key (default: TAILSCALE_AUTH_KEY)")
		f.StringVar(&o.AuthKeyFile, "tailscale-auth-key-file", "", "private auth key file")
		f.StringVar(&o.HelperSourceRef, "helper-source-ref", "", "build helper on the guest from this published GitHub tag/commit")
		f.StringVar(&o.HelperBuilderImage, "helper-builder-image", ignition.DefaultHelperBuilderImage, "pinned multiarchitecture Go builder")
		if cloud {
			f.StringVar(&o.AzureLocation, "region", azureconfig.DefaultRegion, "Azure region")
			f.StringVar(&o.AzureVMSize, "sku", azureconfig.DefaultSKU, "Azure VM SKU")
		} else {
			f.StringVar(&o.NodeHelper, "node-helper", "bin/infestation-node", "local Linux helper when not using --helper-source-ref")
		}
	}
	if !cloud && action == "create" {
		f.StringVar(&o.BaseImage, "base-image", "", "existing trusted Flatcar image; otherwise use the pinned image")
		f.StringVar(&o.Accel, "accel", "kvm", "QEMU accelerator: kvm or tcg")
		f.IntVar(&o.MemoryMiB, "memory-mib", 3072, "memory MiB per VM")
	}
	if !cloud && action == "start" {
		f.StringVar(&o.Accel, "accel", "", "override the stored QEMU accelerator")
	}
	if action == "create" || action == "start" || action == "wait" {
		wait := time.Duration(0)
		if action == "wait" {
			wait = 30 * time.Minute
		}
		f.DurationVar(&o.Wait, "wait", wait, "readiness deadline; 0 returns after provisioning/start; requires workstation tailnet access")
	}
	var destination string
	var force, netcheck bool
	if action == "kubeconfig" {
		f.StringVar(&destination, "out", "", "destination private file (default: provider state/kubeconfig)")
		f.BoolVar(&force, "force", false, "replace an existing destination kubeconfig")
	}
	if !cloud && action == "status" {
		f.BoolVar(&netcheck, "netcheck", false, "include the latest guest Tailscale network diagnostics")
	}
	if err := parse(f, args[2:]); err != nil {
		return help(err)
	}
	if o.Wait < 0 {
		return fmt.Errorf("--wait must not be negative")
	}
	if o.Init && o.Agent {
		return fmt.Errorf("--init requires an API server; it cannot be combined with --agent")
	}
	switch action {
	case "create":
		return vm.CreateCluster(ctx, o, out)
	case "wait":
		return vm.WaitCluster(ctx, o.Options, out)
	case "kubeconfig":
		return vm.Kubeconfig(ctx, o.Options, destination, force, out)
	case "status":
		if netcheck {
			return vm.GroupNetcheck(ctx, o.Options, out)
		}
	}
	if cloud {
		return vm.AzureAction(ctx, o.Options, action, out)
	}
	return vm.ClusterAction(ctx, o.Options, action, out)
}

func help(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}
func parse(f *flag.FlagSet, args []string) error {
	if err := f.Parse(args); err != nil {
		return err
	}
	if f.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	return nil
}
