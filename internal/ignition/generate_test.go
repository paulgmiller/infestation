package ignition

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	config "github.com/coreos/ignition/v2/config/v3_3"
	"infestation/internal/cluster"
	"infestation/internal/testutil"
)

func TestProfilesAndCredentials(t *testing.T) {
	c := testutil.Config()
	off := false
	c.Nodes = append(c.Nodes, cluster.Node{Name: "worker1", IP: "192.168.1.104", Role: "worker"}, cluster.Node{Name: "worker2", IP: "192.168.1.105", Role: "worker", CockroachDB: &off})
	c.TLSSANs = []string{"api.example.test"}
	c.CockroachStorage = "azure-temporary"
	c.Nodes[0].Locality = "cloud=azure,region=westus2,zone=1"
	state := t.TempDir()
	testutil.FakePKI(t, c, state)
	if err := Generate(c, state); err != nil {
		t.Fatal(err)
	}
	for _, n := range c.Nodes {
		raw, err := os.ReadFile(filepath.Join(state, "output", n.Name+".ign"))
		if err != nil {
			t.Fatal(err)
		}
		doc, rpt, err := config.Parse(raw)
		if err != nil {
			t.Fatal(err, rpt)
		}
		files := map[string]string{}
		for _, file := range doc.Storage.Files {
			source := *file.Contents.Source
			if strings.HasPrefix(source, "data:;base64,") {
				decoded, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(source, "data:;base64,"))
				files[file.Path] = string(decoded)
			}
			if file.Path == "/etc/cockroach/certs/node.key" && (*file.Mode != 0400 || file.User.ID == nil || *file.User.ID != 1000) {
				t.Fatal("database key must be owned by UID 1000")
			}
			if strings.Contains(file.Path, "ca.key") || strings.Contains(file.Path, "client.root") {
				t.Fatal("admin credential uploaded")
			}
		}
		conf := files["/etc/rancher/k3s/config.yaml"]
		if n.Worker() {
			if !strings.Contains(conf, `server: "https://192.168.1.101:6443"`) || strings.Contains(conf, "datastore-") || strings.Contains(conf, "tls-san:") {
				t.Fatal("worker profile contains server options")
			}
		}
		if !n.Worker() && (!strings.Contains(conf, `  - "api.example.test"`) || !strings.Contains(conf, "sslmode=verify-full")) {
			t.Fatal("missing server TLS options")
		}
		db := false
		for _, unit := range doc.Systemd.Units {
			if unit.Name == "cockroachdb.service" {
				db = true
				if !strings.Contains(*unit.Contents, "src=/mnt/resource/cockroach,") || !strings.Contains(*unit.Contents, "ExecStartPre=/usr/local/sbin/check-cockroach-storage") {
					t.Fatal("must not fall back to OS disk")
				}
			}
		}
		if db != n.Database() {
			t.Fatal("worker database default/opt-out changed")
		}
		info, _ := os.Stat(filepath.Join(state, "output", n.Name+".ign"))
		if info.Mode().Perm() != 0600 {
			t.Fatal("Ignition must remain private")
		}
	}
	tokenBefore, _ := os.ReadFile(filepath.Join(state, "pki/k3s-token"))
	if err := Generate(c, state); err != nil {
		t.Fatal(err)
	}
	tokenAfter, _ := os.ReadFile(filepath.Join(state, "pki/k3s-token"))
	if string(tokenBefore) != string(tokenAfter) {
		t.Fatal("token replaced")
	}
}
func TestWorkersOnlyRequireExistingToken(t *testing.T) {
	c := testutil.Config()
	c.Nodes = []cluster.Node{{Name: "worker", IP: "192.0.2.1", Role: "worker", ServerURL: "https://api.example.test:6443"}}
	if _, err := Token(c, t.TempDir()); err == nil {
		t.Fatal("must require original cluster token")
	}
}
