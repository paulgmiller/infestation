package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/paulgmiller/infestation/internal/fsutil"
	"github.com/paulgmiller/infestation/internal/ignition"
	"github.com/paulgmiller/infestation/internal/nodebootstrap"
	"github.com/paulgmiller/infestation/internal/pki"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAzureUsesOnlyComputeDiskAndPrivateNetwork(t *testing.T) {
	set := azureScaleSet{Name: "pool", Subscription: "sub", Group: "pool-rg", Location: "westus2", Capacity: 2}
	request := azureResources(set, "Standard_B2s", "ssh-rsa disposable", "secret-profile")
	raw, _ := json.Marshal(request)
	text := string(raw)
	for _, forbidden := range []string{"Microsoft.Storage", "publicIPAddresses", "loadBalancers", "virtualMachines/extensions", "identity", "sshKeys", "adminPassword"} {
		if strings.Contains(text, forbidden) {
			t.Fatal("unexpected Azure dependency", forbidden)
		}
	}
	subnet := request.Network.Properties.Subnets[0].Properties
	if !*subnet.DefaultOutboundAccess || *subnet.NetworkSecurityGroup.ID != azureNetworkID(set, "networkSecurityGroups", "pool-nsg") {
		t.Fatal("missing owned egress subnet/NSG")
	}
	rule := request.SecurityGroup.Properties.SecurityRules[0].Properties
	if string(*rule.Direction) != "Inbound" || string(*rule.Access) != "Deny" {
		t.Fatal("missing inbound deny")
	}
}

func TestCreateDefaultsTailscaleVersionAndExplainsHostDomain(t *testing.T) {
	c := clusterConfig(ClusterOptions{Size: 1, Options: Options{Name: "cloudroach"}, Domain: "quoll-duck.ts.net"})
	if c.Tailscale.Version != TailscaleVersion {
		t.Fatal("create must supply the pinned Tailscale default")
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Tailscale.Domain = "jellyfin.quoll-duck.ts.net"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "without a host or service prefix") || strings.Contains(err.Error(), "version") {
		t.Fatalf("expected a domain-specific error, got %v", err)
	}
}

func TestAzureScaleSetCapacityAndPrivateProfile(t *testing.T) {
	request := azureResources(azureScaleSet{Name: "pool", Subscription: "sub", Group: "pool-rg", Location: "westus2", Capacity: 2}, "Standard_B2s", "ssh-rsa disposable", "shared-profile")
	set := request.ScaleSet
	if *set.SKU.Capacity != 2 || string(*set.Placement.ZonePlacementPolicy) != "Auto" || *set.Properties.SinglePlacementGroup {
		t.Fatal("capacity or zone placement changed")
	}
	if len(set.Zones) != 0 || set.Properties.ZoneBalance != nil {
		t.Fatal("explicit zones/balancing")
	}
	properties := set.Properties
	if string(*properties.OrchestrationMode) != "Uniform" || *properties.Overprovision || string(*properties.UpgradePolicy.Mode) != "Manual" {
		t.Fatal("unexpected lifecycle")
	}
	profile := properties.VirtualMachineProfile
	if *profile.OSProfile.CustomData != "shared-profile" || profile.StorageProfile.OSDisk.DeleteOption != nil {
		t.Fatal("shared profile or disk lifecycle changed")
	}
	raw, _ := json.Marshal(set)
	for _, forbidden := range []string{"loadBalancers", "publicIPAddressConfiguration", "Microsoft.Storage", "privateDnsZones", "automaticRepairsPolicy", "autoscaleSettings"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("unexpected dependency/automation", forbidden)
		}
	}
}

type azureFake struct {
	calls                          []string
	owner                          string
	exists, failDeploy, failDelete bool
	groupError                     error
}

