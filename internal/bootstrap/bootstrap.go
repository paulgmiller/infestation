// Package bootstrap completes database and Kubernetes setup after VM provisioning.
package bootstrap

import (
	"context"
	_ "embed"
	"encoding/csv"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
	"infestation/internal/cluster"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
)

type Outputs struct {
	PublicIPs   map[string]string `json:"publicIPs"`
	APIEndpoint string            `json:"apiEndpoint"`
}
type Marker struct {
	ClusterID               string `json:"clusterID,omitempty"`
	InitializationAttempted bool   `json:"initializationAttempted,omitempty"`
	KubernetesReady         bool   `json:"kubernetesReady,omitempty"`
}

func (m Marker) CanInitialize(allow bool) bool {
	return allow && m.ClusterID == "" && !m.InitializationAttempted
}
func (m Marker) WithIdentity(id string) (Marker, error) {
	if _, err := uuid.Parse(id); err != nil {
		return m, fmt.Errorf("invalid database identity")
	}
	if m.ClusterID != "" && m.ClusterID != id {
		return m, fmt.Errorf("database identity changed; refusing another cluster")
	}
	m.ClusterID = id
	return m, nil
}
func ParseIdentity(result executil.Result) (string, error) {
	if result.Code != 0 {
		return "", nil
	}
	rows, err := csv.NewReader(strings.NewReader(result.Stdout)).ReadAll()
	if err != nil || len(rows) != 2 || len(rows[1]) != 1 {
		return "", fmt.Errorf("unexpected database identity response")
	}
	if _, err = uuid.Parse(rows[1][0]); err != nil {
		return "", err
	}
	return rows[1][0], nil
}

type Remote interface {
	Run(context.Context, string, string) executil.Result
	Tunnel(context.Context, string, string, int) (int, func(), error)
}
type Options struct {
	StateDir, PrivateKey, Cilium string
	AllowInitialize              bool
	Cluster                      cluster.Config
	Outputs                      Outputs
	Runner                       executil.Runner
	Remote                       Remote
	Log                          io.Writer
	Timeout                      time.Duration
}
type Worker struct{ Options }

