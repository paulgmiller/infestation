package ignition

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	config "github.com/coreos/ignition/v2/config/v3_3"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/nodebootstrap"
	"github.com/paulgmiller/infestation/internal/pki"
	"github.com/paulgmiller/infestation/internal/testutil"
	"gopkg.in/yaml.v3"
)

func TestTemporarySQLCredentialOnlyOnExplicitInitializer(t *testing.T) {
	c := testutil.Config()
	c.Nodes[0].Name = "miller"
	c.Nodes = append(c.Nodes, cluster.Node{Name: "worker-0", IP: "192.168.1.104", Role: "worker"})
	state := t.TempDir()
	if err := pki.Create(c, state); err != nil {
		t.Fatal(err)
	}
	credential, err := pki.BootstrapCredential(state)
	if err != nil {
		t.Fatal(err)
	}
	options := Bootstrap{HelperSourceCommit: strings.Repeat("a", 40), DeploymentID: "test", APIEndpoint: "https://api.example.test:6443", Credential: credential}
	if err := Generate(c, state, options); err != nil {
		t.Fatal(err)
	}
	for _, n := range c.Nodes {
		raw, err := os.ReadFile(filepath.Join(state, "output", n.Name+".ign"))
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) > 65535 {
			t.Fatalf("%s exceeds Azure custom-data limit", n.Name)
		}
		doc, report, err := config.Parse(raw)
		if err != nil {
			t.Fatal(err, report)
		}
		for _, unit := range doc.Systemd.Units {
			if unit.Name == "infestation-bootstrap.service" && strings.Contains(*unit.Contents, " --init\n") != n.Init {
				t.Fatal("--init must be passed only to the explicitly selected server")
			}
		}
		count := 0
		chartFound := false
		for _, file := range doc.Storage.Files {
			data, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(*file.Contents.Source, "data:;base64,"))
			if file.Path == "/etc/rancher/k3s/config.yaml" && strings.Contains(string(data), "disable-kube-proxy: true\n") == n.Worker() {
				t.Fatal("disable-kube-proxy must be set only on servers; agents inherit it")
			}
			if file.Path == "/var/lib/rancher/k3s/server/manifests/infestation-cilium.yaml" {
				chartFound = true
				var chart struct {
					Spec struct {
						Bootstrap bool   `yaml:"bootstrap"`
						Values    string `yaml:"valuesContent"`
					} `yaml:"spec"`
				}
				if err := yaml.Unmarshal(data, &chart); err != nil {
					t.Fatal(err)
				}
				var values struct {
					Replacement bool   `yaml:"kubeProxyReplacement"`
					Host        string `yaml:"k8sServiceHost"`
					Port        int    `yaml:"k8sServicePort"`
				}
				if err := yaml.Unmarshal([]byte(chart.Spec.Values), &values); err != nil {
					t.Fatal(err)
				}
				if !chart.Spec.Bootstrap || !values.Replacement || values.Host != "api.example.test" || values.Port != 6443 {
					t.Fatalf("Cilium must bootstrap without a Service VIP: %+v", values)
				}
			}
			if strings.HasPrefix(file.Path, nodebootstrap.BootstrapCredentialDir+"/") {
				count++
				if n.Name != "miller" || file.Mode == nil || *file.Mode != 0600 {
					t.Fatal("bootstrap credentials exposed to wrong node or file mode")
				}
			}
			if strings.Contains(file.Path, "ca.key") {
				t.Fatal("CA signing key embedded in Ignition")
			}
		}
		if chartFound == n.Worker() {
			t.Fatal("Cilium chart must be present only on servers")
		}
		if (n.Name == "miller" && count != 2) || (n.Name != "miller" && count != 0) {
			t.Fatalf("wrong credential files on %s", n.Name)
		}
	}
	options.Credential = nil
	if err := Generate(c, state, options); err == nil {
		t.Fatal("missing bootstrap credential silently accepted")
	}
}

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
		if strings.Contains(conf, "disable-kube-proxy: true\n") == n.Worker() {
			t.Fatal("server-only kube-proxy flag must not be passed to agents")
		}
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
				if !strings.Contains(*unit.Contents, "src=/mnt/resource/cockroach,") || !strings.Contains(*unit.Contents, "ExecStartPre=/opt/infestation/check-cockroach-storage") {
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

func TestAPITailVIPOnlyOnServers(t *testing.T) {
	for _, hostname := range []string{"", "k8s-api.example.ts.net"} {
		t.Run("hostname="+hostname, func(t *testing.T) {
			c := testutil.Config()
			c.APITailnetHostname = hostname
			// A server without local SQL still advertises the API; neither worker does.
			off := false
			c.Nodes[1].CockroachDB = &off
			c.Nodes[1].DatastoreHost = c.Nodes[0].IP
			c.Nodes = append(c.Nodes,
				cluster.Node{Name: "worker-db", IP: "192.168.1.104", Role: "worker"},
				cluster.Node{Name: "worker-only", IP: "192.168.1.105", Role: "worker", CockroachDB: &off})
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
				doc, report, err := config.Parse(raw)
				if err != nil {
					t.Fatal(err, report)
				}
				want := hostname != "" && !n.Worker()
				found := false
				for _, unit := range doc.Systemd.Units {
					if unit.Name != "tailscale-api.service" {
						continue
					}
					found = true
					if unit.Enabled == nil || !*unit.Enabled || unit.Contents == nil ||
						!strings.Contains(*unit.Contents, "--service=svc:k8s-api --tcp=443 tcp://127.0.0.1:6443") ||
						strings.Contains(*unit.Contents, "tailscale down") || strings.Contains(*unit.Contents, "PartOf=") {
						t.Fatalf("incorrect host-level API forwarder on %s", n.Name)
					}
				}
				if found != want {
					t.Fatalf("%s: API forwarder present=%v, want %v", n.Name, found, want)
				}
				for _, file := range doc.Storage.Files {
					if file.Path != "/etc/rancher/k3s/config.yaml" {
						continue
					}
					data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(*file.Contents.Source, "data:;base64,"))
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(data), "k8s-api.example.ts.net") != want {
						t.Fatalf("%s: shared API SAN does not match server opt-in", n.Name)
					}
				}
			}
		})
	}
}
