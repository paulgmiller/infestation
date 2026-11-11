package nodebootstrap

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"infestation/internal/cluster"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
)

const MembershipPort = "7444"
const JoinBundlePath = "/var/lib/infestation/join.json"
const maxEnrollmentBytes = 128 << 10

// JoinBundle carries shared leaf identities, never a CA signing key or SQL root
// client key. Tailnet membership is explicitly the administrator trust boundary.
type JoinBundle struct {
	Version     int               `json:"version"`
	Cluster     cluster.Config    `json:"cluster"`
	Initializer cluster.Node      `json:"initializer"`
	Token       string            `json:"token"`
	Files       map[string][]byte `json:"files"`
}

var joinFiles = []string{"ca.crt", "node.crt", "node.key", "client.kine.crt", "client.kine.key"}

func NewJoinBundle(c cluster.Config, n cluster.Node, state, token string) ([]byte, error) {
	b := JoinBundle{Version: 1, Cluster: c, Initializer: n, Token: token, Files: map[string][]byte{}}
	b.Cluster.Nodes = nil
	b.Cluster.Tailscale = &cluster.Tailscale{Version: c.Tailscale.Version, Domain: c.Tailscale.Domain, SSH: true}
	for _, name := range joinFiles {
		data, err := os.ReadFile(filepath.Join(state, "pki", n.Name, name))
		if err != nil {
			return nil, err
		}
		b.Files[name] = data
	}
	if err := b.Validate(c.Tailscale.Domain); err != nil {
		return nil, err
	}
	return json.Marshal(b)
}

func (b JoinBundle) Identity() string {
	id := sha256.Sum256(b.Files["ca.crt"])
	return hex.EncodeToString(id[:])
}

