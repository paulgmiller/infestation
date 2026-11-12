package cli

import (
	"github.com/spf13/cobra"
	"infestation/internal/azureconfig"
	"infestation/internal/ignition"
	"infestation/internal/vm"
)

func (p provider) create() *cobra.Command {
	cmd, o := p.nodeCommand("create", "Create nodes running CockroachDB and K3s", vm.CreateCluster)
	f := cmd.Flags()
	f.IntVar(&o.Size, "size", 1, "number of nodes created on this provider")
	f.BoolVar(&o.Agent, "agent", false, "run a K3s agent and CockroachDB, without an API server")
	f.BoolVar(&o.Init, "init", false, "authorize the first server in this group to initialize the database")
	f.StringVar(&o.Domain, "tailnet-domain", "", "tailnet MagicDNS domain, such as example.ts.net")
	f.StringVar(&o.AuthKey, "tailscale-auth-key", "", "tag:roach auth key (default: TAILSCALE_AUTH_KEY)")
	f.StringVar(&o.AuthKeyFile, "tailscale-auth-key-file", "", "private auth key file")
	f.StringVar(&o.HelperSourceRef, "helper-source-ref", ignition.DefaultHelperSourceRef, "helper source branch, tag or commit; resolved once before provisioning")
	f.StringVar(&o.HelperBuilderImage, "helper-builder-image", ignition.DefaultHelperBuilderImage, "pinned multiarchitecture Go builder")
	f.DurationVar(&o.Wait, "wait", 0, "readiness deadline; 0 returns after provisioning; requires workstation tailnet access")
	if p.name == "azure" {
		f.StringVar(&o.AzureLocation, "region", azureconfig.DefaultRegion, "Azure region")
		f.StringVar(&o.AzureVMSize, "sku", azureconfig.DefaultSKU, "Azure VM SKU")
	} else {
		f.StringVar(&o.BaseImage, "base-image", "", "existing trusted Flatcar image; otherwise use the pinned image")
		f.StringVar(&o.Accel, "accel", "kvm", "QEMU accelerator: kvm or tcg")
		f.IntVar(&o.MemoryMiB, "memory-mib", 3072, "memory MiB per VM")
	}
	return cmd
}
