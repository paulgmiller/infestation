package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
	"infestation/internal/ignition"
	"infestation/internal/nodebootstrap"
	"infestation/internal/pki"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMixedAzureUsesOnlyComputeDiskAndPrivateNetwork(t *testing.T) {
	set := azureScaleSet{Name: "pool", Location: "westus2", Capacity: 2}
	template := azureScaleSetTemplate(set, "Standard_B2s", "ssh-rsa disposable")
	raw, _ := json.Marshal(template)
	text := string(raw)
	for _, forbidden := range []string{"Microsoft.Storage", "publicIPAddresses", "loadBalancers", "virtualMachines/extensions", "identity", "sshKeys", "adminPassword"} {
		if strings.Contains(text, forbidden) {
			t.Fatal("unexpected Azure dependency", forbidden)
		}
	}
	if !strings.Contains(text, `"defaultOutboundAccess":true`) || !strings.Contains(text, `"direction":"Inbound"`) || !strings.Contains(text, `"access":"Deny"`) {
		t.Fatal("missing outbound-only network")
	}
	if !strings.Contains(text, `"type":"secureString"`) {
		t.Fatal("customData not a secure template parameter")
	}
	c := clusterConfig(ClusterOptions{Size: 3, Init: true, Options: Options{Name: "mixed"}, Domain: "example.ts.net", AzureLocation: "westus2"})
	if len(c.Nodes) != 3 || !c.Tailscale.SSH || c.APITailnetHostname != "k8s-api.example.ts.net" {
		t.Fatal("wrong mixed topology")
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
	template := azureScaleSetTemplate(azureScaleSet{Name: "pool", Location: "westus2", Capacity: 2}, "Standard_B2s", "ssh-rsa disposable")
	resources := template["resources"].([]any)
	if len(resources) != 3 {
		t.Fatal("expected NSG, VNet and one scale set")
	}
	set := resources[2].(map[string]any)
	if set["type"] != "Microsoft.Compute/virtualMachineScaleSets" || set["sku"].(map[string]any)["capacity"] != 2 {
		t.Fatal("size is not VMSS capacity")
	}
	properties := set["properties"].(map[string]any)
	if set["apiVersion"] != "2026-03-01" || set["placement"].(map[string]any)["zonePlacementPolicy"] != "Auto" || properties["singlePlacementGroup"] != false {
		t.Fatal("missing automatic Azure zone placement")
	}
	if _, ok := set["zones"]; ok {
		t.Fatal("zone numbers must be selected by Azure")
	}
	if _, ok := properties["zoneBalance"]; ok {
		t.Fatal("must not impose explicit zone balancing")
	}
	if properties["orchestrationMode"] != "Uniform" || properties["overprovision"] != false || properties["upgradePolicy"].(map[string]any)["mode"] != "Manual" {
		t.Fatal("unexpected scale-set lifecycle")
	}
	profile := properties["virtualMachineProfile"].(map[string]any)
	if profile["osProfile"].(map[string]any)["customData"] != "[parameters('customData')]" {
		t.Fatal("profile is not shared")
	}
	if _, ok := profile["storageProfile"].(map[string]any)["osDisk"].(map[string]any)["deleteOption"]; ok {
		t.Fatal("Uniform VMSS must not use Flexible-only disk deleteOption")
	}

	raw, _ := json.Marshal(template)
	for _, forbidden := range []string{"loadBalancers", "publicIPAddressConfiguration", "Microsoft.Storage", "privateDnsZones", "automaticRepairsPolicy", "autoscaleSettings"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatal("unexpected dependency/automation", forbidden)
		}
	}
}

type azureRunner struct {
	calls      []string
	owner      string
	exists     bool
	failDeploy bool
}

func (r *azureRunner) Run(_ context.Context, name string, args ...string) executil.Result {
	call := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)
	switch {
	case strings.HasPrefix(call, "az account show"):
		return executil.Result{Stdout: "subscription\n"}
	case strings.HasPrefix(call, "az group exists"):
		return executil.Result{Stdout: fmt.Sprintf("%t\n", r.exists)}
	case strings.HasPrefix(call, "az group show"):
		return executil.Result{Stdout: r.owner}
	case strings.HasPrefix(call, "az deployment group create") && r.failDeploy:
		return executil.Result{Code: 1}
	case strings.HasPrefix(call, "az resource list"):
		return executil.Result{Stdout: "[]"}
	default:
		return executil.Result{}
	}
}
func TestAzureCreateDeploysOneScaleSetAndPreservesFailureState(t *testing.T) {
	for _, fail := range []bool{false, true} {
		root := t.TempDir()
		c := clusterConfig(ClusterOptions{Options: Options{Name: "pool"}, Size: 2, Domain: "example.ts.net"})
		if err := fsutil.WriteFile(filepath.Join(root, "output", "pool-0.ign"), []byte("shared-private-profile"), 0600); err != nil {
			t.Fatal(err)
		}
		r := &azureRunner{failDeploy: fail}
		err := createAzureNodes(context.Background(), ClusterOptions{Options: Options{Name: "pool"}, AzureLocation: "westus2", AzureVMSize: "Standard_B2s"}, root, c.Nodes, io.Discard, r)
		if (err != nil) != fail {
			t.Fatal("wrong deployment outcome", err)
		}
		calls := strings.Join(r.calls, "\n")
		if strings.Count(calls, "az group create") != 1 || strings.Count(calls, "az deployment group create") != 1 || strings.Contains(calls, "az vm create") {
			t.Fatal("pool not deployed once", calls)
		}
		var set azureScaleSet
		if err := fsutil.ReadJSON(filepath.Join(root, "azure-scaleset.json"), &set); err != nil || set.Capacity != 2 {
			t.Fatal("capacity/ownership state missing", err)
		}
		var parameters map[string]any
		if err := fsutil.ReadJSON(filepath.Join(root, "azure-parameters.json"), &parameters); err != nil || len(parameters) != 1 {
			t.Fatal("expected one shared private profile", err)
		}
		if fail && strings.Contains(calls, "delete") {
			t.Fatal("failed provisioning destroyed evidence")
		}
	}
}
func TestAzureScaleSetLifecycleChecksOwnershipAndTargetsWholeSet(t *testing.T) {
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
	for _, action := range []string{"start", "stop", "status"} {
		r := &azureRunner{owner: "pool"}
		if err := azureActions(context.Background(), Options{Name: "pool", StateDir: root}, action, io.Discard, r); err != nil {
			t.Fatal(err)
		}
		calls := strings.Join(r.calls, "\n")
		if len(r.calls) != 2 || !strings.Contains(calls, "az vmss ") || strings.Contains(calls, "--instance-ids") {
			t.Fatal("lifecycle did not target the whole owned pool", calls)
		}
	}
	r := &azureRunner{owner: "somebody-else"}
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