func (b JoinBundle) Validate(domain string) error {
	if b.Version != 1 || b.Cluster.Tailscale == nil || b.Cluster.Tailscale.Domain != domain || !b.Cluster.DiscoverPeers || len(b.Cluster.Nodes) != 0 || len(b.Files) != len(joinFiles) {
		return fmt.Errorf("invalid enrollment bundle or tailnet mismatch")
	}
	if !b.Initializer.Init || b.Initializer.Worker() || !b.Initializer.Database() {
		return fmt.Errorf("invalid bootstrap authority")
	}
	c := b.Cluster
	c.Nodes = []cluster.Node{b.Initializer}
	if err := c.Validate(); err != nil {
		return err
	}
	if len(b.Token) < 16 || strings.ContainsAny(b.Token, "\r\n") {
		return fmt.Errorf("invalid cluster token")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(b.Files["ca.crt"]) {
		return fmt.Errorf("invalid enrollment CA")
	}
	for _, name := range joinFiles {
		if len(b.Files[name]) == 0 {
			return fmt.Errorf("missing enrollment credential")
		}
	}
	for _, identity := range []struct{ file, cn string }{{"node", "node"}, {"client.kine", "kine"}} {
		pair, err := tls.X509KeyPair(b.Files[identity.file+".crt"], b.Files[identity.file+".key"])
		if err != nil {
			return fmt.Errorf("invalid enrollment key pair")
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil || leaf.IsCA || leaf.Subject.CommonName != identity.cn {
			return fmt.Errorf("invalid enrollment leaf identity")
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
			return fmt.Errorf("enrollment client certificate verification failed")
		}
		if identity.cn == "node" {
			for _, host := range []string{"independent-node." + domain, "localhost", "127.0.0.1"} {
				if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: host, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
					return fmt.Errorf("enrollment node certificate does not cover independent tailnet names")
				}
			}
		}
	}
	return nil
}

type tailPeer struct {
	ID, DNSName        string
	TailscaleIPs, Tags []string
	Online             bool
}
type enrollmentStatus struct {
	BackendState string
	Self         tailPeer
	Peer         map[string]tailPeer
}
type member struct {
	Identity string       `json:"identity"`
	Node     cluster.Node `json:"node"`
}

func peers(ctx context.Context, runner executil.Runner, domain string) (enrollmentStatus, []tailPeer, error) {
	child, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result := runner.Run(child, "tailscale", "status", "--json")
	var s enrollmentStatus
	if result.Code != 0 || json.Unmarshal([]byte(result.Stdout), &s) != nil || s.BackendState != "Running" {
		return s, nil, fmt.Errorf("tailnet peer discovery is unavailable")
	}
	var out []tailPeer
	for _, p := range s.Peer {
		name := strings.TrimSuffix(p.DNSName, ".")
		if !p.Online || p.ID == "" || !strings.HasSuffix(name, "."+domain) || strings.Count(name, ".") != strings.Count(domain, ".")+1 || !hasTag(p.Tags, "tag:roach") || peerIP(p) == "" {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return s, out, nil
}
func hasTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
func peerIP(p tailPeer) string {
	for _, v := range p.TailscaleIPs {
		ip, err := netip.ParseAddr(v)
		if err == nil && netip.MustParsePrefix("100.64.0.0/10").Contains(ip) {
			return ip.String()
		}
	}
	return ""
}

// Never use HTTP_PROXY for enrollment traffic and never follow redirects off a
// discovered tailnet address. WireGuard supplies transport authentication/privacy.
func enrollmentClient() *http.Client {
	return &http.Client{Timeout: 4 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
func fetch(ctx context.Context, client *http.Client, p tailPeer, path string, dst any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+net.JoinHostPort(peerIP(p), MembershipPort)+path, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("peer enrollment endpoint unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("peer enrollment endpoint is not ready")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxEnrollmentBytes+1))
	if err != nil || len(raw) > maxEnrollmentBytes {
		return fmt.Errorf("invalid enrollment response size")
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("invalid enrollment response")
	}
	return nil
}
func readBundle(root, domain string) (JoinBundle, error) {
	var b JoinBundle
	err := fsutil.ReadJSON(filepath.Join(root, JoinBundlePath), &b)
	if err != nil {
		return b, err
	}
	return b, b.Validate(domain)
}

// JoinMembership runs after enrollment and before the database/K3s. Existing
// pinned material is reused; unreachable peers never authorize a new CA or DB.
func JoinMembership(ctx context.Context, c Config, root string, runner executil.Runner) error {
	return joinMembership(ctx, c, root, runner, enrollmentClient())
}
func joinMembership(ctx context.Context, c Config, root string, runner executil.Runner, client *http.Client) error {
	domain := c.Cluster.Tailscale.Domain
	status, candidates, err := peers(ctx, runner, domain)
	if err != nil {
		return err
	}
	own := peerIP(status.Self)
	if own == "" || strings.TrimSuffix(status.Self.DNSName, ".") != c.Node+"."+domain || !hasTag(status.Self.Tags, "tag:roach") {
		return fmt.Errorf("unexpected tailnet enrollment identity or missing tag:roach")
	}
	bundle, err := readBundle(root, domain)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	pinned := err == nil
	for _, p := range candidates {
		var other JoinBundle
		if fetch(ctx, client, p, "/v1/join", &other) != nil {
			continue
		}
		if err := other.Validate(domain); err != nil {
			return err
		}
		if (pinned || bundle.Version != 0) && bundle.Identity() != other.Identity() {
			return fmt.Errorf("multiple cluster identities in this tailnet; refusing enrollment")
		}
		if !pinned {
			bundle = other
		}
	}
	if bundle.Version == 0 {
		return fmt.Errorf("waiting for a tailnet bootstrap host; only --init may create founding credentials")
	}
	if c.Cluster.CockroachImage != bundle.Cluster.CockroachImage || c.Cluster.K3sExtensionVersion != bundle.Cluster.K3sExtensionVersion || c.Cluster.Tailscale.Version != bundle.Cluster.Tailscale.Version {
		return fmt.Errorf("node and cluster software pins differ")
	}
	if !pinned {
		if err := fsutil.PrivateDir(filepath.Join(root, "/var/lib/infestation")); err != nil {
			return err
		}
		raw, _ := json.Marshal(bundle)
		if err := fsutil.CreatePrivateFile(filepath.Join(root, JoinBundlePath), raw); err != nil {
			return err
		}
	}
	// The database runs as UID 1000. Public directory traversal is required;
	// private leaf keys retain their separate root/UID-1000 permissions.
	certDir := filepath.Join(root, "/etc/cockroach/certs")
	if err := os.MkdirAll(certDir, 0755); err != nil {
		return err
	}
	for _, dir := range []string{filepath.Dir(certDir), certDir} {
		if err := os.Chmod(dir, 0755); err != nil {
			return err
		}
	}
	for _, name := range joinFiles {
		path := filepath.Join(root, "/etc/cockroach/certs", name)
		if old, err := os.ReadFile(path); err == nil && string(old) != string(bundle.Files[name]) {
			return fmt.Errorf("existing database credentials differ; refusing replacement")
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		mode := os.FileMode(0600)
		if name == "ca.crt" || name == "node.crt" {
			mode = 0644
		}
		if name == "node.key" {
			mode = 0400
		}
		if err := fsutil.WriteFile(path, bundle.Files[name], mode); err != nil {
			return err
		}
		if root == "/" && name == "node.key" {
			if err := os.Chown(path, 1000, 1000); err != nil {
				return err
			}
		}
	}
	// Keep local identity/role, adopt cluster settings without acquiring signing keys.
	var local cluster.Node
	for _, n := range c.Cluster.Nodes {
		if n.Name == c.Node {
			local = n
		}
	}
	if !local.Init && local.Name == bundle.Initializer.Name {
		return fmt.Errorf("joining node name collides with bootstrap authority")
	}
	if local.Name == "" {
		return fmt.Errorf("local node identity missing")
	}
	local.IP = own
	if local.Init && local.Name != bundle.Initializer.Name {
		return fmt.Errorf("--init conflicts with the pinned bootstrap authority")
	}
	arch := c.Cluster.Architecture
	c.Cluster = bundle.Cluster
	c.Cluster.Architecture = arch
	c.Cluster.Nodes = []cluster.Node{local}
	if local.Name != bundle.Initializer.Name {
		seed := bundle.Initializer
		seed.IP = "192.0.2.254"
		c.Cluster.Nodes = append(c.Cluster.Nodes, seed)
	}
	c.APIEndpoint = "https://" + c.Cluster.APITailnetHostname + ":443"
	// Use all responsive members as join seeds so later joins survive initializer loss.
	c, err = discoverMembers(ctx, c, bundle, status, candidates, client)
	if err != nil {
		return err
	}
	if err := c.Cluster.Validate(); err != nil {
		return err
	}
	var seeds []string
	for _, n := range c.Cluster.Databases() {
		seeds = append(seeds, net.JoinHostPort(c.Cluster.NodeAddress(n), "26257"))
	}
	if err := fsutil.WriteFile(filepath.Join(root, "/run/infestation/cluster.env"), []byte("INFESTATION_SEEDS="+strings.Join(seeds, ",")+"\n"), 0600); err != nil {
		return err
	}
	configPath := filepath.Join(root, "/etc/rancher/k3s/config.yaml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	var k3s map[string]any
	if err := yaml.Unmarshal(raw, &k3s); err != nil {
		return err
	}
	k3s["token"] = bundle.Token
	raw, err = yaml.Marshal(k3s)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFile(configPath, raw, 0600); err != nil {
		return err
	}
	return fsutil.WriteJSON(filepath.Join(root, "/etc/infestation/bootstrap.json"), c)
}

func discoverMembers(ctx context.Context, c Config, b JoinBundle, status enrollmentStatus, candidates []tailPeer, client *http.Client) (Config, error) {
	nodes := map[string]cluster.Node{}
	for _, n := range c.Cluster.Nodes {
		nodes[n.Name] = n
	}
	for _, p := range candidates {
		var m member
		if fetch(ctx, client, p, "/v1/node", &m) != nil {
			continue
		}
		if m.Identity != b.Identity() {
			return c, fmt.Errorf("discovered a different cluster identity")
		}
		if m.Node.Name+"."+c.Cluster.Tailscale.Domain != strings.TrimSuffix(p.DNSName, ".") {
			return c, fmt.Errorf("peer bootstrap identity disagrees with Tailscale")
		}
		m.Node.IP = peerIP(p)
		m.Node.Init = m.Node.Name == b.Initializer.Name
		nodes[m.Node.Name] = m.Node
	}
	c.Cluster.Nodes = nil
	for _, n := range nodes {
		c.Cluster.Nodes = append(c.Cluster.Nodes, n)
	}
	sort.Slice(c.Cluster.Nodes, func(i, j int) bool { return c.Cluster.Nodes[i].Name < c.Cluster.Nodes[j].Name })
	return c, nil
}

func (w *Worker) discoverTailnet(ctx context.Context) error {
	b, err := readBundle("/", w.Config.Cluster.Tailscale.Domain)
	if err != nil {
		return err
	}
	status, candidates, err := peers(ctx, w.Runner, w.Config.Cluster.Tailscale.Domain)
	if err != nil {
		return err
	}
	w.Config, err = discoverMembers(ctx, w.Config, b, status, candidates, enrollmentClient())
	return err
}

func membershipHandler(c Config, b JoinBundle, root string) http.Handler {
	mux := http.NewServeMux()
	write := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("/v1/join", func(w http.ResponseWriter, r *http.Request) { write(w, b) })
	mux.HandleFunc("/v1/node", func(w http.ResponseWriter, r *http.Request) {
		for _, n := range c.Cluster.Nodes {
			if n.Name == c.Node {
				write(w, member{Identity: b.Identity(), Node: n})
				return
			}
		}
		http.Error(w, "not ready", 503)
	})
	mux.HandleFunc("/v1/kubeconfig", func(w http.ResponseWriter, r *http.Request) {
		raw, err := os.ReadFile(filepath.Join(root, "/etc/infestation/kubeconfig"))
		if err != nil {
			http.Error(w, "not ready", 503)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Write(raw)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		ip, parseErr := netip.ParseAddr(host)
		if err != nil || parseErr != nil || !netip.MustParsePrefix("100.64.0.0/10").Contains(ip) {
			http.Error(w, "tailnet only", 403)
			return
		}
		if r.Method != "GET" {
			http.Error(w, "GET required", 405)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func ServeMembership(ctx context.Context, c Config, runner executil.Runner) error {
	b, err := readBundle("/", c.Cluster.Tailscale.Domain)
	if err != nil {
		return err
	}
	status, _, err := peers(ctx, runner, c.Cluster.Tailscale.Domain)
	if err != nil {
		return err
	}
	ip := peerIP(status.Self)
	if ip == "" {
		return fmt.Errorf("no tailnet IPv4 for bootstrap listener")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(ip, MembershipPort))
	if err != nil {
		return err
	}
	server := &http.Server{Handler: membershipHandler(c, b, "/"), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		server.Shutdown(shutdown)
	}()
	err = server.Serve(listener)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// FetchKubeconfig retrieves the explicitly tailnet-public admin configuration.
// The returned Kubernetes endpoint retains its own CA/client TLS authentication.
func FetchKubeconfig(ctx context.Context, hostname string) ([]byte, error) {
	if !cluster.ValidSAN(hostname) || !strings.HasSuffix(hostname, ".ts.net") {
		return nil, fmt.Errorf("invalid tailnet hostname")
	}
	req, err := http.NewRequestWithContext(ctx, "GET", "http://"+net.JoinHostPort(hostname, MembershipPort)+"/v1/kubeconfig", nil)
	if err != nil {
		return nil, err
	}
	resp, err := enrollmentClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("tailnet kubeconfig endpoint unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("tailnet kubeconfig is not ready")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxEnrollmentBytes+1))
	if err != nil || len(raw) > maxEnrollmentBytes {
		return nil, fmt.Errorf("invalid kubeconfig response size")
	}
	return raw, nil
}
