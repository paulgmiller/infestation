// Package nodebootstrap brings up a node locally without a management connection.
package nodebootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/paulgmiller/infestation/internal/bootstrap"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/fsutil"
)

type Config struct {
	ScaleSet     *ScaleSet      `json:"scaleSet,omitempty"`
	Cluster      cluster.Config `json:"cluster"`
	Node         string         `json:"node"`
	APIEndpoint  string         `json:"apiEndpoint"`
	DeploymentID string         `json:"deploymentID"`
}

type Store interface {
	Get(context.Context, string) ([]byte, error)
	Put(context.Context, string, []byte) error
	Create(context.Context, string, []byte) error
}

type Worker struct {
	Init                 bool // explicit --init authority; never inferred from a node name
	Config               Config
	Runner               executil.Runner
	Store                Store
	Reachable            func(context.Context, string) bool
	KubeconfigPath       string
	CredentialDir        string
	RuntimeCredentialDir string
	CAPath               string
	bootstrapSQL         bool
}

func (w Worker) command(ctx context.Context, name string, args ...string) executil.Result {
	child, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return w.Runner.Run(child, name, args...)
}

func (w Worker) sql(ctx context.Context, host, query string) executil.Result {
	user, certificate, key := "kine", "/cockroach/certs/client.kine.crt", "/cockroach/certs/client.kine.key"
	if w.bootstrapSQL {
		user, certificate, key = "root", RuntimeCredentialDir+"/client.root.crt", RuntimeCredentialDir+"/client.root.key"
	}
	values := url.Values{"sslmode": {"verify-full"}, "sslrootcert": {"/cockroach/certs/ca.crt"}, "sslcert": {certificate}, "sslkey": {key}}
	// v26.2 gates this read-only identity function. Opt in only on the helper's
	// identity-probe sessions; do not change the cluster or Kine role defaults.
	if query == "SELECT crdb_internal.cluster_id()" {
		values.Set("options", "-c allow_unsafe_internals=true")
	}
	endpoint := "postgresql://" + user + "@" + net.JoinHostPort(host, "26257") + "/defaultdb?" + values.Encode()
	return w.command(ctx, "docker", "exec", "--user", "0", "cockroachdb", "/cockroach/cockroach", "sql", "--url", endpoint, "--format=csv", "--execute", query)
}

