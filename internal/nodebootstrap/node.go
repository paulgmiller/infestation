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
	"time"

	"infestation/internal/blobstore"
	"infestation/internal/bootstrap"
	"infestation/internal/cluster"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
)

type Config struct {
	Cluster      cluster.Config `json:"cluster"`
	Azure        *AzurePool     `json:"azure,omitempty"`
	Node         string         `json:"node"`
	APIEndpoint  string         `json:"apiEndpoint"`
	Account      string         `json:"account"`
	DeploymentID string         `json:"deploymentID"`
}

type Store interface {
	Get(context.Context, blobstore.Location) ([]byte, error)
	Put(context.Context, blobstore.Location, []byte) error
	Create(context.Context, blobstore.Location, []byte) error
}

type Worker struct {
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

func (w Worker) location(name string) blobstore.Location {
	return blobstore.Location{Account: w.Config.Account, Container: "access", Blob: name}
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
	initializer, err := w.Config.Cluster.Initializer()
	if err != nil {
		return "", err
	}
	if w.Config.Node != initializer.Name {
		return "", fmt.Errorf("only %s may initialize CockroachDB or run SQL setup", initializer.Name)
	}
	for _, n := range w.Config.Cluster.Databases() {
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
		if !ready(ctx, n.Address()) {
			return "", fmt.Errorf("database listener on %s is not ready", n.Name)
		}
	}
	var marker bootstrap.Marker
	raw, err := w.Store.Get(ctx, w.location("bootstrap-state.json"))
	if err != nil && !errors.Is(err, blobstore.ErrNotFound) {
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
	for _, n := range w.Config.Cluster.Databases() {
		id, err = bootstrap.ParseIdentity(w.sql(ctx, n.Address(), "SELECT crdb_internal.cluster_id()"))
		if err != nil {
			return "", err
		}
		if id != "" {
			break
		}
	}
	if id == "" {
		if !marker.CanInitialize(true) {
			return "", fmt.Errorf("database unavailable after recorded initialization; refusing to create a new cluster")
		}
		marker.InitializationAttempted = true
		intent, _ := json.Marshal(marker)
		// A create-only remote intent survives replacement of the bootstrap VM.
		// Even an ambiguous write/init outcome is never retried as a fresh init.
		if err := w.Store.Create(ctx, w.location("bootstrap-state.json"), intent); err != nil {
			return "", err
		}
		w.command(ctx, "docker", "exec", "--user", "0", "cockroachdb", "/cockroach/cockroach", "init", "--certs-dir="+RuntimeCredentialDir, "--host="+net.JoinHostPort(initializer.Address(), "26257"))
		id, err = bootstrap.ParseIdentity(w.sql(ctx, initializer.Address(), "SELECT crdb_internal.cluster_id()"))
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
	if err := w.Store.Put(ctx, w.location("bootstrap-state.json"), data); err != nil {
		return "", err
	}
	r := w.sql(ctx, initializer.Address(), bootstrap.BootstrapSQL)
	if r.Code != 0 {
		return "", fmt.Errorf("database SQL bootstrap is not ready")
	}
	marker.SQLReady = true
	data, _ = json.Marshal(marker)
	if err := w.Store.Put(ctx, w.location("bootstrap-state.json"), data); err != nil {
		return "", err
	}
	return id, w.cleanupCredentials()
}

func (w Worker) Run(ctx context.Context) error {
	if w.Config.Azure != nil {
		if w.Config.Azure.Role == "worker" {
			return w.cleanupCredentials()
		}
		if err := w.discoverAzure(ctx); err != nil {
			return err
		}
		if w.Config.Node != "api-0" {
			if err := w.cleanupCredentials(); err != nil {
				return err
			}
		}
	}
	if err := w.Config.Cluster.Validate(); err != nil {
		return err
	}
	initializer, err := w.Config.Cluster.Initializer()
	if err != nil {
		return err
	}
	if w.Runner == nil {
		w.Runner = executil.OSRunner{}
	}
	if w.Store == nil {
		if w.Config.Account == "" {
			w.Store = localStore{}
		} else {
			w.Store = blobstore.Client{Token: blobstore.ManagedIdentityToken}
		}
	}
	leader := initializer.Name == w.Config.Node
	id := ""
	if leader {
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
	if err := w.labelWorkers(ctx, local); err != nil {
		return err
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
	if err := w.Store.Put(ctx, w.location("kubeconfig.json"), data); err != nil {
		return err
	}
	if !leader {
		return nil
	}
	for _, n := range w.Config.Cluster.Databases() {
		actual, err := bootstrap.ParseIdentity(w.sql(ctx, n.Address(), "SELECT crdb_internal.cluster_id()"))
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
	return w.Store.Put(ctx, w.location("bootstrap-status.json"), status)
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

type localStore struct{}

func (localStore) Get(_ context.Context, l blobstore.Location) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join("/var/lib/infestation", filepath.Base(l.Blob)))
	if os.IsNotExist(err) {
		return nil, blobstore.ErrNotFound
	}
	return data, err
}
func (localStore) Put(_ context.Context, l blobstore.Location, data []byte) error {
	return fsutil.WriteFile(filepath.Join("/var/lib/infestation", filepath.Base(l.Blob)), data, 0600)
}
func (localStore) Create(_ context.Context, l blobstore.Location, data []byte) error {
	if err := os.MkdirAll("/var/lib/infestation", 0700); err != nil {
		return err
	}
	return fsutil.CreatePrivateFile(filepath.Join("/var/lib/infestation", filepath.Base(l.Blob)), data)
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
