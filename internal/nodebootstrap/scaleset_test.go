package nodebootstrap

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infestation/internal/cluster"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
	"infestation/internal/tailnet"
	"infestation/internal/testutil"
)

func scaleMetadata(ordinal string) Metadata {
	var m Metadata
	m.Compute.SubscriptionID = "sub"
	m.Compute.ResourceGroupName = "pool-rg"
	m.Compute.Location = "westus2"
	m.Compute.VMScaleSetName = "pool"
	m.Compute.ResourceID = "/subscriptions/sub/resourceGroups/pool-rg/providers/Microsoft.Compute/virtualMachineScaleSets/pool/virtualMachines/" + ordinal
	return m
}

type identityRunner struct{ calls []string }

func (r *identityRunner) Run(_ context.Context, name string, args ...string) executil.Result {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return executil.Result{}
}
func TestScaleSetInstancesHaveUniqueIdentityAndOnlyZeroCanInit(t *testing.T) {
	for _, ordinal := range []string{"0", "1", "17"} {
		root := t.TempDir()
		c := testutil.Config()
		c.DiscoverPeers = true
		c.Tailscale = &cluster.Tailscale{Domain: "example.ts.net", Version: "v1.104.2"}
		c.Nodes = c.Nodes[:1]
		c.Nodes[0].Name = "pool-0"
		cfg := Config{Cluster: c, Node: "pool-0", ScaleSet: &ScaleSet{Name: "pool", Group: "pool-rg", Location: "westus2", Initialize: true}}
		if err := fsutil.WriteFile(filepath.Join(root, tailnet.K3sPath), []byte("node-name: pool-0\ntls-san:\n  - pool-0.example.ts.net\n"), 0600); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"client.root.crt", "client.root.key"} {
			if err := fsutil.WriteFile(filepath.Join(root, BootstrapCredentialDir, name), []byte("short-lived-test-credential"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		r := &identityRunner{}
		if err := prepareScaleSet(context.Background(), cfg, scaleMetadata(ordinal), root, r); err != nil {
			t.Fatal(err)
		}
		got, err := Load(filepath.Join(root, "/etc/infestation/bootstrap.json"))
		if err != nil || got.Node != "pool-"+ordinal || got.Cluster.Nodes[0].Init != (ordinal == "0") {
			t.Fatal("wrong instance identity/authority", got.Node, err)
		}
		var tc tailnet.Config
		if err := fsutil.ReadJSON(filepath.Join(root, tailnet.ConfigPath), &tc); err != nil || tc.DNSName != "pool-"+ordinal+".example.ts.net" {
			t.Fatal("tailnet identity was not updated", err)
		}
		unit, _ := os.ReadFile(filepath.Join(root, "/etc/systemd/system/infestation-bootstrap.service.d/20-instance-authority.conf"))
		if strings.Contains(string(unit), " --init") != (ordinal == "0") {
			t.Fatal("nonzero instance retained init command")
		}
		_, err = os.Stat(filepath.Join(root, BootstrapCredentialDir, "client.root.key"))
		if ordinal == "0" && err != nil {
			t.Fatal("initializer lost temporary credential")
		}
		if ordinal != "0" && !os.IsNotExist(err) {
			t.Fatal("non-initializer retained SQL root credential")
		}
	}
}
func TestScaleSetRejectsWrongOrMalformedMetadata(t *testing.T) {
	s := ScaleSet{Name: "pool", Group: "pool-rg", Location: "westus2", Initialize: true}
	for _, change := range []func(*Metadata){
		func(m *Metadata) { m.Compute.VMScaleSetName = "other" }, func(m *Metadata) { m.Compute.ResourceGroupName = "other-rg" }, func(m *Metadata) { m.Compute.Location = "eastus" }, func(m *Metadata) {
			m.Compute.ResourceID = strings.Replace(m.Compute.ResourceID, "/sub/", "/foreign/", 1)
		}, func(m *Metadata) {
			m.Compute.ResourceID = strings.Replace(m.Compute.ResourceID, "/0", "/not-an-ordinal", 1)
		},
	} {
		m := scaleMetadata("0")
		change(&m)
		if _, err := s.node(m, cluster.Node{Name: "pool-0"}); err == nil {
			t.Fatal("foreign metadata accepted")
		}
	}
	if _, err := s.node(scaleMetadata("0"), cluster.Node{Name: "pool-0", Role: "worker"}); err == nil {
		t.Fatal("agent received init authority")
	}
	s.Initialize = false
	if n, err := s.node(scaleMetadata("0"), cluster.Node{Name: "pool-0", Role: "worker"}); err != nil || n.Init || !n.Worker() {
		t.Fatal("agent role not preserved", err)
	}
}
