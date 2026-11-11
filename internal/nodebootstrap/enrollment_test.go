package nodebootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infestation/internal/cluster"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
	"infestation/internal/pki"
	"infestation/internal/testutil"
)

type statusRunner struct{ status enrollmentStatus }

func (r statusRunner) Run(_ context.Context, name string, args ...string) executil.Result {
	if name != "tailscale" || strings.Join(args, " ") != "status --json" {
		return executil.Result{Code: 1}
	}
	raw, _ := json.Marshal(r.status)
	return executil.Result{Stdout: string(raw)}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func enrollmentFixture(t *testing.T) (JoinBundle, Config) {
	t.Helper()
	c := testutil.Config()
	c.Nodes = c.Nodes[:1]
	c.Nodes[0].Name = "miller"
	c.DiscoverPeers = true
	c.Tailscale = &cluster.Tailscale{Version: "v1.104.2", Domain: "example.ts.net", SSH: true}
	c.APITailnetHostname = "k8s-api.example.ts.net"
	state := t.TempDir()
	if err := pki.Create(c, state); err != nil {
		t.Fatal(err)
	}
	shared, err := pki.PoolCertificate(state, c.Tailscale.Domain)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range shared {
		if err := os.WriteFile(filepath.Join(state, "pki", "miller", name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := NewJoinBundle(c, c.Nodes[0], state, "disposable-cluster-token-12345678")
	if err != nil {
		t.Fatal(err)
	}
	var b JoinBundle
	if err := json.Unmarshal(raw, &b); err != nil {
		t.Fatal(err)
	}
	return b, Config{Cluster: c, Node: "miller"}
}
func joinFixture(t *testing.T, b JoinBundle) (Config, string, statusRunner) {
	t.Helper()
	c := b.Cluster
	c.Nodes = []cluster.Node{{Name: "locke", IP: "192.0.2.1"}}
	root := t.TempDir()
	if err := fsutil.WriteFile(filepath.Join(root, "/etc/rancher/k3s/config.yaml"), []byte("token: \"\"\nnode-name: locke\n"), 0600); err != nil {
		t.Fatal(err)
	}
	status := enrollmentStatus{BackendState: "Running", Self: tailPeer{ID: "self", DNSName: "locke.example.ts.net.", Tags: []string{"tag:roach"}, TailscaleIPs: []string{"100.64.0.20"}}, Peer: map[string]tailPeer{"peer": {ID: "peer", DNSName: "miller.example.ts.net.", Tags: []string{"tag:roach"}, TailscaleIPs: []string{"100.64.0.10"}, Online: true}}}
	return Config{Cluster: c, Node: "locke"}, root, statusRunner{status: status}
}
func handlerClient(h http.Handler) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "100.64.0.10:7444" {
			return nil, fmt.Errorf("unexpected enrollment target")
		}
		r.RemoteAddr = "100.64.0.20:12345"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Result(), nil
	})}
}

func TestIndependentJoinAndOfflineRestartPinExistingIdentity(t *testing.T) {
	b, seed := enrollmentFixture(t)
	c, root, runner := joinFixture(t, b)
	client := handlerClient(membershipHandler(seed, b, t.TempDir()))
	if err := joinMembership(context.Background(), c, root, runner, client); err != nil {
		t.Fatal(err)
	}
	for _, name := range joinFiles {
		raw, err := os.ReadFile(filepath.Join(root, "/etc/cockroach/certs", name))
		if err != nil || string(raw) != string(b.Files[name]) {
			t.Fatal("join material was not installed", name, err)
		}
	}
	for _, name := range []string{"ca.key", "client.root.key"} {
		if _, err := os.Stat(filepath.Join(root, "/etc/cockroach/certs", name)); !os.IsNotExist(err) {
			t.Fatal("signing/admin key reached joining VM")
		}
	}
	for _, path := range []string{JoinBundlePath, "/etc/cockroach/certs/client.kine.key"} {
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("private material permissions", path, err)
		}
	}
	for _, path := range []string{"/etc/cockroach", "/etc/cockroach/certs"} {
		info, err := os.Stat(filepath.Join(root, path))
		if err != nil || info.Mode().Perm() != 0755 {
			t.Fatal("database UID cannot traverse certificate directory", path, err)
		}
	}
	caInfo, _ := os.Stat(filepath.Join(root, "/etc/cockroach/certs/ca.crt"))
	if caInfo.Mode().Perm() != 0644 {
		t.Fatal("database container must read public CA")
	}
	cfg, err := Load(filepath.Join(root, "/etc/infestation/bootstrap.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node != "locke" || cfg.Cluster.Nodes[0].Init {
		t.Fatal("joiner acquired init authority")
	}
	// Restart without any currently reachable bootstrap peers: use pinned bundle.
	runner.status.Peer = nil
	if err := joinMembership(context.Background(), cfg, root, runner, client); err != nil {
		t.Fatal("offline restart discarded pinned material", err)
	}
	// An online peer with another CA must not replace that pin.
	other, _ := enrollmentFixture(t)
	runner.status.Peer = map[string]tailPeer{"peer": {ID: "peer", DNSName: "miller.example.ts.net.", Tags: []string{"tag:roach"}, TailscaleIPs: []string{"100.64.0.10"}, Online: true}}
	before, _ := os.ReadFile(filepath.Join(root, JoinBundlePath))
	if err := joinMembership(context.Background(), cfg, root, runner, handlerClient(membershipHandler(seed, other, t.TempDir()))); err == nil {
		t.Fatal("cluster identity changed")
	}
	after, _ := os.ReadFile(filepath.Join(root, JoinBundlePath))
	if string(before) != string(after) {
		t.Fatal("conflicting identity overwrote pin")
	}
}

