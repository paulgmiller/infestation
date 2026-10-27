package azure

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	"infestation/internal/ignition"
	"infestation/internal/nodebootstrap"
)

func loadPools(d *Deployment) (*Deployment, error) {
	s := d.Settings
	key, err := os.ReadFile(filepath.Join(s.IgnitionDir, "..", "pki", "disabled-login.pub"))
	if err != nil {
		return nil, err
	}
	d.DisabledLoginKey = string(key)
	if !s.APILoadBalancer || s.SubnetCIDR != "192.168.1.0/24" {
		return nil, fmt.Errorf("Uniform currently requires API load balancer and subnet 192.168.1.0/24")
	}
	if s.APICount != 3 || s.WorkerCount < 0 || len(d.Cluster.Servers()) != 3 || len(d.Cluster.Nodes)-3 != s.WorkerCount {
		return nil, fmt.Errorf("pool counts must match initial inventory (three API servers)")
	}
	if !strings.HasSuffix(s.PrivateDNSZone, ".internal") || s.PrivateDNSZone != s.Name+"."+s.Location+".internal" {
		return nil, fmt.Errorf("privateDNSZone must be the cluster's regional internal zone")
	}
	var workerDB *bool
	for _, n := range d.Cluster.Nodes {
		if !n.Worker() && !n.Database() {
			return nil, fmt.Errorf("API pool must host CockroachDB")
		}
		if n.Worker() {
			v := n.Database()
			if workerDB != nil && *workerDB != v {
				return nil, fmt.Errorf("worker pool must use a single CockroachDB setting")
			}
			workerDB = &v
		}
	}
	for _, role := range []string{"api", "worker"} {
		raw, err := os.ReadFile(filepath.Join(s.IgnitionDir, role+".ign"))
		if err != nil {
			return nil, err
		}
		if len(raw) > 65535 {
			return nil, fmt.Errorf("pool Ignition exceeds custom-data limit")
		}
		if err := ignition.ValidateNoSSH(raw); err != nil {
			return nil, err
		}
		var doc ignitionDocument
		if err := json.Unmarshal(raw, &doc); err != nil {
			return nil, err
		}
		found := false
		for _, f := range doc.Storage.Files {
			if f.Path == "/etc/infestation/bootstrap.json" {
				source, ok := strings.CutPrefix(f.Contents.Source, "data:;base64,")
				if !ok {
					return nil, fmt.Errorf("missing embedded bootstrap config")
				}
				data, err := base64.StdEncoding.DecodeString(source)
				if err != nil {
					return nil, err
				}
				var c nodebootstrap.Config
				if json.Unmarshal(data, &c) != nil || c.Azure == nil || c.Azure.Validate() != nil || c.Azure.Name != s.Name || c.Azure.Role != role || c.Azure.Zone != s.PrivateDNSZone || c.Azure.Location != s.Location || c.Azure.Subnet != s.SubnetCIDR || c.Azure.APICount != s.APICount || c.Azure.WorkerCount != s.WorkerCount {
					return nil, fmt.Errorf("pool Ignition config is stale or mismatched")
				}
				found = true
			}
		}
		if !found {
			return nil, fmt.Errorf("pool bootstrap config is missing")
		}
		d.Ignition[role] = raw
	}
	return d, nil
}

type poolIdentity struct {
	pulumi.CustomResourceState
	PrincipalID pulumi.StringOutput `pulumi:"principalId"`
	ClientID    pulumi.StringOutput `pulumi:"clientId"`
}

