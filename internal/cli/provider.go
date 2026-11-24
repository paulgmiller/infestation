package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/paulgmiller/infestation/internal/vm"
	"github.com/spf13/cobra"
)

type provider struct{ name string }
type nodeAction func(context.Context, vm.ClusterOptions, io.Writer) error

func (p provider) command() *cobra.Command {
	cmd := &cobra.Command{Use: p.name, Short: "Manage " + p.name + " node groups", Args: noArgs, RunE: showHelp}
	cmd.AddCommand(p.create(), p.start(), p.stop(), p.remove(), p.status(), p.wait(), p.kubeconfig())
	if p.name == "vm" {
		upgrade, upgradeOptions := p.nodeCommand("upgrade", "Reset one owned QEMU server to this binary's pinned versions, retaining its state", func(ctx context.Context, o vm.ClusterOptions, out io.Writer) error {
			return vm.Upgrade(ctx, o.Options, out)
		})
		upgrade.Flags().StringVar(&upgradeOptions.Instance, "instance", "", "required zero-based owned instance; only this node is upgraded")
		_ = upgrade.MarkFlagRequired("instance")
		upgrade.Long += "\nRequires Kubernetes and CockroachDB drains. Uses retained-state Flatcar reset and an offline recovery copy. Does not decommission, certify surviving quorum, or restore snapshots automatically. Supports joining database servers; database version changes are not yet supported."
		cmd.AddCommand(upgrade)
		var enabled bool
		autostart, options := p.nodeCommand("autostart", "Set boot autostart policy on the QEMU compute host", func(ctx context.Context, o vm.ClusterOptions, out io.Writer) error {
			return vm.ConfigureAutostart(ctx, o.Options, enabled, out)
		})
		autostart.Long = "Change saved boot policy without starting VMs. Run on the actual compute host. Stopped VMs stay stopped until an explicit start. Create installs the user boot service by default; see docs/autostart.md."
		autostart.Flags().BoolVar(&enabled, "enable", true, "enable policy; use --enable=false to disable")
		autostart.Flags().StringVar(&options.Instance, "instance", "", "configure only this zero-based owned instance")
		var stateDir string
		resume := &cobra.Command{Use: "autostart-run", Short: "Resume opted-in local VMs after compute-host boot", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			return vm.ResumeLocal(cmd.Context(), vm.Options{StateDir: stateDir}, cmd.OutOrStdout())
		}}
		resume.Flags().StringVar(&stateDir, "state-dir", vm.DefaultStateDir(), "private host-local provider state")
		cmd.AddCommand(autostart, resume)
		reenroll, o := p.nodeCommand("reenroll", "Restore Tailscale access to running QEMU guests", func(ctx context.Context, o vm.ClusterOptions, out io.Writer) error {
			return vm.Reenroll(ctx, o.Options, out)
		})
		reenroll.Long = "Restore Tailscale access to running QEMU guests from their compute host using a temporary Flatcar guest-agent channel."
		reenroll.Flags().StringVar(&o.Instance, "instance", "", "re-enroll only this zero-based instance in the owned group")
		reenroll.Flags().StringVar(&o.AuthKeyFile, "tailscale-auth-key-file", "", "path to a tagged auth key (defaults to TAILSCALE_AUTH_KEY)")
		cmd.AddCommand(reenroll)
	}
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
		PreRunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("instance") && o.Instance == "" {
				return fmt.Errorf("--instance must be a nonnegative index")
			}
			if o.Instance != "" {
				if i, err := strconv.Atoi(o.Instance); err != nil || i < 0 {
					return fmt.Errorf("--instance must be a nonnegative index")
				}
			}
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
			return "Runs QEMU locally or on --ssh. Credentials and ownership state stay on this workstation; remote disks and node profiles stay on the SSH host."
		}
		return "Uses Azure SDK credentials (DefaultAzureCredential); requires --subscription or AZURE_SUBSCRIPTION_ID. Saves resource identifiers and private SDK inputs locally."
	}
	if action == "wait" || action == "kubeconfig" {
		return "Requires saved provider inventory and workstation tailnet access. Can run elsewhere with that inventory; Azure login is not required."
	}
	if p.name == "vm" {
		return "Uses saved provider state; SSH groups automatically use their original SSH host. Local groups require the original QEMU host."
	}
	text := "Requires saved provider state and Azure SDK credentials (DefaultAzureCredential). Another machine can manage the VM only if that state is available there; resources are not discovered by name."
	if action == "start" {
		text += "\nAzure start currently ignores --wait; use azure wait separately for cluster readiness."
	}
	return text
}