func TestJoinWithoutPeerNeverCreatesFoundingCredentials(t *testing.T) {
	b, _ := enrollmentFixture(t)
	c, root, runner := joinFixture(t, b)
	runner.status.Peer = nil
	if err := joinMembership(context.Background(), c, root, runner, enrollmentClient()); err == nil {
		t.Fatal("joined without founding material")
	}
	if _, err := os.Stat(filepath.Join(root, JoinBundlePath)); !os.IsNotExist(err) {
		t.Fatal("created new cluster material on joiner")
	}
}

func TestEnrollmentRejectsRootAndUnknownKeys(t *testing.T) {
	b, _ := enrollmentFixture(t)
	b.Files["ca.key"] = []byte("forbidden-signing-key")
	if b.Validate("example.ts.net") == nil {
		t.Fatal("CA key accepted in enrollment bundle")
	}
	delete(b.Files, "ca.key")
	b.Files["client.root.key"] = []byte("forbidden-admin-key")
	if b.Validate("example.ts.net") == nil {
		t.Fatal("root key accepted in enrollment bundle")
	}
	delete(b.Files, "client.root.key")
	if b.Validate("other.ts.net") == nil {
		t.Fatal("another tailnet accepted")
	}
}

func TestMembershipEndpointIsTailnetOnlyAndDoesNotExposeRootCredential(t *testing.T) {
	b, c := enrollmentFixture(t)
	handler := membershipHandler(c, b, t.TempDir())
	for _, source := range []string{"127.0.0.1:1234", "192.168.1.2:1234", "203.0.113.1:1234"} {
		req := httptest.NewRequest("GET", "http://example/v1/join", nil)
		req.RemoteAddr = source
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != 403 {
			t.Fatal("non-tailnet request accepted", source)
		}
	}
	req := httptest.NewRequest("GET", "http://example/v1/join", nil)
	req.RemoteAddr = "100.64.0.20:1234"
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("enrollment response is unavailable or cacheable")
	}
	var response JoinBundle
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil || response.Validate("example.ts.net") != nil {
		t.Fatal("invalid served bundle", err)
	}
	for name := range response.Files {
		if strings.Contains(name, "root") || name == "ca.key" {
			t.Fatal("administrative signing material served")
		}
	}
}

func TestDiscoveryUsesTaggedTailnetDNSAndStableIDs(t *testing.T) {
	b, _ := enrollmentFixture(t)
	_, _, runner := joinFixture(t, b)
	runner.status.Peer["untagged"] = tailPeer{ID: "other", DNSName: "stranger.example.ts.net.", Online: true, TailscaleIPs: []string{"100.64.0.30"}}
	runner.status.Peer["foreign"] = tailPeer{ID: "foreign", DNSName: "foreign.other.ts.net.", Online: true, Tags: []string{"tag:roach"}, TailscaleIPs: []string{"100.64.0.31"}}
	_, found, err := peers(context.Background(), runner, "example.ts.net")
	if err != nil || len(found) != 1 || found[0].ID != "peer" {
		t.Fatal("incorrect discovery scope", found, err)
	}
}