func New(o Options) *Worker {
	if o.Cilium == "" {
		o.Cilium = "cilium"
	}
	if o.Runner == nil {
		o.Runner = executil.OSRunner{}
	}
	if o.Remote == nil {
		o.Remote = &SSH{Key: o.PrivateKey, KnownHosts: filepath.Join(o.StateDir, "known_hosts")}
	}
	if o.Log == nil {
		o.Log = os.Stdout
	}
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Minute
	}
	return &Worker{o}
}
func (w *Worker) address(n cluster.Node) string {
	if ip := w.Outputs.PublicIPs[n.Name]; ip != "" {
		return ip
	}
	return n.IP
}
func (w *Worker) run(ctx context.Context, name string, args ...string) executil.Result {
	child, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return w.Runner.Run(child, name, args...)
}
func (w *Worker) wait(ctx context.Context, description string, action func(context.Context) executil.Result) (executil.Result, error) {
	child, cancel := context.WithTimeout(ctx, w.Timeout)
	defer cancel()
	for {
		r := action(child)
		if r.Code == 0 {
			return r, nil
		}
		select {
		case <-child.Done():
			return r, fmt.Errorf("waiting for %s: %w (%s)", description, child.Err(), r.Stderr)
		case <-time.After(3 * time.Second):
		}
	}
}
func (w *Worker) db(ctx context.Context, port int, args ...string) executil.Result {
	name := "infestation-admin-" + uuid.NewString()
	// A killed Docker client can otherwise leave its CLI container running.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		w.Runner.Run(cleanup, "docker", "rm", "--force", name)
	}()
	command := []string{"run", "--rm", "--name", name, "--network", "host", "--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()), "-v", filepath.Join(w.StateDir, "pki/admin") + ":/certs:ro", "--entrypoint=/cockroach/cockroach", w.Cluster.CockroachImage}
	command = append(command, args...)
	command = append(command, "--certs-dir=/certs", fmt.Sprintf("--host=127.0.0.1:%d", port))
	return w.run(ctx, "docker", command...)
}
func (w *Worker) identity(ctx context.Context, port int) executil.Result {
	child, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return w.db(child, port, "sql", "--format=csv", "--execute=SELECT crdb_internal.cluster_id()")
}
func (w *Worker) Database(ctx context.Context) error {
	path := filepath.Join(w.StateDir, "bootstrap.json")
	var marker Marker
	if err := fsutil.ReadJSON(path, &marker); err != nil && !os.IsNotExist(err) {
		return err
	}
	dbs, servers := w.Cluster.Databases(), w.Cluster.Servers()
	if len(dbs) == 0 || len(servers) == 0 {
		return fmt.Errorf("bootstrap requires database nodes and servers")
	}
	for _, n := range dbs {
		if _, err := w.wait(ctx, "database service on "+n.Name, func(ctx context.Context) executil.Result {
			return w.Remote.Run(ctx, w.address(n), "sudo systemctl is-active cockroachdb")
		}); err != nil {
			return err
		}
	}
	gateway := w.address(servers[0])
	id := ""
	for _, n := range dbs {
		port, close, err := w.Remote.Tunnel(ctx, gateway, n.IP, 26257)
		if err != nil {
			return err
		}
		id, err = ParseIdentity(w.identity(ctx, port))
		close()
		if err != nil {
			return err
		}
		if id != "" {
			break
		}
	}
	if id == "" {
		port, close, err := w.Remote.Tunnel(ctx, gateway, dbs[0].IP, 26257)
		if err != nil {
			return err
		}
		defer close()
		if marker.CanInitialize(w.AllowInitialize) {
			marker.InitializationAttempted = true
			if err = fsutil.WriteJSON(path, marker); err != nil {
				return err
			}
			// Exactly one attempt. Probe identity afterward even if the RPC outcome was ambiguous.
			w.db(ctx, port, "init")
		} else if marker.ClusterID == "" {
			return fmt.Errorf("database unavailable: first initialization needs --initialize-new-cluster; an attempted init is never repeated automatically")
		}
		result, err := w.wait(ctx, "existing database identity", func(ctx context.Context) executil.Result { return w.identity(ctx, port) })
		if err != nil {
			return err
		}
		id, err = ParseIdentity(result)
		if err != nil {
			return err
		}
	}
	var err error
	marker, err = marker.WithIdentity(id)
	if err != nil {
		return err
	}
	if err = fsutil.WriteJSON(path, marker); err != nil {
		return err
	}
	port, close, err := w.Remote.Tunnel(ctx, gateway, dbs[0].IP, 26257)
	if err != nil {
		return err
	}
	defer close()
	if _, err = w.wait(ctx, "Kine SQL setup", func(ctx context.Context) executil.Result { return w.db(ctx, port, "sql", "--execute", BootstrapSQL) }); err != nil {
		return err
	}
	if _, err = w.wait(ctx, "healthy database replicas", func(ctx context.Context) executil.Result {
		r := w.db(ctx, port, "node", "status", "--ranges", "--format=csv")
		if r.Code != 0 {
			return r
		}
		if err := Healthy(r.Stdout, len(dbs)); err != nil {
			return executil.Result{Code: 1, Stderr: err.Error()}
		}
		return r
	}); err != nil {
		return err
	}
	fmt.Fprintln(w.Log, "Database identity verified:", id)
	return nil
}
func Healthy(data string, count int) error {
	rows, err := csv.NewReader(strings.NewReader(data)).ReadAll()
	if err != nil || len(rows) < 2 {
		return fmt.Errorf("invalid database status")
	}
	cols := map[string]int{}
	for i, k := range rows[0] {
		cols[k] = i
	}
	for _, k := range []string{"is_live", "ranges_unavailable", "ranges_underreplicated"} {
		if _, ok := cols[k]; !ok {
			return fmt.Errorf("missing database status column %s", k)
		}
	}
	live := 0
	for _, row := range rows[1:] {
		if len(row) != len(rows[0]) {
			return fmt.Errorf("malformed database status row")
		}
		if row[cols["is_live"]] == "true" {
			live++
			if row[cols["ranges_unavailable"]] != "0" || row[cols["ranges_underreplicated"]] != "0" {
				return fmt.Errorf("database replicas not healthy")
			}
		}
	}
	if live < count {
		return fmt.Errorf("not all database nodes are live")
	}
	return nil
}

// BootstrapSQL is idempotent and embedded so binaries work outside the checkout.
//
//go:embed bootstrap.sql
var BootstrapSQL string

type Kubeconfig struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
	Clusters   []struct {
		Name    string         `json:"name" yaml:"name"`
		Cluster map[string]any `json:"cluster" yaml:"cluster"`
	} `json:"clusters" yaml:"clusters"`
	Users []struct {
		Name string         `json:"name" yaml:"name"`
		User map[string]any `json:"user" yaml:"user"`
	} `json:"users" yaml:"users"`
	Contexts       []map[string]any `json:"contexts" yaml:"contexts"`
	CurrentContext string           `json:"current-context" yaml:"current-context"`
}

