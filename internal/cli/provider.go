package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/paulgmiller/infestation/internal/vm"
	"github.com/spf13/cobra"
)

type provider struct{ name string }
type nodeAction func(context.Context, vm.ClusterOptions, io.Writer) error

func (p provider) command() *cobra.Command {
	cmd := &cobra.Command{Use: p.name, Short: "Manage " + p.name + " node groups", Args: noArgs, RunE: showHelp}
	cmd.AddCommand(p.create(), p.start(), p.stop(), p.remove(), p.status(), p.wait(), p.kubeconfig())
	return cmd
}

// Each command owns its options so flag defaults cannot leak between actions.
func (p provider) nodeCommand(action, description string, run nodeAction) (*cobra.Command, *vm.ClusterOptions) {
	o := &vm.ClusterOptions{Provider: "qemu"}
	if p.name == "azure" {
		o.Provider = "azure"
	}
	cmd := &cobra.Command{
		Use:   action,
		Short: description,
		Long:  description + ".\n\n" + p.requirements(action) + "\nUse the same --name and --state-dir as create. State contents: docs/local-state.md.",
		Args:  noArgs,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			if o.Wait < 0 {
				return fmt.Errorf("--wait must not be negative")
			}
			if o.Init && o.Agent {
				return fmt.Errorf("--init requires an API server; it cannot be combined with --agent")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error { return run(cmd.Context(), *o, cmd.OutOrStdout()) },
	}
	cmd.Flags().StringVar(&o.Name, "name", "", "provider group name (required; distinct in the tailnet)")
	cmd.Flags().StringVar(&o.StateDir, "state-dir", vm.DefaultStateDir(), "private local provider state")
	_ = cmd.MarkFlagRequired("name")
	return cmd, o
}

func (p provider) requirements(action string) string {
	if action == "create" {
		if p.name == "vm" {
			return "Run on the QEMU host that will hold the VM disks. Saves private provider state."
		}
		return "Requires Azure CLI login; saves resource identifiers and deployment inputs locally."
	}
	if action == "wait" || action == "kubeconfig" {
		return "Requires saved provider inventory and workstation tailnet access. Can run elsewhere with that inventory; Azure login is not required."
	}
	if p.name == "vm" {
		return "Run on the original QEMU host with its saved provider state, VM disks and local control sockets."
	}
	text := "Requires saved provider state and Azure CLI login. Another machine can manage the VM only if that state is available there; resources are not discovered by name."
	if action == "start" {
		text += "\nAzure start currently ignores --wait; use azure wait separately for cluster readiness."
	}
	return text
}

func (p provider) runAction(ctx context.Context, o vm.ClusterOptions, action string, out io.Writer) error {
	if p.name == "azure" {
		return vm.AzureAction(ctx, o.Options, action, out)
	}
	return vm.ClusterAction(ctx, o.Options, action, out)
}

func (p provider) start() *cobra.Command {
	cmd, o := p.nodeCommand("start", "Start saved VMs without replacing disks or identity", func(ctx context.Context, o vm.ClusterOptions, out io.Writer) error {
		return p.runAction(ctx, o, "start", out)
	})
	cmd.Flags().DurationVar(&o.Wait, "wait", 0, "readiness deadline; 0 returns after starting")
	if p.name == "vm" {
		cmd.Flags().StringVar(&o.Accel, "accel", "", "override the stored QEMU accelerator")
	}
	return cmd
}
func (p provider) stop() *cobra.Command {
	cmd, _ := p.nodeCommand("stop", "Stop owned VMs while preserving disks and identity", func(ctx context.Context, o vm.ClusterOptions, out io.Writer) error {
		return p.runAction(ctx, o, "stop", out)
	})
	return cmd
}
func (p provider) remove() *cobra.Command {
	var deleteDisks bool
	description := "Terminate owned QEMU VMs and clean control files; preserve disks and private state"
	if p.name == "azure" {
		description = "Delete the owned Azure resource group, including VMSS, disks and networking; preserve local state"
	}
	cmd, _ := p.nodeCommand("remove", description, func(ctx context.Context, o vm.ClusterOptions, out io.Writer) error {
		return vm.RemoveGroup(ctx, o.Options, o.Provider, deleteDisks, out)
	})
	if p.name == "vm" {
		cmd.Flags().BoolVar(&deleteDisks, "delete-disks", false, "also permanently delete owned QEMU disk overlays (base images and private state are retained)")
	}
	cmd.Long += "\nRemoval does not drain cluster members or remove tailnet registrations."
	return cmd
}
func (p provider) status() *cobra.Command {
	var netcheck bool
	cmd, _ := p.nodeCommand("status", "Show provider power state and available diagnostics", func(ctx context.Context, o vm.ClusterOptions, out io.Writer) error {
		if netcheck {
			return vm.GroupNetcheck(ctx, o.Options, out)
		}
		return p.runAction(ctx, o, "status", out)
	})
	if p.name == "vm" {
		cmd.Flags().BoolVar(&netcheck, "netcheck", false, "include latest guest Tailscale network diagnostics")
	}
	return cmd
}
func (p provider) wait() *cobra.Command {
	cmd, o := p.nodeCommand("wait", "Wait for Kubernetes readiness and save kubeconfig", func(ctx context.Context, o vm.ClusterOptions, out io.Writer) error {
		return vm.WaitCluster(ctx, o.Options, out)
	})
	cmd.Flags().DurationVar(&o.Wait, "wait", 30*time.Minute, "readiness deadline")
	return cmd
}
func (p provider) kubeconfig() *cobra.Command {
	var destination string
	var force bool
	cmd, _ := p.nodeCommand("kubeconfig", "Retrieve admin kubeconfig to a private local file", func(ctx context.Context, o vm.ClusterOptions, out io.Writer) error {
		return vm.Kubeconfig(ctx, o.Options, destination, force, out)
	})
	cmd.Flags().StringVar(&destination, "out", "", "destination private file (default: provider state/kubeconfig)")
	cmd.Flags().BoolVar(&force, "force", false, "replace an existing destination kubeconfig")
	return cmd
}
