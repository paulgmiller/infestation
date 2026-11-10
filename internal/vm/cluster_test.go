package vm

import (
	"encoding/base64"
	"encoding/json"
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
	c := clusterConfig(ClusterOptions{Options: Options{Name: "isolated"}, Domain: "example.ts.net"})
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
	if err := ignition.Generate(c, state, ignition.Bootstrap{LocalHelper: helper, Credential: credential, TailscaleAuthKey: "tskey-auth-TEST"}); err != nil {
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
