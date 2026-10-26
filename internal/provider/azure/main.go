package azure

import (
	"encoding/base64"
	"sort"

	"github.com/pulumi/pulumi-azure-native-sdk/compute/v3"
	"github.com/pulumi/pulumi-azure-native-sdk/network/v3"
	"github.com/pulumi/pulumi-azure-native-sdk/resources/v3"
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
		s := Settings{
			Name: get("name", "flatcar-"+ctx.Stack()), Location: get("location", "westus2"),
			FlatcarVersion: cfg.Require("flatcarVersion"), VMSize: cfg.Require("vmSize"),
			ClusterPath: get("clusterPath", "cluster.local.json"), IgnitionDir: get("ignitionDir", "state/output"),
			VNetCIDR: get("vnetCIDR", "192.168.0.0/16"), SubnetCIDR: get("subnetCIDR", "192.168.1.0/24"),
			OSDiskSizeGB: 32, PublicNodeIPs: cfg.GetBool("publicNodeIPs"),
			OSDiskStorageAccountType: get("osDiskStorageAccountType", "StandardSSD_LRS"),
			APILoadBalancer:          cfg.GetBool("apiLoadBalancer"),
			OutboundPortsPerNode:     1024,
		}
		if cfg.Get("outboundPortsPerNode") != "" {
			s.OutboundPortsPerNode = cfg.RequireInt("outboundPortsPerNode")
		}
		s.APIDNSLabel = get("apiDNSLabel", s.Name+"-api")
		if err := cfg.GetObject("apiAdminCIDRs", &s.APIAdminCIDRs); err != nil {
			return err
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
	if s.APILoadBalancer {
		rules = append(rules,
			&network.SecurityRuleTypeArgs{
				Name: pulumi.String("management-api"), Priority: pulumi.Int(120), Direction: pulumi.String("Inbound"),
				Access: pulumi.String("Allow"), Protocol: pulumi.String("Tcp"), SourcePortRange: pulumi.String("*"),
				DestinationPortRange: pulumi.String("6443"), SourceAddressPrefixes: pulumi.ToStringArray(s.APIAdminCIDRs), DestinationAddressPrefix: pulumi.String("*"),
			},
			&network.SecurityRuleTypeArgs{
				Name: pulumi.String("api-health-probe"), Priority: pulumi.Int(130), Direction: pulumi.String("Inbound"),
				Access: pulumi.String("Allow"), Protocol: pulumi.String("Tcp"), SourcePortRange: pulumi.String("*"),
				DestinationPortRange: pulumi.String("6443"), SourceAddressPrefix: pulumi.String("AzureLoadBalancer"), DestinationAddressPrefix: pulumi.String("*"),
			})
	}
	// Azure returns etags for inline rules; they are server-managed metadata.
	nsg, err := network.NewNetworkSecurityGroup(ctx, "nodes", &network.NetworkSecurityGroupArgs{
		NetworkSecurityGroupName: pulumi.String(s.Name + "-nodes"), ResourceGroupName: rg.Name, Location: location, SecurityRules: rules,
	}, pulumi.IgnoreChanges([]string{"securityRules[*].etag"}))
	if err != nil {
		return err
	}
	subnet, err := network.NewSubnet(ctx, "nodes", &network.SubnetArgs{
		SubnetName: pulumi.String("nodes"), ResourceGroupName: rg.Name, VirtualNetworkName: vnet.Name,
		AddressPrefix: pulumi.String(s.SubnetCIDR), DefaultOutboundAccess: pulumi.Bool(false),
		NetworkSecurityGroup: &network.NetworkSecurityGroupTypeArgs{Id: nsg.ID()},
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
	var apiBackend, egressBackend pulumi.StringOutput
	{
		var dns network.PublicIPAddressDnsSettingsPtrInput
		if s.APILoadBalancer {
			dns = &network.PublicIPAddressDnsSettingsArgs{DomainNameLabel: pulumi.String(s.APIDNSLabel)}
		}
		ip, err := network.NewPublicIPAddress(ctx, "api", &network.PublicIPAddressArgs{
			PublicIpAddressName: pulumi.String(s.Name + "-api"), ResourceGroupName: rg.Name, Location: location,
			Sku: &network.PublicIPAddressSkuArgs{Name: pulumi.String("Standard")}, PublicIPAllocationMethod: pulumi.String("Static"),
			Zones:       pulumi.ToStringArray([]string{"1", "2", "3"}),
			DnsSettings: dns,
		})
		if err != nil {
			return err
		}
		// Derive child IDs from the group so inline rules do not depend on their own LB.
		lbID := pulumi.Sprintf("%s/providers/Microsoft.Network/loadBalancers/%s-api", rg.ID(), s.Name)
		pools := network.BackendAddressPoolArray{&network.BackendAddressPoolArgs{Name: pulumi.String("egress")}}
		var probes network.ProbeArray
		var apiRules network.LoadBalancingRuleArray
		if s.APILoadBalancer {
			pools = append(pools, &network.BackendAddressPoolArgs{Name: pulumi.String("servers")})
			probes = network.ProbeArray{&network.ProbeArgs{Name: pulumi.String("api"), Protocol: pulumi.String("Tcp"), Port: pulumi.Int(6443), IntervalInSeconds: pulumi.Int(5), NumberOfProbes: pulumi.Int(2)}}
			apiRules = network.LoadBalancingRuleArray{&network.LoadBalancingRuleArgs{
				Name: pulumi.String("api"), Protocol: pulumi.String("Tcp"), FrontendPort: pulumi.Int(6443), BackendPort: pulumi.Int(6443),
				FrontendIPConfiguration: &network.SubResourceArgs{Id: pulumi.Sprintf("%s/frontendIPConfigurations/public", lbID)},
				BackendAddressPool:      &network.SubResourceArgs{Id: pulumi.Sprintf("%s/backendAddressPools/servers", lbID)},
				Probe:                   &network.SubResourceArgs{Id: pulumi.Sprintf("%s/probes/api", lbID)},
				DisableOutboundSnat:     pulumi.Bool(true), EnableTcpReset: pulumi.Bool(true), IdleTimeoutInMinutes: pulumi.Int(30),
			}}
		}
		lb, err := network.NewLoadBalancer(ctx, "api", &network.LoadBalancerArgs{
			LoadBalancerName: pulumi.String(s.Name + "-api"), ResourceGroupName: rg.Name, Location: location,
			Sku: &network.LoadBalancerSkuArgs{Name: pulumi.String("Standard")},
			FrontendIPConfigurations: network.FrontendIPConfigurationArray{&network.FrontendIPConfigurationArgs{
				Name: pulumi.String("public"), PublicIPAddress: &network.PublicIPAddressTypeArgs{Id: ip.ID()},
			}},
			BackendAddressPools: pools, Probes: probes, LoadBalancingRules: apiRules,
			OutboundRules: network.OutboundRuleArray{&network.OutboundRuleArgs{
				Name: pulumi.String("egress"), Protocol: pulumi.String("All"), AllocatedOutboundPorts: pulumi.Int(s.OutboundPortsPerNode),
				BackendAddressPool:       &network.SubResourceArgs{Id: pulumi.Sprintf("%s/backendAddressPools/egress", lbID)},
				FrontendIPConfigurations: network.SubResourceArray{&network.SubResourceArgs{Id: pulumi.Sprintf("%s/frontendIPConfigurations/public", lbID)}},
				EnableTcpReset:           pulumi.Bool(true), IdleTimeoutInMinutes: pulumi.Int(4),
			}},
			// NIC IP configurations own pool membership. Azure also returns derived
			// address entries here; omitting those must not detach the NIC backends.
		}, pulumi.IgnoreChanges([]string{"backendAddressPools[*].loadBalancerBackendAddresses"}))
		if err != nil {
			return err
		}
		apiBackend = pulumi.Sprintf("%s/backendAddressPools/servers", lb.ID())
		egressBackend = pulumi.Sprintf("%s/backendAddressPools/egress", lb.ID())
		ctx.Export("egressPublicIP", ip.IpAddress)
		if s.APILoadBalancer {
			ctx.Export("apiEndpoint", pulumi.String("https://"+s.apiHostname()+":6443"))
			ctx.Export("apiPublicIP", ip.IpAddress)
		}
	}
	privateIPs, publicIPs, apiEndpoints := pulumi.StringMap{}, pulumi.StringMap{}, pulumi.StringMap{}
	for _, node := range d.Cluster.Nodes {
		ipConfig := &network.NetworkInterfaceIPConfigurationArgs{
			Name: pulumi.String("primary"), Primary: pulumi.Bool(true),
			PrivateIPAddress: pulumi.String(node.IP), PrivateIPAllocationMethod: pulumi.String("Static"),
			Subnet:                          &network.SubnetTypeArgs{Id: subnet.ID()},
			LoadBalancerBackendAddressPools: network.BackendAddressPoolArray{&network.BackendAddressPoolArgs{Id: egressBackend}},
		}
		if s.APILoadBalancer && !node.Worker() {
			ipConfig.LoadBalancerBackendAddressPools = network.BackendAddressPoolArray{&network.BackendAddressPoolArgs{Id: egressBackend}, &network.BackendAddressPoolArgs{Id: apiBackend}}
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
				ImageReference: &compute.ImageReferenceArgs{Publisher: pulumi.String("kinvolk"), Offer: pulumi.String("flatcar-container-linux-corevm-amd64"), Sku: pulumi.String("stable-gen2"), Version: pulumi.String(s.FlatcarVersion)},
				OsDisk: &compute.OSDiskArgs{
					Name: pulumi.String(s.Name + "-" + node.Name + "-os"), CreateOption: pulumi.String("FromImage"),
					DiskSizeGB: pulumi.Int(s.OSDiskSizeGB), Caching: compute.CachingTypesReadWrite, DeleteOption: pulumi.String("Detach"),
					ManagedDisk: &compute.ManagedDiskParametersArgs{StorageAccountType: pulumi.String(s.OSDiskStorageAccountType)},
				},
			},
			DiagnosticsProfile: &compute.DiagnosticsProfileArgs{BootDiagnostics: &compute.BootDiagnosticsArgs{Enabled: pulumi.Bool(true)}},
		}, pulumi.Protect(true), pulumi.IgnoreChanges([]string{"osProfile.customData"}))
		if err != nil {
			return err
		}
		privateIPs[node.Name] = pulumi.String(node.IP)
		if !node.Worker() {
			apiEndpoints[node.Name] = pulumi.String("https://" + node.IP + ":6443")
		}
	}
	ctx.Export("scaleSetID", vmss.ID())
	ctx.Export("privateIPs", privateIPs)
	ctx.Export("publicIPs", publicIPs)
	ctx.Export("apiEndpoints", apiEndpoints)
	return nil
}
