package main

import (
	"path/filepath"
	"strconv"

	"github.com/pulumi/pulumi-terraform-provider/sdks/go/libvirt/libvirt"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

func main() {
	pulumi.Run(func(ctx *pulumi.Context) error {
		cfg := config.New(ctx, "")
		get := func(key, fallback string) string {
			if value := cfg.Get(key); value != "" {
				return value
			}
			return fallback
		}
		getInt := func(key string, fallback int) int {
			if cfg.Get(key) != "" {
				return cfg.RequireInt(key)
			}
			return fallback
		}
		name := get("name", "flatcar-"+ctx.Stack())
		s := Settings{
			Name: name, ClusterPath: get("clusterPath", "cluster.local.json"), IgnitionDir: get("ignitionDir", "state/output"),
			BaseImagePath: cfg.Require("baseImagePath"), PoolDir: get("poolDir", "/var/lib/libvirt/images/"+name),
			NetworkCIDR: get("networkCIDR", "192.168.124.0/24"), QEMUUserID: cfg.Require("qemuUserID"), QEMUGroupID: cfg.Require("qemuGroupID"),
			MemoryMiB: getInt("memoryMiB", 3072), VCPUs: getInt("vcpus", 2), DiskGiB: getInt("diskGiB", 40),
			Running: true, Autostart: cfg.GetBool("autostart"),
		}
		if cfg.Get("running") != "" {
			s.Running = cfg.RequireBool("running")
		}
		d, err := loadDeployment(s)
		if err != nil {
			return err
		}
		return deploy(ctx, d)
	})
}

func deploy(ctx *pulumi.Context, d *Deployment) error {
	s := d.Settings
	provider, err := libvirt.NewProvider(ctx, "local", &libvirt.ProviderArgs{Uri: pulumi.String("qemu:///system")})
	if err != nil {
		return err
	}
	providerOpt := pulumi.Provider(provider)
	pool, err := libvirt.NewPool(ctx, "cluster", &libvirt.PoolArgs{
		Name: pulumi.String(s.Name), Type: pulumi.String("dir"),
		Create: &libvirt.PoolCreateArgs{Build: pulumi.Bool(true), Start: pulumi.Bool(true), Autostart: pulumi.Bool(true)},
		Target: &libvirt.PoolTargetArgs{
			Path:        pulumi.String(s.PoolDir),
			Permissions: &libvirt.PoolTargetPermissionsArgs{Mode: pulumi.String("0700"), Owner: pulumi.String(s.QEMUUserID), Group: pulumi.String(s.QEMUGroupID)},
		},
	}, providerOpt, pulumi.Protect(true))
	if err != nil {
		return err
	}
	hosts := libvirt.NetworkIpDhcpHostArray{}
	for _, node := range d.Cluster.Nodes {
		hosts = append(hosts, &libvirt.NetworkIpDhcpHostArgs{
			Name: pulumi.String(node.Name), Mac: pulumi.String(nodeMAC(s.Name, node.Name)), Ip: pulumi.String(node.IP),
		})
	}
	network, err := libvirt.NewNetwork(ctx, "cluster", &libvirt.NetworkArgs{
		Name: pulumi.String(s.Name), Autostart: pulumi.Bool(true),
		Forward: &libvirt.NetworkForwardArgs{Mode: pulumi.String("nat")},
		Dns:     &libvirt.NetworkDnsArgs{Enable: pulumi.String("yes")},
		Ips: libvirt.NetworkIpArray{&libvirt.NetworkIpArgs{
			Address: pulumi.String(d.Gateway), Prefix: pulumi.Float64(d.Prefix), Family: pulumi.String("ipv4"),
			Dhcp: &libvirt.NetworkIpDhcpArgs{Hosts: hosts},
		}},
	}, providerOpt, pulumi.Protect(true))
	if err != nil {
		return err
	}
	permissions := func() *libvirt.VolumeTargetPermissionsArgs {
		return &libvirt.VolumeTargetPermissionsArgs{Mode: pulumi.String("0600"), Owner: pulumi.String(s.QEMUUserID), Group: pulumi.String(s.QEMUGroupID)}
	}
	base, err := libvirt.NewVolume(ctx, "base", &libvirt.VolumeArgs{
		Name: pulumi.String("flatcar-base.qcow2"), Pool: pool.Name,
		Capacity: pulumi.Float64(d.BaseSize),
		Create:   &libvirt.VolumeCreateArgs{Content: &libvirt.VolumeCreateContentArgs{Url: pulumi.String(s.BaseImagePath)}},
		Target:   &libvirt.VolumeTargetArgs{Format: &libvirt.VolumeTargetFormatArgs{Type: pulumi.String("qcow2")}, Permissions: permissions()},
	}, providerOpt, pulumi.Protect(true), pulumi.IgnoreChanges([]string{"*"}))
	if err != nil {
		return err
	}
	privateIPs, sshCommands, apiEndpoints, domainNames := pulumi.StringMap{}, pulumi.StringMap{}, pulumi.StringMap{}, pulumi.StringMap{}
	for _, node := range d.Cluster.Nodes {
		// Upload existing private Ignition directly: the provider's Ignition helper
		// writes 0644 temporary files, unsuitable for our embedded private keys.
		ignition, err := libvirt.NewVolume(ctx, node.Name+"-ignition", &libvirt.VolumeArgs{
			Name: pulumi.String(node.Name + ".ign"), Pool: pool.Name,
			Create: &libvirt.VolumeCreateArgs{Content: &libvirt.VolumeCreateContentArgs{Url: pulumi.String(filepath.Join(s.IgnitionDir, node.Name+".ign"))}},
			Target: &libvirt.VolumeTargetArgs{Format: &libvirt.VolumeTargetFormatArgs{Type: pulumi.String("raw")}, Permissions: permissions()},
		}, providerOpt, pulumi.Protect(true), pulumi.IgnoreChanges([]string{"*"}))
		if err != nil {
			return err
		}
		disk, err := libvirt.NewVolume(ctx, node.Name+"-disk", &libvirt.VolumeArgs{
			Name: pulumi.String(node.Name + ".qcow2"), Pool: pool.Name,
			Capacity: pulumi.Float64(uint64(s.DiskGiB) * 1024 * 1024 * 1024), Allocation: pulumi.Float64(0),
			BackingStore: &libvirt.VolumeBackingStoreArgs{Path: base.Path, Format: &libvirt.VolumeBackingStoreFormatArgs{Type: pulumi.String("qcow2")}},
			Target:       &libvirt.VolumeTargetArgs{Format: &libvirt.VolumeTargetFormatArgs{Type: pulumi.String("qcow2")}, Permissions: permissions()},
		}, providerOpt, pulumi.Protect(true), pulumi.IgnoreChanges([]string{"*"}))
		if err != nil {
			return err
		}
		domain, err := libvirt.NewDomain(ctx, node.Name, &libvirt.DomainArgs{
			Name: pulumi.String(s.Name + "-" + node.Name), Type: pulumi.String("kvm"),
			Memory: pulumi.Float64(s.MemoryMiB), MemoryUnit: pulumi.String("MiB"), Vcpu: pulumi.Float64(s.VCPUs),
			Cpu:      &libvirt.DomainCpuArgs{Mode: pulumi.String("host-passthrough")},
			Os:       &libvirt.DomainOsArgs{Type: pulumi.String("hvm"), TypeArch: pulumi.String("x86_64"), TypeMachine: pulumi.String("q35")},
			Features: &libvirt.DomainFeaturesArgs{Acpi: pulumi.Bool(true)},
			Running:  pulumi.Bool(s.Running), Autostart: pulumi.Bool(s.Autostart),
			OnReboot: pulumi.String("restart"), OnPoweroff: pulumi.String("destroy"), OnCrash: pulumi.String("destroy"),
			Destroy: &libvirt.DomainDestroyArgs{Shutdown: &libvirt.DomainDestroyShutdownArgs{Timeout: pulumi.Float64(120)}},
			SysInfos: libvirt.DomainSysInfoArray{&libvirt.DomainSysInfoArgs{
				FwCfg: &libvirt.DomainSysInfoFwCfgArgs{Entries: libvirt.DomainSysInfoFwCfgEntryArray{&libvirt.DomainSysInfoFwCfgEntryArgs{
					Name: pulumi.String("opt/org.flatcar-linux/config"), File: ignition.Path, Value: pulumi.String(""),
				}}},
			}},
			Devices: &libvirt.DomainDevicesArgs{
				Disks: libvirt.DomainDevicesDiskArray{&libvirt.DomainDevicesDiskArgs{
					Device: pulumi.String("disk"), Driver: &libvirt.DomainDevicesDiskDriverArgs{Name: pulumi.String("qemu"), Type: pulumi.String("qcow2")},
					Source: &libvirt.DomainDevicesDiskSourceArgs{File: &libvirt.DomainDevicesDiskSourceFileArgs{File: disk.Path}},
					Target: &libvirt.DomainDevicesDiskTargetArgs{Dev: pulumi.String("vda"), Bus: pulumi.String("virtio")},
				}},
				Interfaces: libvirt.DomainDevicesInterfaceArray{&libvirt.DomainDevicesInterfaceArgs{
					Model:  &libvirt.DomainDevicesInterfaceModelArgs{Type: pulumi.String("virtio")},
					Mac:    &libvirt.DomainDevicesInterfaceMacArgs{Address: pulumi.String(nodeMAC(s.Name, node.Name))},
					Source: &libvirt.DomainDevicesInterfaceSourceArgs{Network: &libvirt.DomainDevicesInterfaceSourceNetworkArgs{Network: network.Name}},
				}},
				Serials: libvirt.DomainDevicesSerialArray{&libvirt.DomainDevicesSerialArgs{
					Source: &libvirt.DomainDevicesSerialSourceArgs{Pty: &libvirt.DomainDevicesSerialSourcePtyArgs{Path: pulumi.String("")}},
					Target: &libvirt.DomainDevicesSerialTargetArgs{Port: pulumi.Float64(0)},
				}},
				Consoles: libvirt.DomainDevicesConsoleArray{&libvirt.DomainDevicesConsoleArgs{
					Source: &libvirt.DomainDevicesConsoleSourceArgs{Pty: &libvirt.DomainDevicesConsoleSourcePtyArgs{Path: pulumi.String("")}},
					Target: &libvirt.DomainDevicesConsoleTargetArgs{Type: pulumi.String("serial"), Port: pulumi.Float64(0)},
				}},
			},
		}, providerOpt, pulumi.Protect(true), pulumi.IgnoreChanges([]string{
			"devices.serials[0].source.pty.path",
			"devices.consoles[0].source.pty.path",
		}))
		if err != nil {
			return err
		}
		privateIPs[node.Name] = pulumi.String(node.IP)
		sshCommands[node.Name] = pulumi.String("ssh core@" + node.IP)
		domainNames[node.Name] = domain.Name
		if node.Role != "worker" {
			apiEndpoints[node.Name] = pulumi.String("https://" + node.IP + ":6443")
		}
	}
	ctx.Export("privateIPs", privateIPs)
	ctx.Export("sshCommands", sshCommands)
	ctx.Export("domainNames", domainNames)
	ctx.Export("apiEndpoints", apiEndpoints)
	ctx.Export("networkCIDR", pulumi.String(s.NetworkCIDR))
	ctx.Export("memoryMiBTotal", pulumi.String(strconv.Itoa(len(d.Cluster.Nodes)*s.MemoryMiB)))
	return nil
}
