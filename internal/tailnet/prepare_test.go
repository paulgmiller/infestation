package tailnet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
)

type fakeRunner struct {
	calls             [][]string
	status            Status
	needLogin, failed bool
}

func (r *fakeRunner) Run(_ context.Context, name string, args ...string) executil.Result {
	r.calls = append(r.calls, append([]string{name}, args...))
	if r.failed {
		return executil.Result{Code: 1, Stderr: "SECRET should never appear"}
	}
	if args[0] == "up" {
		r.needLogin = false
	}
	s := r.status
	if r.needLogin {
		s.BackendState = "NeedsLogin"
	}
	data, _ := json.Marshal(s)
	return executil.Result{Stdout: string(data)}
}
func testRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for path, data := range map[string][]byte{
		ConfigPath: []byte(`{"name":"kube-0","dns_name":"kube-0.example.ts.net","server":true}`),
		AuthPath:   []byte("tskey-auth-DISPOSABLE-TEST\n"),
		K3sPath:    []byte("node-ip: 192.0.2.1\nadvertise-address: 192.0.2.1\ntoken: test-token\ntls-san: [kube-0.example.ts.net]\n"),
	} {
		if err := fsutil.WriteFile(filepath.Join(root, path), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
func testStatus() Status {
	s := Status{BackendState: "Running", TailscaleIPs: []string{"100.100.1.2"}}
	s.Self.DNSName = "kube-0.example.ts.net."
	s.Self.Tags = []string{"tag:infestation"}
	return s
}
func TestPrepareEnrolsOnceAndPreservesIdentity(t *testing.T) {
	root := testRoot(t)
	r := &fakeRunner{status: testStatus(), needLogin: true}
	if err := Prepare(context.Background(), root, r); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, AuthPath)); !os.IsNotExist(err) {
		t.Fatal("enrollment key retained", err)
	}
	if err := Prepare(context.Background(), root, r); err != nil {
		t.Fatal("reboot should use durable identity", err)
	}
	ups := 0
	for _, args := range r.calls {
		text := strings.Join(args, " ")
		if strings.Contains(text, "--reset") || strings.Contains(text, "DISPOSABLE-TEST") {
			t.Fatal("reset or raw credential in argv", text)
		}
		if args[1] == "up" {
			ups++
			if !strings.Contains(text, "--auth-key=file:") {
				t.Fatal(text)
			}
		}
	}
	if ups != 1 {
		t.Fatal("reenrolled durable node", ups)
	}
	data, _ := os.ReadFile(filepath.Join(root, K3sPath))
	var conf map[string]any
	if err := yaml.Unmarshal(data, &conf); err != nil {
		t.Fatal(err)
	}
	if conf["node-ip"] != "100.100.1.2" || conf["advertise-address"] != "100.100.1.2" || conf["token"] != "test-token" {
		t.Fatal(conf)
	}
}
func TestPrepareRefusesWrongUntaggedOrExpiredIdentity(t *testing.T) {
	for _, change := range []func(*fakeRunner){
		func(r *fakeRunner) { r.status.Self.DNSName = "other.example.ts.net." },
		func(r *fakeRunner) { r.status.Self.Tags = nil },
		func(r *fakeRunner) { r.status.TailscaleIPs = []string{"192.0.2.1"} },
		func(r *fakeRunner) { r.failed = true },
	} {
		root := testRoot(t)
		r := &fakeRunner{status: testStatus()}
		change(r)
		before, _ := os.ReadFile(filepath.Join(root, K3sPath))
		err := Prepare(context.Background(), root, r)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatal("identity/error handling", err)
		}
		after, _ := os.ReadFile(filepath.Join(root, K3sPath))
		if string(before) != string(after) {
			t.Fatal("changed config for invalid identity")
		}
	}
	root := testRoot(t)
	if err := os.Remove(filepath.Join(root, AuthPath)); err != nil {
		t.Fatal(err)
	}
	if err := Prepare(context.Background(), root, &fakeRunner{status: testStatus(), needLogin: true}); err == nil {
		t.Fatal("expired identity without enrollment key accepted")
	}
}
