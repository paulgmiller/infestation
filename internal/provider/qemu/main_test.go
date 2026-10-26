package qemu

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	clusterconfig "infestation/internal/cluster"
	"infestation/internal/ignition"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource"
	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

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

// Generate profiles with the shared Go generator and disposable fake PKI.
func fixture(t *testing.T) Settings {
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
	cluster["architecture"] = "x86-64"
	delete(cluster, "single_node_lab")
	cluster["ssh_authorized_key"] = "ssh-ed25519 " + base64.StdEncoding.EncodeToString(append([]byte("\x00\x00\x00\x0bssh-ed25519\x00\x00\x00\x20"), make([]byte, 32)...))
	nodes := []map[string]any{}
	for i := 1; i <= 3; i++ {
		nodes = append(nodes, map[string]any{"name": fmt.Sprintf("kube%d", i), "ip": fmt.Sprintf("192.168.124.%d", 10+i)})
	}
	nodes = append(nodes, map[string]any{"name": "worker1", "ip": "192.168.124.14", "role": "worker"})
	cluster["nodes"] = nodes
	path := filepath.Join(root, "cluster.json")
	writeJSON(t, path, cluster)
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
	cfg, err := clusterconfig.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := ignition.Generate(cfg, root); err != nil {
		t.Fatal(err)
	}
	header := make([]byte, 32)
	copy(header, []byte("QFI\xfb"))
	binary.BigEndian.PutUint32(header[4:8], 3)
	binary.BigEndian.PutUint64(header[24:32], 8*1024*1024*1024)
	image := filepath.Join(root, "fake-base.qcow2")
	if err := os.WriteFile(image, header, 0600); err != nil {
		t.Fatal(err)
	}
	return Settings{
		Name: "test", ClusterPath: path, IgnitionDir: filepath.Join(root, "output"), BaseImagePath: image,
		PoolDir: "/var/lib/libvirt/images/test", NetworkCIDR: "192.168.124.0/24", QEMUUserID: "1000", QEMUGroupID: "1000",
		MemoryMiB: 3072, VCPUs: 2, DiskGiB: 40, Running: true,
	}
}