func (r *azureFake) call(op string, set azureScaleSet) {
	r.calls = append(r.calls, op+" "+set.Subscription+" "+set.Group+" "+set.Name)
}
func (r *azureFake) Group(_ context.Context, set azureScaleSet) (string, bool, error) {
	r.call("get", set)
	return r.owner, r.exists, r.groupError
}
func (r *azureFake) CreateGroup(_ context.Context, set azureScaleSet) error {
	r.call("create", set)
	return nil
}
func (r *azureFake) Provision(_ context.Context, set azureScaleSet, _ azureRequest, stage func(string) error) error {
	r.call("provision", set)
	if err := stage("Creating scale set"); err != nil {
		return err
	}
	if r.failDeploy {
		return fmt.Errorf("provision failed")
	}
	return nil
}
func (r *azureFake) Resources(_ context.Context, set azureScaleSet) ([]azureResource, error) {
	r.call("resources", set)
	return []azureResource{}, nil
}
func (r *azureFake) Power(_ context.Context, set azureScaleSet, action string) error {
	r.call(action, set)
	return nil
}
func (r *azureFake) Instances(_ context.Context, set azureScaleSet) ([]azureInstance, error) {
	r.call("instances", set)
	return []azureInstance{}, nil
}
func (r *azureFake) DeleteGroup(_ context.Context, set azureScaleSet) error {
	r.call("delete", set)
	if r.failDelete {
		return fmt.Errorf("delete failed")
	}
	return nil
}

func TestAzureCreateDeploysOneScaleSetAndPreservesFailureState(t *testing.T) {
	for _, fail := range []bool{false, true} {
		root := t.TempDir()
		c := clusterConfig(ClusterOptions{Options: Options{Name: "pool"}, Size: 2, Domain: "example.ts.net"})
		if err := fsutil.WriteFile(filepath.Join(root, "output", "pool-0.ign"), []byte("shared-private-profile"), 0600); err != nil {
			t.Fatal(err)
		}
		r := &azureFake{failDeploy: fail}
		err := createAzureNodes(context.Background(), ClusterOptions{Options: Options{Name: "pool"}, AzureLocation: "westus2", AzureVMSize: "Standard_B2s", AzureSubscription: "subscription"}, root, c.Nodes, io.Discard, r)
		if (err != nil) != fail {
			t.Fatal("wrong deployment outcome", err)
		}
		calls := strings.Join(r.calls, "\n")
		if strings.Count(calls, "create subscription pool-rg pool") != 1 || strings.Count(calls, "provision subscription pool-rg pool") != 1 {
			t.Fatal("pool not deployed once", calls)
		}
		var set azureScaleSet
		if err := fsutil.ReadJSON(filepath.Join(root, "azure-scaleset.json"), &set); err != nil || set.Capacity != 2 {
			t.Fatal("capacity/ownership state missing", err)
		}
		var request azureRequest
		if err := fsutil.ReadJSON(filepath.Join(root, "azure-request.json"), &request); err != nil || request.ScaleSet.Properties.VirtualMachineProfile.OSProfile.CustomData == nil {
			t.Fatal("private request missing", err)
		}
		info, err := os.Stat(filepath.Join(root, "azure-request.json"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("request not private", err)
		}
		if fail && strings.Contains(calls, "delete") {
			t.Fatal("failed provisioning destroyed evidence")
		}
	}
}
func TestAzureScaleSetLifecycleChecksOwnershipAndTargetsWholeSet(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "different-subscription")
	root, err := os.MkdirTemp("", "vmss-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	dir := filepath.Join(root, "pool")
	c := clusterConfig(ClusterOptions{Options: Options{Name: "pool"}, Size: 2, Domain: "example.ts.net"})
	if err := fsutil.WriteJSON(filepath.Join(dir, "nodes.json"), c); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteJSON(filepath.Join(dir, "azure-scaleset.json"), azureScaleSet{Subscription: "subscription", Group: "pool-rg", Name: "pool", Ownership: "pool", Capacity: 2}); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteJSON(filepath.Join(dir, "provider.json"), "azure"); err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"start", "stop", "status"} {
		r := &azureFake{owner: "pool", exists: true}
		if err := azureActions(context.Background(), Options{Name: "pool", StateDir: root}, action, io.Discard, r); err != nil {
			t.Fatal(err)
		}
		calls := strings.Join(r.calls, "\n")
		if len(r.calls) != 2 || !strings.Contains(calls, "subscription pool-rg pool") {
			t.Fatal("lifecycle did not target the whole owned pool", calls)
		}
	}
	r := &azureFake{owner: "somebody-else", exists: true}
	if err := azureActions(context.Background(), Options{Name: "pool", StateDir: root}, "stop", io.Discard, r); err == nil || len(r.calls) != 1 {
		t.Fatal("foreign resource group was operated")
	}
}

