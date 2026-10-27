package ignition

import (
	"encoding/base64"
	"encoding/json"
	"infestation/internal/nodebootstrap"
	"infestation/internal/pki"
	"infestation/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUniformProfiles(t *testing.T) {
	c := testutil.Config()
	c.Nodes[0], c.Nodes[2] = c.Nodes[2], c.Nodes[0]
	c.CockroachStorage = "azure-temporary"
	state := t.TempDir()
	if err := pki.Create(c, state); err != nil {
		t.Fatal(err)
	}
	cred, err := pki.BootstrapCredential(state)
	if err != nil {
		t.Fatal(err)
	}
	b := Bootstrap{Account: "inf0123456789", HelperSHA256: strings.Repeat("a", 64), Credential: cred, APIEndpoint: "https://test-api.westus2.cloudapp.azure.com:6443", Pool: &nodebootstrap.AzurePool{Name: "test", Role: "api", Zone: "test.westus2.internal", Location: "westus2", Subnet: "192.168.1.0/24", APICount: 3, WorkerDatabase: true}}
	if err := Generate(c, state, b); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"api", "worker"} {
		raw, err := os.ReadFile(filepath.Join(state, "output", role+".ign"))
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > 65535 {
			t.Fatal("oversize profile")
		}
		var doc struct {
			Storage struct {
				Files []struct {
					Path     string
					Contents struct{ Source string }
				}
			}
			Systemd struct {
				Units []struct{ Name, Contents string }
			}
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		root := false
		prep := false
		for _, f := range doc.Storage.Files {
			if strings.Contains(f.Path, "client.root.key") {
				root = true
			}
			if strings.HasPrefix(f.Path, "/usr/") {
				t.Fatal("generated file under read-only /usr")
			}
			if f.Path == "/etc/hostname" || strings.HasSuffix(f.Path, "ca.key") {
				t.Fatal("hostname or CA signing key in shared profile")
			}
			data, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(f.Contents.Source, "data:;base64,"))
			if f.Path == "/etc/infestation/bootstrap.json" {
				var cfg nodebootstrap.Config
				_ = json.Unmarshal(data, &cfg)
				if cfg.Azure.Role != role || cfg.Azure.Token == "" {
					t.Fatal("wrong pool runtime config")
				}
			}
		}
		for _, u := range doc.Systemd.Units {
			if u.Name == "infestation-prepare.service" {
				prep = true
			}
			if u.Name == "cockroachdb.service" && (!strings.Contains(u.Contents, "${INFESTATION_LOCALITY}") || !strings.Contains(u.Contents, "${INFESTATION_ADDRESS}")) {
				t.Fatal("static locality/address in pool")
			}
		}
		if !prep || root != (role == "api") {
			t.Fatal("prepare/credential role scope", role, prep, root)
		}
	}
}