func PublicKubeconfig(data []byte, endpoint string) (Kubeconfig, error) {
	var c Kubeconfig
	if err := yaml.Unmarshal(data, &c); err != nil {
		return c, err
	}
	u, urlErr := url.Parse(endpoint)
	if len(c.Clusters) != 1 || len(c.Users) != 1 || urlErr != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return c, fmt.Errorf("expected one admin kubeconfig and HTTPS endpoint")
	}
	if _, ok := c.Clusters[0].Cluster["certificate-authority-data"]; !ok {
		return c, fmt.Errorf("missing API CA")
	}
	if _, ok := c.Users[0].User["client-key-data"]; !ok {
		return c, fmt.Errorf("missing admin client key")
	}
	delete(c.Clusters[0].Cluster, "insecure-skip-tls-verify")
	c.Clusters[0].Name = "infestation"
	c.Clusters[0].Cluster["server"] = endpoint
	c.Users[0].Name = "infestation-admin"
	c.Contexts = []map[string]any{{"name": "infestation", "context": map[string]string{"cluster": "infestation", "user": "infestation-admin"}}}
	c.CurrentContext = "infestation"
	return c, nil
}
func (w *Worker) kubectl(ctx context.Context, args ...string) executil.Result {
	return w.run(ctx, "kubectl", append([]string{"--kubeconfig", filepath.Join(w.StateDir, "kubeconfig"), "--request-timeout=15s"}, args...)...)
}
func (w *Worker) Kubernetes(ctx context.Context) error {
	if w.Outputs.APIEndpoint == "" {
		return fmt.Errorf("automatic bootstrap requires a reachable API endpoint")
	}
	node := w.Cluster.Servers()[0]
	result, err := w.wait(ctx, "admin kubeconfig", func(ctx context.Context) executil.Result {
		return w.Remote.Run(ctx, w.address(node), "sudo cat /etc/rancher/k3s/k3s.yaml")
	})
	if err != nil {
		return err
	}
	conf, err := PublicKubeconfig([]byte(result.Stdout), w.Outputs.APIEndpoint)
	if err != nil {
		return err
	}
	if err = fsutil.WriteJSON(filepath.Join(w.StateDir, "kubeconfig"), conf); err != nil {
		return err
	}
	if _, err = w.wait(ctx, "API TLS and readiness", func(ctx context.Context) executil.Result { return w.kubectl(ctx, "get", "--raw=/readyz") }); err != nil {
		return err
	}
	existing := w.kubectl(ctx, "-n", "kube-system", "get", "daemonset", "cilium", "--ignore-not-found", "-o", "name")
	if err = existing.Err("inspect Cilium"); err != nil {
		return err
	}
	if strings.TrimSpace(existing.Stdout) == "" {
		args := []string{"--kubeconfig", filepath.Join(w.StateDir, "kubeconfig"), "install", "--version", "1.20.4"}
		replicas := "operator.replicas=2"
		if w.Cluster.SingleNodeLab {
			replicas = "operator.replicas=1"
		}
		for _, setting := range []string{replicas, "kubeProxyReplacement=false", "ipam.operator.clusterPoolIPv4PodCIDRList=10.42.0.0/16", "cni.binPath=/opt/cni/bin", "cni.confPath=/etc/cni/net.d", "k8sServiceHost=10.43.0.1", "k8sServicePort=443"} {
			args = append(args, "--set", setting)
		}
		if err = w.run(ctx, w.Cilium, args...).Err("install Cilium"); err != nil {
			return err
		}
	}
	r := w.Runner.Run(ctx, w.Cilium, "--kubeconfig", filepath.Join(w.StateDir, "kubeconfig"), "status", "--wait", "--wait-duration=10m")
	fmt.Fprint(w.Log, r.Stdout)
	if err = r.Err("Cilium readiness"); err != nil {
		return err
	}
	for _, n := range w.Cluster.Nodes {
		if err = w.kubectl(ctx, "wait", "--for=condition=Ready", "node/"+n.Name, "--timeout=90s").Err("node readiness"); err != nil {
			return err
		}
	}
	replacement := w.kubectl(ctx, "-n", "kube-system", "get", "configmap", "cilium-config", "-o", "jsonpath={.data.kube-proxy-replacement}")
	if replacement.Code != 0 || replacement.Stdout != "false" {
		return fmt.Errorf("unexpected kube-proxy replacement configuration")
	}
	var marker Marker
	if err = fsutil.ReadJSON(filepath.Join(w.StateDir, "bootstrap.json"), &marker); err != nil {
		return err
	}
	marker.KubernetesReady = true
	if err = fsutil.WriteJSON(filepath.Join(w.StateDir, "bootstrap.json"), marker); err != nil {
		return err
	}
	fmt.Fprintln(w.Log, "Bootstrap complete. Kubeconfig:", filepath.Join(w.StateDir, "kubeconfig"))
	return nil
}
func (w *Worker) Complete(ctx context.Context) error {
	if err := w.Cluster.Validate(); err != nil {
		return err
	}
	if err := w.Database(ctx); err != nil {
		return err
	}
	return w.Kubernetes(ctx)
}

// LoadOutputs parses Pulumi's non-secret stack outputs without logging them.
func LoadOutputs(path string) (Outputs, error) {
	var o Outputs
	err := fsutil.ReadJSON(path, &o)
	return o, err
}
