package nodebootstrap

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUniformIdentity(t *testing.T) {
	p := AzurePool{Name: "test", Role: "api", Zone: "test.westus2.internal", Location: "westus2", Subnet: "192.168.1.0/24", Token: strings.Repeat("a", 32), APICount: 3}
	raw := `{"compute":{"location":"westus2","zone":"3","vmScaleSetName":"test-api","resourceId":"/subscriptions/sub/resourceGroups/test-rg/providers/Microsoft.Compute/virtualMachineScaleSets/test-api/virtualMachines/0","subscriptionId":"sub","resourceGroupName":"test-rg"},"network":{"interface":[{"ipv4":{"ipAddress":[{"privateIpAddress":"192.168.1.12"}]}}]}}`
	var m Metadata
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	id, err := p.Identity(m)
	if err != nil || id.Name != "api-0" || id.Locality != "cloud=azure,region=westus2,zone=3" {
		t.Fatalf("%+v %v", id, err)
	}
	for _, tc := range []struct{ old, new string }{{`"zone":"3"`, `"zone":""`}, {"test-api", "test-worker"}, {"virtualMachines/0", "virtualMachines/not-zero"}, {"192.168.1.12", "203.0.113.4"}, {"test-rg", "other-rg"}, {`"location":"westus2"`, `"location":"eastus"`}} {
		var bad Metadata
		_ = json.Unmarshal([]byte(strings.ReplaceAll(raw, tc.old, tc.new)), &bad)
		if _, err := p.Identity(bad); err == nil {
			t.Errorf("accepted invalid metadata: %s", tc.old)
		}
	}
	p.Role = "worker"
	raw = strings.ReplaceAll(raw, "test-api", "test-worker")
	_ = json.Unmarshal([]byte(raw), &m)
	id, err = p.Identity(m)
	if err != nil || id.Name != "worker-0" {
		t.Fatalf("worker zero confused with API zero: %+v %v", id, err)
	}
	raw = strings.ReplaceAll(raw, "virtualMachines/0", "virtualMachines/123456")
	_ = json.Unmarshal([]byte(raw), &m)
	id, err = p.Identity(m)
	if err != nil || id.Name != "worker-123456" {
		t.Fatalf("scale-out ID: %+v %v", id, err)
	}
}
