package cli

import (
	"github.com/paulgmiller/infestation/internal/ignition"
	"github.com/paulgmiller/infestation/internal/vm"
	"github.com/spf13/cobra"
)

func proxmoxCommand() *cobra.Command {
	root := &cobra.Command{Use: "proxmox", Short: "Export a joining node for manual Proxmox import", Args: noArgs, RunE: showHelp}
	o := vm.ClusterOptions{}
	var destination string
	cmd := &cobra.Command{Use: "export", Short: "Export a VHD and private enrollment profile for manual Proxmox setup", Args: noArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return vm.ExportProxmox(cmd.Context(), o, destination, cmd.OutOrStdout())
	}}
	f := cmd.Flags()
	f.StringVar(&o.Name, "name", "", "unique joining node hostname (required)")
	f.StringVar(&destination, "out-dir", "", "new private export directory (required; never overwritten)")
	f.StringVar(&o.StateDir, "state-dir", vm.DefaultStateDir(), "image cache parent")
	f.StringVar(&o.BaseImage, "base-image", "", "trusted never-booted standalone Flatcar QEMU qcow2; otherwise download pinned image")
	f.StringVar(&o.Domain, "tailnet-domain", "", "tailnet MagicDNS domain (default: discover locally)")
	f.StringVar(&o.AuthKeyFile, "tailscale-auth-key-file", "", "private tag:roach enrollment key file (or set TAILSCALE_AUTH_KEY)")
	f.StringVar(&o.HelperSourceRef, "helper-source-ref", ignition.DefaultHelperSourceRef, "helper source branch, tag or commit; resolved once")
	f.StringVar(&o.HelperBuilderImage, "helper-builder-image", ignition.DefaultHelperBuilderImage, "pinned multiarchitecture Go builder")
	f.StringVar(&o.Host, "host", "", "stable Proxmox physical host label")
	f.StringVar(&o.Site, "site", "", "shared physical site label")
	f.BoolVar(&o.Agent, "agent", false, "join as a K3s agent with CockroachDB")
	_ = cmd.MarkFlagRequired("name")
	_ = cmd.MarkFlagRequired("out-dir")
	root.AddCommand(cmd)
	return root
}
