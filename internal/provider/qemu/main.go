package qemu

import (
	"path/filepath"
	"strconv"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi/config"
)

func Run() {
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
	provider, err := newProvider(ctx, "local", pulumi.Map{"uri": pulumi.String("qemu:///system")})
	if err != nil {
		return err
	}
	providerOpt := pulumi.Provider(provider)
	pool, err := newResource(ctx, "pool", "Pool", "cluster", pulumi.Map{
		"name": pulumi.String(s.Name), "type": pulumi.String("dir"),
		"create": pulumi.Map{"build": pulumi.Bool(true), "start": pulumi.Bool(true), "autostart": pulumi.Bool(true)},
		"target": pulumi.Map{
			"path":        pulumi.String(s.PoolDir),
			"permissions": pulumi.Map{"mode": pulumi.String("0700"), "owner": pulumi.String(s.QEMUUserID), "group": pulumi.String(s.QEMUGroupID)},
		},
	}, providerOpt, pulumi.Protect(true))
	if err != nil {
		return err
	}
	hosts := pulumi.Array{}
	for _, node := range d.Cluster.Nodes {
		hosts = append(hosts, pulumi.Map{
			"name": pulumi.String(node.Name), "mac": pulumi.String(nodeMAC(s.Name, node.Name)), "ip": pulumi.String(node.IP),
		})
	}
	network, err := newResource(ctx, "network", "Network", "cluster", pulumi.Map{
		"name": pulumi.String(s.Name), "autostart": pulumi.Bool(true),
		"forward": pulumi.Map{"mode": pulumi.String("nat")},
		"dns":     pulumi.Map{"enable": pulumi.String("yes")},
		"ips": pulumi.Array{pulumi.Map{
			"address": pulumi.String(d.Gateway), "prefix": pulumi.Float64(d.Prefix), "family": pulumi.String("ipv4"),
			"dhcp": pulumi.Map{"hosts": hosts},
		}},
	}, providerOpt, pulumi.Protect(true))
	if err != nil {
		return err
	}
	permissions := func() pulumi.Map {
		return pulumi.Map{"mode": pulumi.String("0600"), "owner": pulumi.String(s.QEMUUserID), "group": pulumi.String(s.QEMUGroupID)}
	}
	base, err := newResource(ctx, "volume", "Volume", "base", pulumi.Map{
		"name": pulumi.String("flatcar-base.qcow2"), "pool": pool.Name,
		"capacity": pulumi.Float64(d.BaseSize),
		"create":   pulumi.Map{"content": pulumi.Map{"url": pulumi.String(s.BaseImagePath)}},
		"target":   pulumi.Map{"format": pulumi.Map{"type": pulumi.String("qcow2")}, "permissions": permissions()},
	}, providerOpt, pulumi.Protect(true), pulumi.IgnoreChanges([]string{"*"}))
	if err != nil {
		return err
	}
	privateIPs, apiEndpoints, domainNames := pulumi.StringMap{}, pulumi.StringMap{}, pulumi.StringMap{}
	for _, node := range d.Cluster.Nodes {
		// Upload existing private Ignition directly: the provider's Ignition helper
		// writes 0644 temporary files, unsuitable for our embedded private keys.
		ignition, err := newResource(ctx, "volume", "Volume", node.Name+"-ignition", pulumi.Map{
			"name": pulumi.String(node.Name + ".ign"), "pool": pool.Name,
			"create": pulumi.Map{"content": pulumi.Map{"url": pulumi.String(filepath.Join(s.IgnitionDir, node.Name+".ign"))}},
			"target": pulumi.Map{"format": pulumi.Map{"type": pulumi.String("raw")}, "permissions": permissions()},
		}, providerOpt, pulumi.Protect(true), pulumi.IgnoreChanges([]string{"*"}))
		if err != nil {
			return err
		}
		disk, err := newResource(ctx, "volume", "Volume", node.Name+"-disk", pulumi.Map{
			"name": pulumi.String(node.Name + ".qcow2"), "pool": pool.Name,
			"capacity": pulumi.Float64(uint64(s.DiskGiB) * 1024 * 1024 * 1024), "allocation": pulumi.Float64(0),
			"backingStore": pulumi.Map{"path": base.Path, "format": pulumi.Map{"type": pulumi.String("qcow2")}},
			"target":       pulumi.Map{"format": pulumi.Map{"type": pulumi.String("qcow2")}, "permissions": permissions()},
		}, providerOpt, pulumi.Protect(true), pulumi.IgnoreChanges([]string{"*"}))
		if err != nil {
			return err
		}
		domain, err := newResource(ctx, "domain", "Domain", node.Name, pulumi.Map{
			"name": pulumi.String(s.Name + "-" + node.Name), "type": pulumi.String("kvm"),
			"memory": pulumi.Float64(s.MemoryMiB), "memoryUnit": pulumi.String("MiB"), "vcpu": pulumi.Float64(s.VCPUs),
			"cpu":      pulumi.Map{"mode": pulumi.String("host-passthrough")},
			"os":       pulumi.Map{"type": pulumi.String("hvm"), "typeArch": pulumi.String("x86_64"), "typeMachine": pulumi.String("q35")},
			"features": pulumi.Map{"acpi": pulumi.Bool(true)},
			"running":  pulumi.Bool(s.Running), "autostart": pulumi.Bool(s.Autostart),
			"onReboot": pulumi.String("restart"), "onPoweroff": pulumi.String("destroy"), "onCrash": pulumi.String("destroy"),
			"destroy": pulumi.Map{"shutdown": pulumi.Map{"timeout": pulumi.Float64(120)}},
			"sysInfos": pulumi.Array{pulumi.Map{
				"fwCfg": pulumi.Map{"entries": pulumi.Array{pulumi.Map{
					"name": pulumi.String("opt/org.flatcar-linux/config"), "file": ignition.Path, "value": pulumi.String(""),
				}}},
			}},
			"devices": pulumi.Map{
				"disks": pulumi.Array{pulumi.Map{
					"device": pulumi.String("disk"), "driver": pulumi.Map{"name": pulumi.String("qemu"), "type": pulumi.String("qcow2")},
					"source": pulumi.Map{"file": pulumi.Map{"file": disk.Path}},
					"target": pulumi.Map{"dev": pulumi.String("vda"), "bus": pulumi.String("virtio")},
				}},
				"interfaces": pulumi.Array{pulumi.Map{
					"model":  pulumi.Map{"type": pulumi.String("virtio")},
					"mac":    pulumi.Map{"address": pulumi.String(nodeMAC(s.Name, node.Name))},
					"source": pulumi.Map{"network": pulumi.Map{"network": network.Name}},
				}},
				"serials": pulumi.Array{pulumi.Map{
					"source": pulumi.Map{"pty": pulumi.Map{"path": pulumi.String("")}},
					"target": pulumi.Map{"port": pulumi.Float64(0)},
				}},
				"consoles": pulumi.Array{pulumi.Map{
					"source": pulumi.Map{"pty": pulumi.Map{"path": pulumi.String("")}},
					"target": pulumi.Map{"type": pulumi.String("serial"), "port": pulumi.Float64(0)},
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
		domainNames[node.Name] = domain.Name
		if node.Role != "worker" {
			apiEndpoints[node.Name] = pulumi.String("https://" + node.IP + ":6443")
		}
	}
	ctx.Export("privateIPs", privateIPs)
	ctx.Export("domainNames", domainNames)
	ctx.Export("apiEndpoints", apiEndpoints)
	ctx.Export("networkCIDR", pulumi.String(s.NetworkCIDR))
	ctx.Export("memoryMiBTotal", pulumi.String(strconv.Itoa(len(d.Cluster.Nodes)*s.MemoryMiB)))
	return nil
}