func TestSharedScaleSetProfileBuildsIdentityBeforeEnrollment(t *testing.T) {
	c := clusterConfig(ClusterOptions{Options: Options{Name: "pool"}, Size: 2, Init: true, Domain: "example.ts.net"})
	state := t.TempDir()
	if err := pki.Create(c, state); err != nil {
		t.Fatal(err)
	}
	shared, err := pki.PoolCertificate(state, c.Tailscale.Domain)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range shared {
		if err := fsutil.WriteFile(filepath.Join(state, "pki", "pool-0", name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	token, err := ignition.Token(c, state)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := nodebootstrap.NewJoinBundle(c, c.Nodes[0], state, token)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := pki.BootstrapCredential(state)
	if err != nil {
		t.Fatal(err)
	}
	b := ignition.Bootstrap{HelperSourceCommit: strings.Repeat("a", 40), JoinBundle: bundle, Credential: credential, TailscaleAuthKey: "tskey-auth-TEST", ScaleSet: &nodebootstrap.ScaleSet{Name: "pool", Group: "pool-rg", Location: "westus2", Initialize: true}}
	doc, err := ignition.Render(c, c.Nodes[0], state, token, b)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(doc)
	if len(raw) > 65535 {
		t.Fatalf("shared profile exceeds Azure customData limit: %d", len(raw))
	}
	identity, dependency, dynamicAddress := false, false, false
	for _, u := range doc.Systemd.Units {
		if u.Name == "infestation-instance-identity.service" && strings.Contains(*u.Contents, "Requires=infestation-helper-build.service") {
			identity = true
		}
		if u.Name == "infestation-tailnet.service" {
			for _, d := range u.Dropins {
				if strings.Contains(*d.Contents, "Requires=infestation-instance-identity.service") {
					dependency = true
				}
			}
		}
		if u.Name == "cockroachdb.service" && strings.Contains(*u.Contents, "--advertise-addr=${INFESTATION_DATABASE_ADDRESS}") {
			dynamicAddress = true
		}
	}
	if !identity || !dependency || !dynamicAddress {
		t.Fatal("VMSS identity must gate tailnet enrollment and database addressing")
	}
}

func TestAzureMissingSubscriptionFailsBeforeStateOrExternalCommands(t *testing.T) {
	t.Setenv("AZURE_SUBSCRIPTION_ID", "")
	t.Setenv("PATH", t.TempDir())
	root := t.TempDir()
	err := CreateCluster(context.Background(), ClusterOptions{Options: Options{Name: "pool", StateDir: root}, Provider: "azure", Size: 1}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--subscription") {
		t.Fatalf("missing subscription not explained: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("invalid subscription created state", err)
	}
}

func TestAzureCreateRefusesExistingOrUnreadableGroupsBeforeMutation(t *testing.T) {
	for _, unreadable := range []bool{false, true} {
		root := t.TempDir()
		fake := &azureFake{exists: true, owner: "somebody-else"}
		if unreadable {
			fake.groupError = fmt.Errorf("access denied")
		}
		nodes := clusterConfig(ClusterOptions{Options: Options{Name: "pool"}, Size: 1, Domain: "example.ts.net"}).Nodes
		err := createAzureNodes(context.Background(), ClusterOptions{Options: Options{Name: "pool"}, AzureSubscription: "subscription"}, root, nodes, io.Discard, fake)
		if err == nil || len(fake.calls) != 1 {
			t.Fatal("existing/unreadable group was mutated", err, fake.calls)
		}
		entries, err := os.ReadDir(root)
		if err != nil || len(entries) != 0 {
			t.Fatal("refused create wrote provider inputs", err)
		}
	}
}