func (w Worker) Database(ctx context.Context) (string, error) {
	if !w.Init {
		return "", fmt.Errorf("database initialization/SQL setup requires --init")
	}
	initializer, err := w.Config.Cluster.Initializer()
	if err != nil {
		return "", err
	}
	if w.Config.Node != initializer.Name {
		return "", fmt.Errorf("only %s may initialize CockroachDB or run SQL setup", initializer.Name)
	}
	ready := w.Reachable
	if ready == nil {
		ready = func(ctx context.Context, host string) bool {
			conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(host, "26257"))
			if err != nil {
				return false
			}
			conn.Close()
			return true
		}
	}
	var listeners []cluster.Node
	for _, n := range w.Config.Cluster.Databases() {
		if ready(ctx, w.Config.Cluster.NodeAddress(n)) {
			listeners = append(listeners, n)
		}
	}
	var marker bootstrap.Marker
	raw, err := w.Store.Get(ctx, "bootstrap-state.json")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err == nil && json.Unmarshal(raw, &marker) != nil {
		return "", fmt.Errorf("invalid persistent bootstrap marker")
	}
	if marker.SQLReady && marker.ClusterID == "" {
		return "", fmt.Errorf("bootstrap SQL marker is missing its cluster identity")
	}
	if !marker.SQLReady {
		if err := w.prepareCredentials(); err != nil {
			return "", err
		}
		w.bootstrapSQL = true
	}
	id := ""
	for _, n := range listeners {
		id, err = bootstrap.ParseIdentity(w.sql(ctx, w.Config.Cluster.NodeAddress(n), "SELECT crdb_internal.cluster_id()"))
		if err != nil {
			return "", err
		}
		if id != "" {
			break
		}
	}
	if id == "" {
		if !marker.CanInitialize(w.Init) {
			return "", fmt.Errorf("database unavailable after recorded initialization; refusing to create a new cluster")
		}
		minimum := 3
		if w.Config.Cluster.SingleNodeLab {
			minimum = 1
		}
		localReady := false
		for _, n := range listeners {
			if n.Name == initializer.Name {
				localReady = true
			}
		}
		if !localReady || len(listeners) < minimum {
			return "", fmt.Errorf("waiting for %d database listeners including the initializer; found %d", minimum, len(listeners))
		}
		marker.InitializationAttempted = true
		intent, _ := json.Marshal(marker)
		// A create-only intent persists on the node's disk across helper restarts.
		// Even an ambiguous write/init outcome is never retried as a fresh init.
		if err := w.Store.Create(ctx, "bootstrap-state.json", intent); err != nil {
			return "", err
		}
		w.command(ctx, "docker", "exec", "--user", "0", "cockroachdb", "/cockroach/cockroach", "init", "--certs-dir="+RuntimeCredentialDir, "--host="+net.JoinHostPort(w.Config.Cluster.NodeAddress(initializer), "26257"))
		id, err = bootstrap.ParseIdentity(w.sql(ctx, w.Config.Cluster.NodeAddress(initializer), "SELECT crdb_internal.cluster_id()"))
		if err != nil || id == "" {
			return "", fmt.Errorf("initialization outcome requires an existing database identity; init will not be repeated")
		}
	}
	marker, err = marker.WithIdentity(id)
	if err != nil {
		return "", err
	}
	if marker.SQLReady {
		// Successful setup uses the limited Kine identity thereafter, even after
		// the temporary certificate expires. Also retry any interrupted cleanup.
		return id, w.cleanupCredentials()
	}
	data, _ := json.Marshal(marker)
	if err := w.Store.Put(ctx, "bootstrap-state.json", data); err != nil {
		return "", err
	}
	r := w.sql(ctx, w.Config.Cluster.NodeAddress(initializer), bootstrap.BootstrapSQL)
	if r.Code != 0 {
		return "", fmt.Errorf("database SQL bootstrap is not ready")
	}
	marker.SQLReady = true
	data, _ = json.Marshal(marker)
	if err := w.Store.Put(ctx, "bootstrap-state.json", data); err != nil {
		return "", err
	}
	return id, w.cleanupCredentials()
}

