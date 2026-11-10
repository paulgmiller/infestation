package vm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMixedAzureUsesOnlyComputeDiskAndPrivateNetwork(t *testing.T) {
	node := azureNode{Node: "mixed-2", Location: "westus2"}
	template := azureTemplate(node, "Standard_B2s")
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
	c := clusterConfig(ClusterOptions{Options: Options{Name: "mixed"}, Domain: "example.ts.net", AzureLocation: "westus2"})
	if len(c.Nodes) != 3 || !c.Tailscale.SSH || c.APITailnetHostname != "k8s-api.example.ts.net" {
		t.Fatal("wrong mixed topology")
	}
}
