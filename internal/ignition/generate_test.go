package ignition

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	config "github.com/coreos/ignition/v2/config/v3_3"
	"gopkg.in/yaml.v3"
	"infestation/internal/cluster"
	"infestation/internal/nodebootstrap"
	"infestation/internal/pki"
	"infestation/internal/testutil"
)

func TestTemporarySQLCredentialOnlyOnOrdinalZero(t *testing.T) {
	c := testutil.Config()
	c.Nodes = append(c.Nodes, cluster.Node{Name: "worker-0", IP: "192.168.1.104", Role: "worker"})
	state := t.TempDir()
	if err := pki.Create(c, state); err != nil {
		t.Fatal(err)
	}
	credential, err := pki.BootstrapCredential(state)
	if err != nil {
		t.Fatal(err)
	}
	options := Bootstrap{Account: "testaccount", HelperSHA256: strings.Repeat("a", 64), DeploymentID: "test", APIEndpoint: "https://api.example.test:6443", Credential: credential}
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
		count := 0
		chartFound := false
		for _, file := range doc.Storage.Files {
			data, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(*file.Contents.Source, "data:;base64,"))
			if file.Path == "/etc/rancher/k3s/config.yaml" && !strings.Contains(string(data), "disable-kube-proxy: true\n") {
				t.Fatal("kube-proxy must be disabled on every role")
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
				if n.Name != "kube-0" || file.Mode == nil || *file.Mode != 0600 {
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
		if (n.Name == "kube-0" && count != 2) || (n.Name != "kube-0" && count != 0) {
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
		if !strings.Contains(conf, "disable-kube-proxy: true\n") {
			t.Fatal("kube-proxy must be disabled even without automatic bootstrap")
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
