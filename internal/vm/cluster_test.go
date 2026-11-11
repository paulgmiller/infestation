package vm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	config "github.com/coreos/ignition/v2/config/v3_3"
	"infestation/internal/ignition"
	"infestation/internal/nodebootstrap"
	"infestation/internal/pki"
)

func TestClusterProfileKeepsCredentialsLocalAndNodesIsolated(t *testing.T) {
	c := clusterConfig(ClusterOptions{Size: 3, Init: true, Options: Options{Name: "isolated"}, Domain: "example.ts.net"})
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(c.Nodes) != 3 || len(c.Servers()) != 3 || len(c.Databases()) != 3 {
		t.Fatal("wrong quorum topology")
	}
	state := t.TempDir()
	helper := filepath.Join(state, "helper")
	if err := os.WriteFile(helper, []byte("test-helper"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := pki.Create(c, state); err != nil {
		t.Fatal(err)
	}
	credential, err := pki.BootstrapCredential(state)
	if err != nil {
		t.Fatal(err)
	}
	shared, err := pki.PoolCertificate(state, c.Tailscale.Domain)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range shared {
		if err := os.WriteFile(filepath.Join(state, "pki", c.Nodes[0].Name, name), data, 0600); err != nil {
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
	if err := ignition.Generate(c, state, ignition.Bootstrap{JoinBundle: bundle, LocalHelper: helper, Credential: credential, TailscaleAuthKey: "tskey-auth-TEST"}); err != nil {
		t.Fatal(err)
	}
	caKey, _ := os.ReadFile(filepath.Join(state, "pki", "ca.key"))
	adminKey, _ := os.ReadFile(filepath.Join(state, "pki", "admin", "client.root.key"))
	for _, n := range c.Nodes {
		raw, err := os.ReadFile(filepath.Join(state, "output", n.Name+".ign"))
		if err != nil {
			t.Fatal(err)
		}
		raw, err = addClusterChannels(raw)
		if err != nil {
			t.Fatal(err)
		}
		doc, report, err := config.Parse(raw)
		if err != nil {
			t.Fatal(err, report)
		}
		foundDiagnostic := false
		for _, u := range doc.Systemd.Units {
			if u.Name == "infestation-diagnostics.timer" && u.Enabled != nil && *u.Enabled {
				foundDiagnostic = true
			}
		}
		if !foundDiagnostic {
			t.Fatal("missing private diagnostics")
		}
		rootCredential := false
		for _, f := range doc.Storage.Files {
			if f.Contents.Source == nil {
				continue
			}
			s, ok := strings.CutPrefix(*f.Contents.Source, "data:;base64,")
			if !ok {
				continue
			}
			data, _ := base64.StdEncoding.DecodeString(s)
			if strings.Contains(string(data), string(caKey)) || strings.Contains(string(data), string(adminKey)) {
				t.Fatal("workstation signing/admin credential embedded in VM")
			}
			if f.Path == nodebootstrap.BootstrapCredentialDir+"/client.root.key" {
				rootCredential = true
				if !strings.HasSuffix(n.Name, "-0") {
					t.Fatal("non-initializer received SQL root credential")
				}
			}
		}
		if strings.HasSuffix(n.Name, "-0") && !rootCredential {
			t.Fatal("initializer lost its one-hour SQL credential")
		}
		args := strings.Join(arguments(filepath.Join(state, n.Name), machine{Name: n.Name, Accel: "tcg", Network: "slirp", MemoryMiB: 3072}), " ")
		if !strings.Contains(args, "-netdev user,id=egress") || strings.Contains(args, "hostfwd") || strings.Contains(args, "type=tap") || strings.Contains(args, "hubport") {
			t.Fatal("VM underlay not isolated", args)
		}
		if !strings.Contains(args, "-m 3072") {
			t.Fatal("cluster memory budget missing")
		}
	}
	// Inventory persistence must never capture the transient enrollment key.
	raw, _ := json.Marshal(c)
	if strings.Contains(string(raw), "tskey-auth-") {
		t.Fatal("auth key in inventory")
	}
}

func TestProviderGroupInventoryContainsOnlyOwnedNodes(t *testing.T) {
	for _, tc := range []struct {
		name, provider string
		size           int
		init           bool
	}{{"miller", "qemu", 2, true}, {"locke", "azure", 1, false}} {
		c := clusterConfig(ClusterOptions{Options: Options{Name: tc.name}, Domain: "example.ts.net", Provider: tc.provider, Size: tc.size, Init: tc.init})
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if len(c.Nodes) != tc.size {
			t.Fatal("inventory contains other provider nodes")
		}
		marked := 0
		for _, n := range c.Nodes {
			if n.Init {
				marked++
			}
		}
		if (tc.init && marked != 1) || (!tc.init && marked != 0) {
			t.Fatal("incorrect bootstrap authority")
		}
	}
}

func TestIndependentJoiningProfileNeedsNoWorkstationPKI(t *testing.T) {
	c := clusterConfig(ClusterOptions{Options: Options{Name: "friend"}, Domain: "example.ts.net", Size: 1})
	state := t.TempDir()
	helper := filepath.Join(state, "helper")
	if err := os.WriteFile(helper, []byte("disposable-helper"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := ignition.Generate(c, state, ignition.Bootstrap{LocalHelper: helper, TailscaleAuthKey: "tskey-auth-TEST"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "pki")); !os.IsNotExist(err) {
		t.Fatal("joiner generated another CA or token")
	}
	raw, err := os.ReadFile(filepath.Join(state, "output", "friend-0.ign"))
	if err != nil {
		t.Fatal(err)
	}
	doc, report, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err, report)
	}
	for _, f := range doc.Storage.Files {
		if strings.HasPrefix(f.Path, "/etc/cockroach/certs/") || f.Path == nodebootstrap.JoinBundlePath || strings.HasPrefix(f.Path, nodebootstrap.BootstrapCredentialDir+"/") {
			t.Fatal("joiner profile contains founding credentials", f.Path)
		}
	}
	units := map[string]string{}
	for _, u := range doc.Systemd.Units {
		if u.Contents != nil {
			units[u.Name] = *u.Contents
		}
	}
	if !strings.Contains(units["infestation-join.service"], "infestation-node enroll") || !strings.Contains(units["infestation-membership.service"], "infestation-node membership") {
		t.Fatal("missing independent enrollment services")
	}
	if strings.Contains(units["infestation-bootstrap.service"], " --init") {
		t.Fatal("joiner acquired initialization authority")
	}
	if !strings.Contains(units["cockroachdb.service"], "--join=${INFESTATION_SEEDS}") {
		t.Fatal("database join list is frozen in creator inventory")
	}
}

func TestLocalLifecycleRejectsAzureOwnershipBeforeSideEffects(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cloud")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	c := clusterConfig(ClusterOptions{Options: Options{Name: "cloud"}, Size: 1, Domain: "example.ts.net"})
	raw, _ := json.Marshal(c)
	if err := os.WriteFile(filepath.Join(dir, "cluster.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "provider.json"), []byte(`"azure"`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	for _, action := range []string{"start", "stop", "status"} {
		if err := ClusterAction(context.Background(), Options{Name: "cloud", StateDir: root}, action, io.Discard); err == nil || !strings.Contains(err.Error(), "use azure") {
			t.Fatal("local command crossed provider boundary", action, err)
		}
	}
}