func deployPools(ctx *pulumi.Context, d *Deployment, group, location pulumi.StringInput, subnet, vnet, apiBackend, egressBackend pulumi.StringOutput, artifacts *artifactAccess) error {
	s := d.Settings
	zone, err := artifactResourceNew(ctx, "privatedns:PrivateZone", "database-dns", pulumi.Map{"privateZoneName": pulumi.String(s.PrivateDNSZone), "resourceGroupName": group, "location": pulumi.String("global")})
	if err != nil {
		return err
	}
	link, err := artifactResourceNew(ctx, "privatedns:VirtualNetworkLink", "database-dns-link", pulumi.Map{"privateZoneName": pulumi.String(s.PrivateDNSZone), "resourceGroupName": group, "virtualNetworkLinkName": pulumi.String("nodes"), "location": pulumi.String("global"), "registrationEnabled": pulumi.Bool(false), "virtualNetwork": pulumi.Map{"id": vnet}}, pulumi.DependsOn([]pulumi.Resource{zone}))
	if err != nil {
		return err
	}
	ids := pulumi.StringMap{}
	for _, role := range []string{"api", "worker"} {
		identity := &poolIdentity{}
		if err := ctx.RegisterResource("azure-native:managedidentity:UserAssignedIdentity", "identity-"+role, pulumi.Map{"resourceName": pulumi.String(s.Name + "-" + role), "resourceGroupName": group, "location": location}, identity, pulumi.Version("3.29.0")); err != nil {
			return err
		}
		// VMSS instances share the pool identity; IAM exists before first boot.
		if err := artifacts.grant(ctx, role+"-assets", artifacts.Assets, identity.PrincipalID, false, true); err != nil {
			return err
		}
		var permissions []pulumi.Resource
		assignment := pulumi.All(zone.ID(), identity.PrincipalID).ApplyT(func(v []interface{}) string {
			return uuid.NewSHA1(uuid.NameSpaceURL, []byte(fmt.Sprint(v[0])+"/"+fmt.Sprint(v[1])+"/dns")).String()
		}).(pulumi.StringOutput)
		dnsPermission, err := artifactResourceNew(ctx, "authorization:RoleAssignment", role+"-dns", pulumi.Map{"roleAssignmentName": assignment, "scope": zone.ID(), "principalId": identity.PrincipalID, "principalType": pulumi.String("ServicePrincipal"), "roleDefinitionId": pulumi.String("/subscriptions/" + artifacts.Subscription + "/providers/Microsoft.Authorization/roleDefinitions/b12aa53e-6015-4669-85d0-8515ebb3ae7f")})
		if err != nil {
			return err
		}
		permissions = append(permissions, dnsPermission, link)
		if role == "api" {
			if err := artifacts.grant(ctx, role+"-access", artifacts.Access, identity.PrincipalID, true, true); err != nil {
				return err
			}
		}
		pools := pulumi.Array{pulumi.Map{"id": egressBackend}}
		count := s.WorkerCount
		if role == "api" {
			count = s.APICount
			pools = append(pools, pulumi.Map{"id": apiBackend})
		}
		identities := identity.ID().ToStringOutput().ApplyT(func(id string) map[string]interface{} { return map[string]interface{}{id: map[string]interface{}{}} })
		profile := pulumi.Map{
			"osProfile":          pulumi.Map{"computerNamePrefix": pulumi.String(role), "adminUsername": pulumi.String("core"), "customData": pulumi.ToSecret(pulumi.String(base64.StdEncoding.EncodeToString(d.Ignition[role]))), "linuxConfiguration": pulumi.Map{"disablePasswordAuthentication": pulumi.Bool(true), "ssh": pulumi.Map{"publicKeys": pulumi.Array{pulumi.Map{"path": pulumi.String("/home/core/.ssh/authorized_keys"), "keyData": pulumi.String(d.DisabledLoginKey)}}}}},
			"storageProfile":     pulumi.Map{"imageReference": pulumi.Map{"publisher": pulumi.String("kinvolk"), "offer": pulumi.String("flatcar-container-linux-corevm-amd64"), "sku": pulumi.String("stable-gen2"), "version": pulumi.String(s.FlatcarVersion)}, "osDisk": pulumi.Map{"createOption": pulumi.String("FromImage"), "diskSizeGB": pulumi.Int(s.OSDiskSizeGB), "caching": pulumi.String("ReadWrite"), "managedDisk": pulumi.Map{"storageAccountType": pulumi.String(s.OSDiskStorageAccountType)}}},
			"networkProfile":     pulumi.Map{"networkInterfaceConfigurations": pulumi.Array{pulumi.Map{"name": pulumi.String("primary"), "primary": pulumi.Bool(true), "enableIPForwarding": pulumi.Bool(true), "ipConfigurations": pulumi.Array{pulumi.Map{"name": pulumi.String("primary"), "primary": pulumi.Bool(true), "subnet": pulumi.Map{"id": subnet}, "loadBalancerBackendAddressPools": pools}}}}},
			"diagnosticsProfile": pulumi.Map{"bootDiagnostics": pulumi.Map{"enabled": pulumi.Bool(true)}},
		}
		// This versioned resource is present in the pinned 3.29.0 provider schema.
		// The default generated Go SDK predates placement.zonePlacementPolicy.
		vmss, err := artifactResourceNew(ctx, "compute/v20260301:VirtualMachineScaleSet", role+"-pool", pulumi.Map{
			"vmScaleSetName": pulumi.String(s.Name + "-" + role), "resourceGroupName": group, "location": location,
			"orchestrationMode": pulumi.String("Uniform"), "overprovision": pulumi.Bool(false), "singlePlacementGroup": pulumi.Bool(false),
			"placement":   pulumi.Map{"zonePlacementPolicy": pulumi.String("auto")},
			"zoneBalance": pulumi.Bool(true), "resiliencyPolicy": pulumi.Map{"zoneAllocationPolicy": pulumi.Map{"maxZoneCount": pulumi.Int(3)}},
			"sku":           pulumi.Map{"name": pulumi.String(s.VMSize), "tier": pulumi.String("Standard"), "capacity": pulumi.Int(count)},
			"identity":      pulumi.Map{"type": pulumi.String("UserAssigned"), "userAssignedIdentities": identities},
			"upgradePolicy": pulumi.Map{"mode": pulumi.String("Manual")}, "virtualMachineProfile": profile,
		}, pulumi.Protect(true), pulumi.DependsOn(permissions))
		if err != nil {
			return err
		}
		ids[role] = vmss.ID().ToStringOutput()
	}
	ctx.Export("scaleSetIDs", ids)
	ctx.Export("privateDNSZone", pulumi.String(s.PrivateDNSZone))
	ctx.Export("publicIPs", pulumi.StringMap{})
	return nil
}
