package main

import (
	"encoding/base64"
	"sort"

	"github.com/pulumi/pulumi-azure-native-sdk/compute/v3"
	"github.com/pulumi/pulumi-azure-native-sdk/network/v3"
	"github.com/pulumi/pulumi-azure-native-sdk/resources/v3"
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
		s := Settings{
			Name: get("name", "flatcar-"+ctx.Stack()), Location: get("location", "westus2"),
			FlatcarVersion: cfg.Require("flatcarVersion"), VMSize: get("vmSize", "Standard_D4s_v5"),
			ClusterPath: get("clusterPath", "cluster.local.json"), IgnitionDir: get("ignitionDir", "state/output"),
			VNetCIDR: get("vnetCIDR", "192.168.0.0/16"), SubnetCIDR: get("subnetCIDR", "192.168.1.0/24"),
			OSDiskSizeGB: 128, PublicNodeIPs: cfg.GetBool("publicNodeIPs"),
		}
		if err := cfg.GetObject("nodeZones", &s.NodeZones); err != nil {
			return err
		}
		if err := cfg.GetObject("adminCIDRs", &s.AdminCIDRs); err != nil {
			return err
		}
		if cfg.Get("osDiskSizeGB") != "" {
			s.OSDiskSizeGB = cfg.RequireInt("osDiskSizeGB")
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
	location := pulumi.String(s.Location)
	rg, err := resources.NewResourceGroup(ctx, "cluster", &resources.ResourceGroupArgs{
		ResourceGroupName: pulumi.String(s.Name + "-rg"), Location: location,
	})
	if err != nil {
		return err
	}
	vnet, err := network.NewVirtualNetwork(ctx, "cluster", &network.VirtualNetworkArgs{
		VirtualNetworkName: pulumi.String(s.Name + "-vnet"), ResourceGroupName: rg.Name, Location: location,
		AddressSpace: &network.AddressSpaceArgs{AddressPrefixes: pulumi.ToStringArray([]string{s.VNetCIDR})},
	})
	if err != nil {
		return err
	}
	publicIP := func(name string) (*network.PublicIPAddress, error) {
		return network.NewPublicIPAddress(ctx, name, &network.PublicIPAddressArgs{
			PublicIpAddressName: pulumi.String(s.Name + "-" + name), ResourceGroupName: rg.Name, Location: location,
			Sku:                      &network.PublicIPAddressSkuArgs{Name: pulumi.String("Standard")},
			PublicIPAllocationMethod: pulumi.String("Static"),
		})
	}
	egress, err := publicIP("egress")
	if err != nil {
		return err
	}
	nat, err := network.NewNatGateway(ctx, "egress", &network.NatGatewayArgs{
		NatGatewayName: pulumi.String(s.Name + "-nat"), ResourceGroupName: rg.Name, Location: location,
		Sku:               &network.NatGatewaySkuArgs{Name: pulumi.String("Standard")},
		PublicIpAddresses: network.SubResourceArray{&network.SubResourceArgs{Id: egress.ID()}},
	})
	if err != nil {
		return err
	}
	rules := network.SecurityRuleTypeArray{
		&network.SecurityRuleTypeArgs{
			Name: pulumi.String("cluster-internal"), Priority: pulumi.Int(100), Direction: pulumi.String("Inbound"),
			Access: pulumi.String("Allow"), Protocol: pulumi.String("*"), SourcePortRange: pulumi.String("*"),
			DestinationPortRange: pulumi.String("*"), SourceAddressPrefix: pulumi.String(s.SubnetCIDR), DestinationAddressPrefix: pulumi.String(s.SubnetCIDR),
		},
		&network.SecurityRuleTypeArgs{
			Name: pulumi.String("deny-other-inbound"), Priority: pulumi.Int(200), Direction: pulumi.String("Inbound"),
			Access: pulumi.String("Deny"), Protocol: pulumi.String("*"), SourcePortRange: pulumi.String("*"),
			DestinationPortRange: pulumi.String("*"), SourceAddressPrefix: pulumi.String("*"), DestinationAddressPrefix: pulumi.String("*"),
		},
	}
	if len(s.AdminCIDRs) > 0 {
		rules = append(rules, &network.SecurityRuleTypeArgs{
			Name: pulumi.String("management-ssh"), Priority: pulumi.Int(110), Direction: pulumi.String("Inbound"),
			Access: pulumi.String("Allow"), Protocol: pulumi.String("Tcp"), SourcePortRange: pulumi.String("*"),
			DestinationPortRange: pulumi.String("22"), SourceAddressPrefixes: pulumi.ToStringArray(s.AdminCIDRs), DestinationAddressPrefix: pulumi.String("*"),
		})
	}
	nsg, err := network.NewNetworkSecurityGroup(ctx, "nodes", &network.NetworkSecurityGroupArgs{
		NetworkSecurityGroupName: pulumi.String(s.Name + "-nodes"), ResourceGroupName: rg.Name, Location: location, SecurityRules: rules,
	})
	if err != nil {
		return err
	}
	subnet, err := network.NewSubnet(ctx, "nodes", &network.SubnetArgs{
		SubnetName: pulumi.String("nodes"), ResourceGroupName: rg.Name, VirtualNetworkName: vnet.Name,
		AddressPrefix: pulumi.String(s.SubnetCIDR), DefaultOutboundAccess: pulumi.Bool(false),
		NatGateway: &network.SubResourceArgs{Id: nat.ID()}, NetworkSecurityGroup: &network.NetworkSecurityGroupTypeArgs{Id: nsg.ID()},
	})
	if err != nil {
		return err
	}
	zoneSet := map[string]bool{}
	for _, zone := range s.NodeZones {
		zoneSet[zone] = true
	}
	zones := make([]string, 0, len(zoneSet))
	for zone := range zoneSet {
		zones = append(zones, zone)
	}
	sort.Strings(zones)
	vmss, err := compute.NewVirtualMachineScaleSet(ctx, "cluster", &compute.VirtualMachineScaleSetArgs{
		VmScaleSetName: pulumi.String(s.Name + "-vmss"), ResourceGroupName: rg.Name, Location: location,
		OrchestrationMode: pulumi.String("Flexible"), PlatformFaultDomainCount: pulumi.Int(1),
		SinglePlacementGroup: pulumi.Bool(false), Zones: pulumi.ToStringArray(zones),
		// Each VM has its own configuration; there is no shared scaling profile.
	})
	if err != nil {
		return err
	}
	privateIPs, publicIPs, apiEndpoints := pulumi.StringMap{}, pulumi.StringMap{}, pulumi.StringMap{}
	for _, node := range d.Cluster.Nodes {
		ipConfig := &network.NetworkInterfaceIPConfigurationArgs{
			Name: pulumi.String("primary"), Primary: pulumi.Bool(true),
			PrivateIPAddress: pulumi.String(node.IP), PrivateIPAllocationMethod: pulumi.String("Static"),
			Subnet: &network.SubnetTypeArgs{Id: subnet.ID()},
		}
		if s.PublicNodeIPs {
			ip, err := publicIP("node-" + node.Name)
			if err != nil {
				return err
			}
			ipConfig.PublicIPAddress = &network.PublicIPAddressTypeArgs{Id: ip.ID()}
			publicIPs[node.Name] = ip.IpAddress.Elem()
		}
		nic, err := network.NewNetworkInterface(ctx, "node-"+node.Name, &network.NetworkInterfaceArgs{
			NetworkInterfaceName: pulumi.String(s.Name + "-" + node.Name), ResourceGroupName: rg.Name, Location: location,
			EnableIPForwarding: pulumi.Bool(true), IpConfigurations: network.NetworkInterfaceIPConfigurationArray{ipConfig},
		})
		if err != nil {
			return err
		}
		_, err = compute.NewVirtualMachine(ctx, "node-"+node.Name, &compute.VirtualMachineArgs{
			VmName: pulumi.String(s.Name + "-" + node.Name), ResourceGroupName: rg.Name, Location: location,
			Zones:                  pulumi.ToStringArray([]string{s.NodeZones[node.Name]}),
			VirtualMachineScaleSet: &compute.SubResourceArgs{Id: vmss.ID()},
			HardwareProfile:        &compute.HardwareProfileArgs{VmSize: pulumi.String(s.VMSize)},
			NetworkProfile:         &compute.NetworkProfileArgs{NetworkInterfaces: compute.NetworkInterfaceReferenceArray{&compute.NetworkInterfaceReferenceArgs{Id: nic.ID(), Primary: pulumi.Bool(true)}}},
			OsProfile: &compute.OSProfileArgs{
				ComputerName: pulumi.String(node.Name), AdminUsername: pulumi.String("core"),
				CustomData: pulumi.ToSecret(pulumi.String(base64.StdEncoding.EncodeToString(d.Ignition[node.Name]))).(pulumi.StringOutput),
				LinuxConfiguration: &compute.LinuxConfigurationArgs{
					DisablePasswordAuthentication: pulumi.Bool(true),
					Ssh: &compute.SshConfigurationArgs{PublicKeys: compute.SshPublicKeyTypeArray{&compute.SshPublicKeyTypeArgs{
						Path: pulumi.String("/home/core/.ssh/authorized_keys"), KeyData: pulumi.String(d.Cluster.SSHAuthorizedKey),
					}}},
				},
			},
			StorageProfile: &compute.StorageProfileArgs{
				ImageReference: &compute.ImageReferenceArgs{Publisher: pulumi.String("kinvolk"), Offer: pulumi.String("flatcar-container-linux-free"), Sku: pulumi.String("stable-gen2"), Version: pulumi.String(s.FlatcarVersion)},
				OsDisk: &compute.OSDiskArgs{
					Name: pulumi.String(s.Name + "-" + node.Name + "-os"), CreateOption: pulumi.String("FromImage"),
					DiskSizeGB: pulumi.Int(s.OSDiskSizeGB), Caching: compute.CachingTypesReadWrite, DeleteOption: pulumi.String("Detach"),
					ManagedDisk: &compute.ManagedDiskParametersArgs{StorageAccountType: pulumi.String("Premium_LRS")},
				},
			},
			Plan:               &compute.PlanArgs{Publisher: pulumi.String("kinvolk"), Product: pulumi.String("flatcar-container-linux-free"), Name: pulumi.String("stable-gen2")},
			DiagnosticsProfile: &compute.DiagnosticsProfileArgs{BootDiagnostics: &compute.BootDiagnosticsArgs{Enabled: pulumi.Bool(true)}},
		}, pulumi.Protect(true), pulumi.IgnoreChanges([]string{"osProfile.customData"}))
		if err != nil {
			return err
		}
		privateIPs[node.Name] = pulumi.String(node.IP)
		if !node.worker() {
			apiEndpoints[node.Name] = pulumi.String("https://" + node.IP + ":6443")
		}
	}
	ctx.Export("scaleSetID", vmss.ID())
	ctx.Export("privateIPs", privateIPs)
	ctx.Export("publicIPs", publicIPs)
	ctx.Export("apiEndpoints", apiEndpoints)
	return nil
}
