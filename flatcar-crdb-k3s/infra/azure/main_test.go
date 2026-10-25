package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Exercise the actual Python generator, using only disposable fake credentials.
func fixture(t *testing.T, workerDB bool) Settings {
	t.Helper()
	root := t.TempDir()
	data, err := os.ReadFile("../../cluster.json")
	if err != nil {
		t.Fatal(err)
	}
	var cluster map[string]any
	if err := json.Unmarshal(data, &cluster); err != nil {
		t.Fatal(err)
	}
	cluster["ssh_authorized_key"] = "ssh-ed25519 " + base64.StdEncoding.EncodeToString(append([]byte("\x00\x00\x00\x0bssh-ed25519\x00\x00\x00\x20"), make([]byte, 32)...))
	nodes := []map[string]any{}
	for i := 1; i <= 3; i++ {
		nodes = append(nodes, map[string]any{"name": fmt.Sprintf("kube%d", i), "ip": fmt.Sprintf("192.168.1.%d", 100+i)})
	}
	worker := map[string]any{"name": "worker1", "ip": "192.168.1.104", "role": "worker"}
	if !workerDB {
		worker["cockroachdb"] = false
	}
	nodes = append(nodes, worker)
	cluster["nodes"] = nodes
	cluster["architecture"] = "x86-64"
	delete(cluster, "single_node_lab")
	configPath := filepath.Join(root, "cluster.json")
	writeJSON(t, configPath, cluster)
	for _, node := range nodes {
		dir := filepath.Join(root, "pki", node["name"].(string))
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for _, cert := range []string{"ca.crt", "node.crt", "node.key", "client.kine.crt", "client.kine.key"} {
			if err := os.WriteFile(filepath.Join(dir, cert), []byte("FAKE-TEST-CERTIFICATE"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cmd := exec.Command("python3", "../../generate.py", "ignition", "--config", configPath, "--state-dir", root)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate test Ignition: %v\n%s", err, output)
	}
	return Settings{
		Name: "test", Location: "westus2", VMSize: "Standard_D4s_v5", FlatcarVersion: "4757.2.2",
		ClusterPath: configPath, IgnitionDir: filepath.Join(root, "output"), OSDiskSizeGB: 128,
		VNetCIDR: "192.168.0.0/16", SubnetCIDR: "192.168.1.0/24",
		NodeZones: map[string]string{"kube1": "1", "kube2": "2", "kube3": "3", "worker1": "1"},
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRejectInvalidDeployment(t *testing.T) {
	cases := []struct {
		name, want string
		change     func(*Settings)
	}{
		{"unpinned-image", "exact numeric", func(s *Settings) { s.FlatcarVersion = "latest" }},
		{"wrong-subnet", "usable private IP", func(s *Settings) { s.SubnetCIDR = "192.168.2.0/24" }},
		{"outside-vnet", "inside vnetCIDR", func(s *Settings) { s.SubnetCIDR = "192.169.1.0/24" }},
		{"pod-network-overlap", "overlaps", func(s *Settings) { s.VNetCIDR = "10.0.0.0/8"; s.SubnetCIDR = "10.1.0.0/24" }},
		{"no-ssh-source", "adminCIDRs", func(s *Settings) { s.PublicNodeIPs = true }},
		{"worldwide-ssh", "not 0.0.0.0/0", func(s *Settings) { s.AdminCIDRs = []string{"0.0.0.0/0"} }},
		{"missing-zone", "nodeZones", func(s *Settings) { delete(s.NodeZones, "worker1") }},
		{"extra-zone", "exactly", func(s *Settings) { s.NodeZones["unknown"] = "1" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t, true)
			tc.change(&s)
			if _, err := loadDeployment(s); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRejectStaleAndOversizeIgnition(t *testing.T) {
	s := fixture(t, true)
	path := filepath.Join(s.IgnitionDir, "worker1.ign")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The profile still belongs to a server instead of this worker.
	wrong := strings.ReplaceAll(string(data), "k3s-agent.service", "k3s.service")
	if err := os.WriteFile(path, []byte(wrong), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeployment(s); err == nil || !strings.Contains(err.Error(), "service differs") {
		t.Fatalf("expected role mismatch, got %v", err)
	}
	if err := os.WriteFile(path, append(data, []byte(strings.Repeat(" ", 65536))...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeployment(s); err == nil || !strings.Contains(err.Error(), "custom-data limit") {
		t.Fatalf("expected custom-data limit, got %v", err)
	}
}

type mockAzure struct {
	mu        sync.Mutex
	resources []pulumi.MockResourceArgs
}

func (m *mockAzure) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	m.resources = append(m.resources, args)
	m.mu.Unlock()
	outputs := args.Inputs.Copy()
	outputs["name"] = resource.NewStringProperty(args.Name)
	outputs["ipAddress"] = resource.NewStringProperty("203.0.113.20")
	return "/mock/" + args.Name, outputs, nil
}

func (*mockAzure) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) { return args.Args, nil }

func plain(value resource.PropertyValue) resource.PropertyValue {
	if value.IsSecret() {
		return plain(value.SecretValue().Element)
	}
	return value
}

func TestAzureResources(t *testing.T) {
	for _, public := range []bool{false, true} {
		t.Run(fmt.Sprintf("public=%t", public), func(t *testing.T) {
			s := fixture(t, true)
			s.PublicNodeIPs = public
			if public {
				s.AdminCIDRs = []string{"203.0.113.10/32"}
			}
			d, err := loadDeployment(s)
			if err != nil {
				t.Fatal(err)
			}
			mocks := &mockAzure{}
			if err := pulumi.RunErr(func(ctx *pulumi.Context) error { return deploy(ctx, d) }, pulumi.WithMocks("test", "test", mocks)); err != nil {
				t.Fatal(err)
			}
			vms, publicIPs, sshRules := 0, 0, 0
			for _, res := range mocks.resources {
				switch res.TypeToken {
				case "azure-native:compute:VirtualMachine":
					vms++
					if !res.RegisterRPC.GetProtect() || !slices.Contains(res.RegisterRPC.GetIgnoreChanges(), "osProfile.customData") {
						t.Error("VMs must be protected and keep their first-boot custom data")
					}
					disk := plain(res.Inputs["storageProfile"]).ObjectValue()["osDisk"].ObjectValue()
					if disk["deleteOption"].StringValue() != "Detach" {
						t.Error("retain managed OS disk")
					}
					profile := plain(res.Inputs["osProfile"]).ObjectValue()
					if !profile["customData"].IsSecret() && !res.Inputs["osProfile"].IsSecret() {
						t.Error("Ignition must be a Pulumi secret")
					}
					if res.Name == "node-worker1" {
						encoded := plain(profile["customData"]).StringValue()
						decoded, err := base64.StdEncoding.DecodeString(encoded)
						if err != nil || !strings.Contains(string(decoded), "k3s-agent.service") || !strings.Contains(string(decoded), "cockroachdb.service") {
							t.Error("worker must get agent plus host database Ignition")
						}
					}
				case "azure-native:network:PublicIPAddress":
					publicIPs++
				case "azure-native:compute:VirtualMachineScaleSet":
					if res.Inputs["orchestrationMode"].StringValue() != "Flexible" {
						t.Error("use Flexible VMSS")
					}
					if _, ok := res.Inputs["virtualMachineProfile"]; ok {
						t.Error("do not share instance credentials in a scaling profile")
					}
				case "azure-native:network:Subnet":
					if res.Inputs["defaultOutboundAccess"].BoolValue() {
						t.Error("use explicit egress")
					}
					if _, ok := res.Inputs["natGateway"]; !ok {
						t.Error("private nodes need NAT for first boot")
					}
				case "azure-native:network:NetworkInterface":
					if res.Name == "node-worker1" {
						ip := res.Inputs["ipConfigurations"].ArrayValue()[0].ObjectValue()["privateIPAddress"].StringValue()
						if ip != "192.168.1.104" {
							t.Error("reserve the configured worker IP")
						}
					}
				case "azure-native:network:NetworkSecurityGroup":
					for _, value := range res.Inputs["securityRules"].ArrayValue() {
						rule := value.ObjectValue()
						if rule["name"].StringValue() == "management-ssh" {
							sshRules++
							if rule["destinationPortRange"].StringValue() != "22" || rule["sourceAddressPrefixes"].ArrayValue()[0].StringValue() != "203.0.113.10/32" {
								t.Error("restrict external access to management SSH")
							}
						}
					}
				}
			}
			wantIPs, wantSSH := 1, 0 // One NAT public IP even when the nodes are private.
			if public {
				wantIPs, wantSSH = 5, 1
			}
			if vms != 4 || publicIPs != wantIPs || sshRules != wantSSH {
				t.Fatalf("got %d VMs, %d public IPs, %d SSH rules", vms, publicIPs, sshRules)
			}
		})
	}
}

func TestWorkerDatabaseOptOut(t *testing.T) {
	d, err := loadDeployment(fixture(t, false))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(d.Ignition["worker1"]), "cockroachdb.service") {
		t.Error("explicit CockroachDB opt-out must survive cloud provisioning")
	}
}