func TestInvalidConfiguration(t *testing.T) {
	cases := []struct {
		name, want string
		change     func(*Settings)
	}{
		{"wrong-subnet", "usable IP", func(s *Settings) { s.NetworkCIDR = "192.168.125.0/24" }},
		{"public-network", "IPv4 subnet", func(s *Settings) { s.NetworkCIDR = "203.0.113.0/24" }},
		{"pod-overlap", "overlaps", func(s *Settings) { s.NetworkCIDR = "10.42.0.0/16" }},
		{"noncanonical-network", "IPv4 subnet", func(s *Settings) { s.NetworkCIDR = "192.168.124.1/24" }},
		{"missing-owner", "qemuUserID", func(s *Settings) { s.QEMUUserID = "" }},
		{"pool-root", "dedicated", func(s *Settings) { s.PoolDir = "/" }},
		{"disk-too-small", "virtual capacity", func(s *Settings) {
			b := make([]byte, 32)
			copy(b, []byte("QFI\xfb"))
			binary.BigEndian.PutUint64(b[24:], 80*1024*1024*1024)
			if err := os.WriteFile(s.BaseImagePath, b, 0600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := fixture(t)
			tc.change(&s)
			if _, err := loadDeployment(s); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestStaleProfilesAndSingleNodeDatabaseGuard(t *testing.T) {
	s := fixture(t)
	path := filepath.Join(s.IgnitionDir, "worker1.ign")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(data), "k3s-agent.service", "k3s.service")), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDeployment(s); err == nil || !strings.Contains(err.Error(), "K3s role") {
		t.Fatalf("expected role mismatch, got %v", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(s.ClusterPath)
	if err != nil {
		t.Fatal(err)
	}
	var cluster map[string]any
	if err := json.Unmarshal(data, &cluster); err != nil {
		t.Fatal(err)
	}
	cluster["single_node_lab"] = true
	nodes := cluster["nodes"].([]any)
	cluster["nodes"] = []any{nodes[0], nodes[len(nodes)-1]}
	writeJSON(t, s.ClusterPath, cluster)
	if _, err := loadDeployment(s); err == nil || !strings.Contains(err.Error(), "one database host") {
		t.Fatalf("expected guard against independent databases, got %v", err)
	}
}

type mocks struct {
	mu        sync.Mutex
	resources []pulumi.MockResourceArgs
}

func (m *mocks) NewResource(args pulumi.MockResourceArgs) (string, resource.PropertyMap, error) {
	m.mu.Lock()
	m.resources = append(m.resources, args)
	m.mu.Unlock()
	outputs := args.Inputs.Copy()
	outputs["path"] = resource.NewStringProperty("/pool/" + args.Name)
	return "mock-" + args.Name, outputs, nil
}
func (*mocks) Call(args pulumi.MockCallArgs) (resource.PropertyMap, error) { return args.Args, nil }

func TestLibvirtResources(t *testing.T) {
	s := fixture(t)
	d, err := loadDeployment(s)
	if err != nil {
		t.Fatal(err)
	}
	m := &mocks{}
	if err := pulumi.RunErr(func(ctx *pulumi.Context) error { return deploy(ctx, d) }, pulumi.WithMocks("test", "test", m)); err != nil {
		t.Fatal(err)
	}
	domains, volumes, networks := 0, 0, 0
	reservations := map[string]string{}
	for _, res := range m.resources {
		switch res.TypeToken {
		case "libvirt:index/network:Network":
			networks++
			if !res.RegisterRPC.GetProtect() {
				t.Error("prevent implicit DHCP network replacement")
			}
			if res.Inputs["forward"].ObjectValue()["mode"].StringValue() != "nat" {
				t.Error("VMs need NAT egress for first boot")
			}
			ip := res.Inputs["ips"].ArrayValue()[0].ObjectValue()
			if ip["address"].StringValue() != "192.168.124.1" {
				t.Error("reserve subnet's first host for the gateway")
			}
			for _, value := range ip["dhcp"].ObjectValue()["hosts"].ArrayValue() {
				host := value.ObjectValue()
				reservations[host["mac"].StringValue()] = host["ip"].StringValue()
			}
		case "libvirt:index/volume:Volume":
			volumes++
			if !res.RegisterRPC.GetProtect() || !slices.Contains(res.RegisterRPC.GetIgnoreChanges(), "*") {
				t.Error("preserve immutable base, Ignition, and persistent writable disks")
			}
			permissions := res.Inputs["target"].ObjectValue()["permissions"].ObjectValue()
			if permissions["mode"].StringValue() != "0600" || permissions["owner"].StringValue() != "1000" {
				t.Error("only the QEMU account should read disk/credential volumes")
			}
			if strings.HasSuffix(res.Name, "-disk") && res.Inputs["backingStore"].ObjectValue()["path"].StringValue() != "/pool/base" {
				t.Error("use separate copy-on-write disks backed by the immutable image")
			}
		case "libvirt:index/domain:Domain":
			domains++
			if !res.RegisterRPC.GetProtect() {
				t.Error("protect VM deletion/replacement")
			}
			if !res.Inputs["running"].BoolValue() || res.Inputs["autostart"].BoolValue() {
				t.Error("start VMs on up, but do not enable host-boot autostart by default")
			}
			if !res.Inputs["features"].ObjectValue()["acpi"].BoolValue() {
				t.Error("q35 fw_cfg requires ACPI")
			}
			fwcfg := res.Inputs["sysInfos"].ArrayValue()[0].ObjectValue()["fwCfg"].ObjectValue()["entries"].ArrayValue()[0].ObjectValue()
			if fwcfg["name"].StringValue() != "opt/org.flatcar-linux/config" || fwcfg["file"].StringValue() != "/pool/"+res.Name+"-ignition" {
				t.Error("use the corresponding node's Flatcar fw_cfg file")
			}
			iface := res.Inputs["devices"].ObjectValue()["interfaces"].ArrayValue()[0].ObjectValue()
			if iface["mac"].ObjectValue()["address"].StringValue() != nodeMAC(s.Name, res.Name) {
				t.Error("domain MAC must match its DHCP reservation")
			}
			if iface["source"].ObjectValue()["network"].ObjectValue()["network"].StringValue() != s.Name {
				t.Error("connect every VM to the shared cluster network")
			}
		}
	}
	if domains != 4 || volumes != 9 || networks != 1 || len(reservations) != 4 {
		t.Fatalf("got %d domains, %d volumes, %d networks, %d reservations", domains, volumes, networks, len(reservations))
	}
	if reservations[nodeMAC(s.Name, "worker1")] != "192.168.124.14" {
		t.Error("worker address must match its Ignition and certificates")
	}
	data, err := os.ReadFile(filepath.Join(d.Settings.IgnitionDir, "worker1.ign"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "cockroachdb.service") || !strings.Contains(string(data), "k3s-agent.service") {
		t.Error("workers keep the host database and K3s agent")
	}
}