func (p provider) runAction(ctx context.Context, o vm.ClusterOptions, action string, out io.Writer) error {
	compute, err := vm.ProviderFor(o.Provider)
	if err != nil {
		return err
	}
	switch action {
	case "start":
		return compute.Start(ctx, o.Options, out)
	case "stop":
		return compute.Stop(ctx, o.Options, out)
	case "status":
		return compute.Status(ctx, o.Options, out)
	default:
		return fmt.Errorf("unknown compute action %q", action)
	}
}

func (p provider) start() *cobra.Command {
	cmd, o := p.nodeCommand("start", "Start saved VMs without replacing disks or identity", func(ctx context.Context, o vm.ClusterOptions, out io.Writer) error {
		return p.runAction(ctx, o, "start", out)
	})
	cmd.Flags().DurationVar(&o.Wait, "wait", 0, "readiness deadline; 0 returns after starting")
	if p.name == "vm" {
		cmd.Flags().StringVar(&o.Accel, "accel", "", "override the stored QEMU accelerator")
		cmd.Flags().StringVar(&o.Instance, "instance", "", "start only this zero-based instance in the owned group")
	}
	return cmd
}
func (p provider) stop() *cobra.Command {
	var all bool
	cmd, o := p.nodeCommand("stop", "Stop owned VMs while preserving disks and identity", func(ctx context.Context, o vm.ClusterOptions, out io.Writer) error {
		if all {
			return vm.StopAllLocal(ctx, o.Options, out)
		}
		return p.runAction(ctx, o, "stop", out)
	})
	cmd.Flags().BoolVar(&o.AllowOutage, "allow-outage", false, "permit power-off if a database drain cannot be confirmed")
	if p.name == "vm" {
		// --all replaces --name for host-local shutdown only.
		delete(cmd.Flags().Lookup("name").Annotations, cobra.BashCompOneRequiredFlag)
		cmd.Flags().BoolVar(&all, "all", false, "stop every owned local VM under --state-dir; skip SSH and Azure groups")
		cmd.MarkFlagsOneRequired("name", "all")
		cmd.MarkFlagsMutuallyExclusive("name", "all")
		cmd.Flags().StringVar(&o.Instance, "instance", "", "stop only this zero-based instance in the owned group")
		cmd.MarkFlagsMutuallyExclusive("all", "instance")
		cmd.Long += "\nUse --all --allow-outage to gracefully shut down all local VMs discovered under --state-dir, across named groups. SSH-backed groups and Azure resources are excluded. Disks and private state are retained."
	}
	return cmd
}
func (p provider) remove() *cobra.Command {
	var removal vm.RemovalOptions
	description := "Terminate owned QEMU VMs and clean control files; preserve disks and private state"
	if p.name == "azure" {
		description = "Delete the owned Azure resource group, including VMSS, disks and networking; preserve local state"
	}
	cmd, _ := p.nodeCommand("remove", description, func(ctx context.Context, o vm.ClusterOptions, out io.Writer) error {
		compute, err := vm.ProviderFor(o.Provider)
		if err != nil {
			return err
		}
		return compute.Remove(ctx, o.Options, removal, out)
	})
	if p.name == "vm" {
		cmd.Flags().BoolVar(&removal.DeleteDisks, "delete-disks", false, "also permanently delete owned QEMU disk overlays (base images and private state are retained)")
	}
	cmd.Flags().BoolVar(&removal.AbandonCluster, "abandon-cluster", false, "explicitly skip database safety checks for teardown of an abandoned cluster (may lose data)")
	cmd.Flags().DurationVar(&removal.Timeout, "decommission-timeout", 30*time.Minute, "deadline for verified database evacuation before deleting compute")
	cmd.Long += "\nNormal removal asks one verified guest to retire the entire database batch using its existing node credentials over Tailscale SSH. It requires a current retirement helper on every target, strict decommissioning and healthy replacement capacity. Use --abandon-cluster only for intentional destructive teardown. Kubernetes workload draining and cleanup of tailnet registrations remain unimplemented."
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