func (w Worker) Run(ctx context.Context) error {
	if w.Runner == nil {
		w.Runner = executil.OSRunner{}
	}
	if w.Store == nil {
		w.Store = localStore{dir: "/var/lib/infestation"}
	}
	if w.Config.Cluster.DiscoverPeers {
		if err := w.discoverTailnet(ctx); err != nil {
			return err
		}
	}
	err := w.Config.Cluster.Validate()
	if err != nil {
		return err
	}
	for _, n := range w.Config.Cluster.Nodes {
		if n.Name == w.Config.Node && n.Worker() {
			if w.Init {
				return fmt.Errorf("a K3s agent cannot initialize the database")
			}
			return w.cleanupCredentials()
		}
	}
	initializer, targetErr := w.Config.Cluster.Initializer()
	if targetErr != nil {
		return targetErr
	}
	leader := initializer.Name == w.Config.Node
	if w.Init && !leader {
		return fmt.Errorf("--init is only authorized on the configured initializer")
	}
	id := ""
	if leader && w.Init {
		id, err = w.Database(ctx)
		if err != nil {
			return err
		}
	}
	local := "/etc/rancher/k3s/k3s.yaml"
	if w.KubeconfigPath != "" {
		local = w.KubeconfigPath
	}
	if w.command(ctx, "k3s", "kubectl", "--kubeconfig", local, "--request-timeout=15s", "get", "--raw=/readyz").Code != 0 {
		return fmt.Errorf("local API is not ready")
	}
	data, err := os.ReadFile(local)
	if err != nil {
		return err
	}
	conf, err := bootstrap.PublicKubeconfig(data, w.Config.APIEndpoint)
	if err != nil {
		return fmt.Errorf("local kubeconfig is not ready")
	}
	data, _ = json.Marshal(conf)
	if err := w.Store.Put(ctx, "kubeconfig.json", data); err != nil {
		return err
	}
	if w.Config.Cluster.Tailscale != nil {
		if err := publishVMAccess(data); err != nil {
			return err
		}
	}
	// Operator access must remain available when a worker cannot register or
	// networking is unhealthy. Role reconciliation gates readiness, not publication.
	if err := w.labelWorkers(ctx, local); err != nil {
		return err
	}
	if !leader {
		return nil
	}
	for _, n := range w.Config.Cluster.Databases() {
		actual, err := bootstrap.ParseIdentity(w.sql(ctx, w.Config.Cluster.NodeAddress(n), "SELECT crdb_internal.cluster_id()"))
		if err != nil || actual != id {
			return fmt.Errorf("database identity on %s is not ready", n.Name)
		}
	}
	for _, n := range w.Config.Cluster.Nodes {
		if w.command(ctx, "k3s", "kubectl", "--kubeconfig", local, "--request-timeout=15s", "wait", "--for=condition=Ready", "node/"+n.Name, "--timeout=15s").Code != 0 {
			return fmt.Errorf("node %s is not ready", n.Name)
		}
	}
	status, _ := json.Marshal(struct {
		DeploymentID string `json:"deploymentID"`
		ClusterID    string `json:"clusterID"`
		Ready        bool   `json:"ready"`
	}{w.Config.DeploymentID, id, true})
	return w.Store.Put(ctx, "bootstrap-status.json", status)
}

// Kubernetes reserves role labels; apply them with the server's admin identity,
// rather than asking a kubelet to self-assign node-role.kubernetes.io/worker.
// Every API bootstrap pass reconciles workers, including VMSS scale-out nodes.
func (w Worker) labelWorkers(ctx context.Context, kubeconfig string) error {
	for _, n := range w.Config.Cluster.Nodes {
		if !n.Worker() {
			continue
		}
		if w.command(ctx, "k3s", "kubectl", "--kubeconfig", kubeconfig, "--request-timeout=15s", "label", "node", n.Name, "node-role.kubernetes.io/worker=worker", "--overwrite").Code != 0 {
			return fmt.Errorf("worker role on %s is not ready", n.Name)
		}
	}
	return nil
}

// localStore retains bootstrap identity and published access on the node disk.
type localStore struct{ dir string }

func (s localStore) Get(_ context.Context, name string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(s.dir, filepath.Base(name)))
	if os.IsNotExist(err) {
		return nil, os.ErrNotExist
	}
	return data, err
}
func (s localStore) Put(_ context.Context, name string, data []byte) error {
	return fsutil.WriteFile(filepath.Join(s.dir, filepath.Base(name)), data, 0600)
}
func (s localStore) Create(_ context.Context, name string, data []byte) error {
	if err := fsutil.PrivateDir(s.dir); err != nil {
		return err
	}
	return fsutil.CreatePrivateFile(filepath.Join(s.dir, filepath.Base(name)), data)
}

func Load(path string) (Config, error) {
	var config Config
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return config, err
	}
	err = json.Unmarshal(raw, &config)
	return config, err
}

// Publish access only through the explicit private VM-to-host channel, never
// serial console logs. Plain files are rejected to avoid accidental disclosure.
func publishVMAccess(data []byte) error {
	path := "/dev/virtio-ports/org.infestation.access"
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil
	} // Non-QEMU hosts need not expose a channel.
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("VM access channel is not a character device")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}
