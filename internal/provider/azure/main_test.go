package azure

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	clusterconfig "infestation/internal/cluster"
	"infestation/internal/ignition"
	"infestation/internal/nodebootstrap"
	"infestation/internal/pki"
	"infestation/internal/testutil"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

// Exercise the shared Go Ignition generator, using only disposable fake credentials.
func fixture(t *testing.T, workerDB bool) Settings {
	t.Helper()
	root := t.TempDir()
	data, err := os.ReadFile("../../../examples/cluster.json")
	if err != nil {
		t.Fatal(err)
	}
	var cluster map[string]any
	if err := json.Unmarshal(data, &cluster); err != nil {
		t.Fatal(err)
	}
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
	cluster["tls_sans"] = []string{"test-api.westus2.cloudapp.azure.com"}
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
	cfg, err := clusterconfig.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ignition.Generate(cfg, root); err != nil {
		t.Fatal(err)
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
		{"missing-zone", "nodeZones", func(s *Settings) { delete(s.NodeZones, "worker1") }},
		{"invalid-snat-port-block", "multiple of 8", func(s *Settings) { s.OutboundPortsPerNode = 1025 }},
		{"snat-budget", "SNAT budget", func(s *Settings) { s.OutboundPortsPerNode = 32000 }},
		{"extra-zone", "exactly", func(s *Settings) { s.NodeZones["unknown"] = "1" }},
		{"api-needs-sources", "apiAdminCIDRs", func(s *Settings) { s.APILoadBalancer = true; s.APIDNSLabel = "test-api" }},
		{"api-invalid-source", "IPv4 CIDRs", func(s *Settings) {
			s.APILoadBalancer = true
			s.APIDNSLabel = "test-api"
			s.APIAdminCIDRs = []string{"invalid"}
		}},
		{"api-missing-san", "tls_sans", func(s *Settings) {
			s.APILoadBalancer = true
			s.APIDNSLabel = "other-api"
			s.APIAdminCIDRs = []string{"203.0.113.10/32"}
		}},
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
	outputs["identity"] = resource.NewObjectProperty(resource.PropertyMap{"type": resource.NewStringProperty("SystemAssigned"), "principalId": resource.NewStringProperty("33333333-3333-3333-3333-333333333333")})
	outputs["principalId"] = resource.NewStringProperty("33333333-3333-3333-3333-333333333333")
	outputs["ipAddress"] = resource.NewStringProperty("203.0.113.20")
	return "/mock/" + args.Name, outputs, nil
}

func (*mockAzure) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) {
	if args.Token == "azure-native:authorization:getClientConfig" {
		return resource.NewPropertyMapFromMap(map[string]any{"objectId": "11111111-1111-1111-1111-111111111111", "subscriptionId": "22222222-2222-2222-2222-222222222222"}), nil
	}
	return args.Args, nil
}

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
			if public {
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
				case "azure-native:network:NatGateway":
					t.Error("NAT Gateway must not be provisioned")
				case "azure-native:network:LoadBalancer":
					outbound := res.Inputs["outboundRules"].ArrayValue()[0].ObjectValue()
					if outbound["protocol"].StringValue() != "All" || outbound["allocatedOutboundPorts"].NumberValue() != 1024 {
						t.Error("SLB must provide explicit TCP/UDP egress with per-node SNAT allocation")
					}
				case "azure-native:compute:VirtualMachine":
					vms++
					image := plain(res.Inputs["storageProfile"]).ObjectValue()["imageReference"].ObjectValue()
					if image["offer"].StringValue() != "flatcar-container-linux-corevm-amd64" || !res.Inputs["plan"].IsNull() {
						t.Error("use the CoreVM image without a Marketplace purchase plan")
					}
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
					if value := res.Inputs["natGateway"]; !value.IsNull() {
						t.Error("subnet must use SLB egress")
					}
				case "azure-native:network:NetworkInterface":
					pools := res.Inputs["ipConfigurations"].ArrayValue()[0].ObjectValue()["loadBalancerBackendAddressPools"].ArrayValue()
					if len(pools) != 1 || !strings.HasSuffix(pools[0].ObjectValue()["id"].StringValue(), "/backendAddressPools/egress") {
						t.Error("every node must join the egress pool")
					}
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
			wantIPs, wantSSH := 1, 0 // One SLB public IP even when the nodes are private.
			if public {
				wantIPs, wantSSH = 1, 0
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

func TestPrivateCrossRegionArtifactStorage(t *testing.T) {
	d, err := loadDeployment(fixture(t, true))
	if err != nil {
		t.Fatal(err)
	}
	mocks := &mockAzure{}
	if err := pulumi.RunErr(func(ctx *pulumi.Context) error { return deploy(ctx, d) }, pulumi.WithMocks("test", "test", mocks)); err != nil {
		t.Fatal(err)
	}
	account, container, lock, role := 0, 0, 0, 0
	for _, r := range mocks.resources {
		switch r.TypeToken {
		case "azure-native:storage:StorageAccount":
			account++
			if r.Inputs["resourceGroupName"].StringValue() != "cluster" || r.Inputs["sku"].ObjectValue()["name"].StringValue() != "Standard_RAGZRS" {
				t.Error("artifact account must share the resource group and support secondary-region reads")
			}
			if r.Inputs["allowBlobPublicAccess"].BoolValue() || r.Inputs["allowSharedKeyAccess"].BoolValue() || !r.Inputs["enableHttpsTrafficOnly"].BoolValue() || r.Inputs["minimumTlsVersion"].StringValue() != "TLS1_2" || !r.RegisterRPC.GetProtect() {
				t.Error("artifact account must require authenticated HTTPS and be protected")
			}
		case "azure-native:storage:BlobContainer":
			container++
			if r.Inputs["publicAccess"].StringValue() != "None" {
				t.Error("kubeconfigs must not be publicly readable")
			}
		case "azure-native:authorization:ManagementLockAtResourceLevel":
			lock++
			if r.Inputs["level"].StringValue() != "CanNotDelete" || !r.RegisterRPC.GetProtect() {
				t.Error("retain recovery artifacts until explicitly unlocked")
			}
		case "azure-native:authorization:RoleAssignment":
			if r.Name != "artifact-publisher" {
				continue
			}
			role++
			if r.Inputs["scope"].StringValue() != "/mock/access-artifacts" || r.Inputs["principalId"].StringValue() != "11111111-1111-1111-1111-111111111111" || !strings.HasSuffix(r.Inputs["roleDefinitionId"].StringValue(), "/ba92f5b4-2d11-453d-a403-e96b0029c9fe") {
				t.Error("grant the deployment identity access only to the artifact container")
			}
		}
	}
	if account != 1 || container != 2 || lock != 1 || role != 1 {
		t.Fatalf("account=%d container=%d lock=%d role=%d", account, container, lock, role)
	}
}

func TestAPILoadBalancer(t *testing.T) {
	s := fixture(t, true)
	s.APILoadBalancer = true
	s.APIDNSLabel = "test-api"
	s.APIAdminCIDRs = []string{"198.51.100.10/32"}
	d, err := loadDeployment(s)
	if err != nil {
		t.Fatal(err)
	}
	mocks := &mockAzure{}
	if err := pulumi.RunErr(func(ctx *pulumi.Context) error { return deploy(ctx, d) }, pulumi.WithMocks("test", "test", mocks)); err != nil {
		t.Fatal(err)
	}
	lbCount, backends, apiRules, probeRules, clusterRules := 0, 0, 0, 0, 0
	for _, res := range mocks.resources {
		switch res.TypeToken {
		case "azure-native:network:LoadBalancer":
			lbCount++
			rule := res.Inputs["loadBalancingRules"].ArrayValue()[0].ObjectValue()
			if rule["protocol"].StringValue() != "Tcp" || rule["frontendPort"].NumberValue() != 6443 || rule["backendPort"].NumberValue() != 6443 || !rule["disableOutboundSnat"].BoolValue() {
				t.Error("API must use TCP passthrough on 6443, with separate explicit egress")
			}
			probe := res.Inputs["probes"].ArrayValue()[0].ObjectValue()
			if probe["protocol"].StringValue() != "Tcp" || probe["port"].NumberValue() != 6443 {
				t.Error("probe must check API listener")
			}
		case "azure-native:network:NetworkInterface":
			cfg := res.Inputs["ipConfigurations"].ArrayValue()[0].ObjectValue()
			pool := cfg["loadBalancerBackendAddressPools"]
			for _, entry := range pool.ArrayValue() {
				if strings.HasSuffix(entry.ObjectValue()["id"].StringValue(), "/backendAddressPools/servers") {
					backends++
					if res.Name == "node-worker1" {
						t.Error("workers must not be API backends")
					}
				}
			}
		case "azure-native:network:NetworkSecurityGroup":
			for _, item := range res.Inputs["securityRules"].ArrayValue() {
				rule := item.ObjectValue()
				switch rule["name"].StringValue() {
				case "management-api":
					apiRules++
					if rule["destinationPortRange"].StringValue() != "6443" || rule["sourceAddressPrefixes"].ArrayValue()[0].StringValue() != "198.51.100.10/32" {
						t.Error("only the API port should use the explicitly configured public source range")
					}
				case "cluster-public-api":
					clusterRules++
					if rule["sourceAddressPrefix"].StringValue() != "203.0.113.20" || rule["destinationPortRange"].StringValue() != "6443" || rule["access"].StringValue() != "Allow" || rule["protocol"].StringValue() != "Tcp" || rule["direction"].StringValue() != "Inbound" || rule["priority"].NumberValue() >= 200 {
						t.Error("allow the cluster egress IP to reach the public API without widening operator access")
					}
				case "api-health-probe":
					probeRules++
					if rule["sourceAddressPrefix"].StringValue() != "AzureLoadBalancer" || rule["destinationPortRange"].StringValue() != "6443" {
						t.Error("allow only Azure API health probes")
					}
				}
			}
		}
	}
	if lbCount != 1 || backends != 3 || apiRules != 1 || probeRules != 1 || clusterRules != 1 {
		t.Fatalf("load balancer=%d backends=%d api rules=%d probe rules=%d cluster rules=%d", lbCount, backends, apiRules, probeRules, clusterRules)
	}
}

func TestTemporaryDatabaseStorage(t *testing.T) {
	s := fixture(t, true)
	data, err := os.ReadFile(s.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	var cluster map[string]any
	if err := json.Unmarshal(data, &cluster); err != nil {
		t.Fatal(err)
	}
	cluster["cockroach_storage"] = "azure-temporary"
	for _, raw := range cluster["nodes"].([]any) {
		node := raw.(map[string]any)
		node["locality"] = "cloud=azure,region=westus2,zone=" + s.NodeZones[node["name"].(string)]
	}
	writeJSON(t, s.ClusterPath, cluster)
	cfg, err := clusterconfig.Load(s.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := ignition.Generate(cfg, filepath.Dir(s.ClusterPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeployment(s); err != nil {
		t.Fatal(err)
	}
	s.NodeZones["kube1"] = "2"
	if _, err := loadDeployment(s); err == nil || !strings.Contains(err.Error(), "locality must match") {
		t.Fatalf("expected locality mismatch, got %v", err)
	}
	s.NodeZones["kube1"] = "1"
	doc := ignitionDocument{}
	path := filepath.Join(s.IgnitionDir, "kube1.ign")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for i := range doc.Systemd.Units {
		doc.Systemd.Units[i].Contents = strings.ReplaceAll(doc.Systemd.Units[i].Contents, "src=/mnt/resource/cockroach,", "src=/var/lib/cockroach,")
	}
	writeJSON(t, path, doc)
	if _, err := loadDeployment(s); err == nil || !strings.Contains(err.Error(), "temporary disk profile missing") {
		t.Fatalf("expected OS-disk fallback rejection, got %v", err)
	}
}

func TestUniformPools(t *testing.T) {
	root := t.TempDir()
	c := testutil.Config()
	c.TLSSANs = []string{"test-api.westus2.cloudapp.azure.com", "192.168.1.4"}
	for i := range c.Nodes {
		c.Nodes[i].Name = fmt.Sprintf("api-%d", i)
	}
	if err := pki.Create(c, root); err != nil {
		t.Fatal(err)
	}
	credential, err := pki.BootstrapCredential(root)
	if err != nil {
		t.Fatal(err)
	}
	pool := &nodebootstrap.AzurePool{Name: "test", Role: "api", Zone: "test.westus2.internal", Location: "westus2", Subnet: "192.168.1.0/24", APICount: 3, WorkerDatabase: true}
	if err := ignition.Generate(c, root, ignition.Bootstrap{Account: "inf0123456789", HelperSHA256: strings.Repeat("a", 64), Credential: credential, APIEndpoint: "https://test-api.westus2.cloudapp.azure.com:6443", Pool: pool}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "cluster.json")
	writeJSON(t, path, c)
	s := Settings{Name: "test", Location: "westus2", VMSize: "Standard_D2d_v4", FlatcarVersion: "4757.2.1", ClusterPath: path, IgnitionDir: filepath.Join(root, "output"), VNetCIDR: "192.168.0.0/16", SubnetCIDR: "192.168.1.0/24", OSDiskSizeGB: 32, APILoadBalancer: true, APIDNSLabel: "test-api", APIAdminCIDRs: []string{"0.0.0.0/0"}, Uniform: true, PrivateDNSZone: pool.Zone, APICount: 3}
	d, err := loadDeployment(s)
	if err != nil {
		t.Fatal(err)
	}
	mocks := &mockAzure{}
	if err := pulumi.RunErr(func(ctx *pulumi.Context) error { return deploy(ctx, d) }, pulumi.WithMocks("test", "uniform", mocks)); err != nil {
		t.Fatal(err)
	}
	pools, ips := 0, 0
	for _, res := range mocks.resources {
		if res.TypeToken == "azure-native:network:NetworkSecurityGroup" {
			deny, internal := 0, 0
			for _, r := range res.Inputs["securityRules"].ArrayValue() {
				rule := r.ObjectValue()
				priority := int(rule["priority"].NumberValue())
				if priority < 100 || priority > 4096 {
					t.Error("invalid Azure rule priority")
				}
				if rule["name"].StringValue() == "deny-ssh" {
					deny = priority
				}
				if rule["name"].StringValue() == "cluster-internal" {
					internal = priority
				}
			}
			if deny == 0 || deny >= internal {
				t.Error("SSH deny must precede internal allow")
			}
		}
		switch res.TypeToken {
		case "azure-native:compute:VirtualMachine", "azure-native:network:NetworkInterface":
			t.Error("individual VM/NIC in Uniform deployment")
		case "azure-native:network:PublicIPAddress":
			ips++
		case "azure-native:compute/v20260301:VirtualMachineScaleSet":
			pools++
			if res.Inputs["orchestrationMode"].StringValue() != "Uniform" || res.Inputs["overprovision"].BoolValue() || !res.Inputs["zones"].IsNull() {
				t.Error("wrong orchestration/placement")
			}
			if res.Inputs["placement"].ObjectValue()["zonePlacementPolicy"].StringValue() != "auto" {
				t.Error("missing automatic zone placement")
			}
			profile := plain(res.Inputs["virtualMachineProfile"]).ObjectValue()
			osProfile := plain(profile["osProfile"]).ObjectValue()
			if !osProfile["customData"].IsSecret() && !profile["osProfile"].IsSecret() && !res.Inputs["virtualMachineProfile"].IsSecret() {
				t.Error("Ignition must be secret")
			}
			nic := profile["networkProfile"].ObjectValue()["networkInterfaceConfigurations"].ArrayValue()[0].ObjectValue()
			ip := nic["ipConfigurations"].ArrayValue()[0].ObjectValue()
			if !ip["privateIPAddress"].IsNull() || !ip["publicIPAddressConfiguration"].IsNull() {
				t.Error("must use DHCP and private NICs")
			}
			backends := len(ip["loadBalancerBackendAddressPools"].ArrayValue())
			want := 1
			if res.Name == "api-pool" {
				want = 2
			}
			if backends != want {
				t.Error("wrong backend membership")
			}
		}
	}
	if pools != 2 || ips != 1 {
		t.Fatalf("got %d VMSS and %d public IPs", pools, ips)
	}
}
