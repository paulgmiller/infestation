package ignition

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	config "github.com/coreos/ignition/v2/config/v3_3"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/pki"
	"github.com/paulgmiller/infestation/internal/testutil"
	"gopkg.in/yaml.v3"
)

func TestTailnetAuthArgumentIsRenderOnly(t *testing.T) {
	c := testutil.Config()
	c.Tailscale = &cluster.Tailscale{Version: "v1.104.2", Domain: "example.ts.net"}
	noDB := false
	n := cluster.Node{Name: "worker-0", IP: "192.0.2.4", Role: "worker", CockroachDB: &noDB}
	c.Nodes = append(c.Nodes, n)
	state := t.TempDir()
	helper := filepath.Join(state, "helper")
	if err := os.WriteFile(helper, []byte("fake helper"), 0700); err != nil {
		t.Fatal(err)
	}
	key := "tskey-auth-DISPOSABLE-ARGUMENT"
	b := Bootstrap{LocalHelper: helper, TailscaleAuthKey: key}
	doc, err := Render(c, n, state, "test-token", b)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range doc.Storage.Files {
		if f.Contents.Source == nil {
			continue
		}
		s, ok := strings.CutPrefix(*f.Contents.Source, "data:;base64,")
		if !ok {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), key) {
			if f.Path != "/etc/infestation/tailscale-authkey" || f.Mode == nil || *f.Mode != 0600 {
				t.Fatal("key escaped its root-only enrollment file")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("argument was not rendered")
	}
	serialized, err := json.Marshal(b)
	if err != nil || strings.Contains(string(serialized), key) {
		t.Fatal("serialized render-only key", err)
	}
	for _, invalid := range []string{"", "bad-SECRET", "tskey-auth-SECRET\nSECRET"} {
		b.TailscaleAuthKey = invalid
		_, err := Render(c, n, state, "test-token", b)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("missing/invalid key accepted or exposed", err)
		}
	}
	c.Tailscale.AuthKeyFile = "must-not-be-read"
	b.TailscaleAuthKey = key
	if _, err := Render(c, n, state, "test-token", b); err == nil {
		t.Fatal("ambiguous key sources accepted")
	}
}

func TestTailnetProfilesKeepHostsIndependentAndUseNativeBackend(t *testing.T) {
	c := testutil.Config()
	c.Nodes = append(c.Nodes, cluster.Node{Name: "worker-0", IP: "192.0.2.4", Role: "worker"})
	state := t.TempDir()
	keyfile := filepath.Join(state, "authkey")
	if err := os.WriteFile(keyfile, []byte("tskey-auth-DISPOSABLE-TEST"), 0600); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(state, "node-helper")
	if err := os.WriteFile(helper, []byte("test helper"), 0700); err != nil {
		t.Fatal(err)
	}
	c.Tailscale = &cluster.Tailscale{Version: "v1.104.2", Domain: "example.ts.net", AuthKeyFile: keyfile}
	if err := pki.Create(c, state); err != nil {
		t.Fatal(err)
	}
	cred, err := pki.BootstrapCredential(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := Generate(c, state, Bootstrap{LocalHelper: helper, Credential: cred}); err != nil {
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
		files := map[string]string{}
		for _, f := range doc.Storage.Files {
			if strings.Contains(f.Path, "cilium") {
				t.Fatal("legacy Cilium chart installed")
			}
			if strings.Contains(f.Path, "authkey") && (f.Mode == nil || *f.Mode != 0600) {
				t.Fatal("auth key mode")
			}
			if text, ok := strings.CutPrefix(*f.Contents.Source, "data:;base64,"); ok {
				data, _ := base64.StdEncoding.DecodeString(text)
				files[f.Path] = string(data)
			}
		}
		var conf map[string]any
		if err := yaml.Unmarshal([]byte(files["/etc/rancher/k3s/config.yaml"]), &conf); err != nil {
			t.Fatal(err)
		}
		if conf["disable-kube-proxy"] == true || conf["vpn-auth-file"] != "/etc/rancher/k3s/vpn-auth" {
			t.Fatal("kube-proxy/native VPN", conf)
		}
		if !n.Worker() && conf["flannel-backend"] != "tailscale" {
			t.Fatal(conf)
		}
		if n.Worker() && conf["server"] != "https://kube-0.example.ts.net:6443" {
			t.Fatal("worker underlay join", conf)
		}
		if !strings.Contains(files["/opt/infestation/infestation-node"], "test helper") {
			t.Fatal("missing host helper")
		}
		hostUnit, dbUnit, k3sUnit := "", "", ""
		for _, u := range doc.Systemd.Units {
			if u.Name == "infestation-tailnet.service" {
				hostUnit = *u.Contents
			}
			if u.Name == "cockroachdb.service" {
				dbUnit = *u.Contents
			}
			if u.Name == "k3s.service" || u.Name == "k3s-agent.service" {
				for _, drop := range u.Dropins {
					k3sUnit += *drop.Contents
				}
			}
		}
		if hostUnit == "" || strings.Contains(hostUnit, "PartOf=") || strings.Contains(hostUnit, "After=k3s") {
			t.Fatal("host depends on cluster", hostUnit)
		}
		if !strings.Contains(k3sUnit, "Requires=infestation-tailnet.service") || !strings.Contains(k3sUnit, "ExecStartPre=") {
			t.Fatal(k3sUnit)
		}
		if !strings.Contains(dbUnit, "--network host --dns=100.100.100.100") {
			t.Fatal("host container bypasses tailnet DNS", dbUnit)
		}
		if !strings.Contains(dbUnit, "Requires=infestation-tailnet.service") || !strings.Contains(dbUnit, "--advertise-addr="+n.Name+".example.ts.net:26257") {
			t.Fatal("underlay DB dependency", dbUnit)
		}
		linked := false
		for _, l := range doc.Storage.Links {
			if l.Path == "/etc/systemd/system/multi-user.target.wants/tailscaled.service" {
				linked = true
			}
		}
		if !linked {
			t.Fatal("tailscaled not enabled")
		}
	}
}
