package nodebootstrap

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"infestation/internal/blobstore"
	"infestation/internal/bootstrap"
	"infestation/internal/cluster"
	"infestation/internal/executil"
	"infestation/internal/pki"
	"infestation/internal/testutil"
)

const identity = "96c1d420-f94b-43e3-845d-e946b2fcce1b"

type memoryStore struct {
	blobs      map[string][]byte
	failCreate bool
	writes     int
}

func (s *memoryStore) Get(_ context.Context, l blobstore.Location) ([]byte, error) {
	if b, ok := s.blobs[l.Blob]; ok {
		return b, nil
	}
	return nil, blobstore.ErrNotFound
}
func (s *memoryStore) Put(_ context.Context, l blobstore.Location, b []byte) error {
	s.blobs[l.Blob] = append([]byte{}, b...)
	s.writes++
	return nil
}
func (s *memoryStore) Create(ctx context.Context, l blobstore.Location, b []byte) error {
	if _, ok := s.blobs[l.Blob]; ok {
		return blobstore.ErrExists
	}
	if s.failCreate {
		return fmt.Errorf("ambiguous blob request")
	}
	return s.Put(ctx, l, b)
}

type dbRunner struct {
	// This fake checks sequencing/target selection, not real SQL authentication.
	t                     *testing.T
	store                 *memoryStore
	initialized, failInit bool
	inits                 int
	target                string
	failSQL               bool
}

func (r *dbRunner) Run(_ context.Context, name string, args ...string) executil.Result {
	if name == "k3s" {
		return executil.Result{}
	}
	if name != "docker" {
		r.t.Fatal("bootstrap must execute locally in the existing host container")
	}
	for _, arg := range args {
		if arg == "init" {
			if !strings.Contains(strings.Join(args, " "), "--host="+r.target+":26257") {
				r.t.Fatal("init targeted a different VM")
			}
			var marker bootstrap.Marker
			json.Unmarshal(r.store.blobs["bootstrap-state.json"], &marker)
			if !marker.InitializationAttempted {
				r.t.Fatal("persistent intent must precede init")
			}
			r.inits++
			r.initialized = !r.failInit
			return executil.Result{}
		}
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "postgresql://node@") {
		r.t.Fatal("node certificate cannot authenticate SQL")
	}
	if !strings.Contains(joined, "postgresql://root@") && !strings.Contains(joined, "postgresql://kine@") {
		r.t.Fatal("SQL must use temporary bootstrap root credentials or the existing Kine identity")
	}
	if strings.Contains(joined, "SELECT crdb_internal.cluster_id()") {
		if !r.initialized {
			return executil.Result{Code: 1}
		}
		return executil.Result{Stdout: "cluster_id\n" + identity + "\n"}
	}
	if r.failSQL {
		return executil.Result{Code: 1}
	}
	return executil.Result{}
}

var testCredentials struct {
	sync.Once
	raw, ca []byte
	err     error
}

func credentialFixture(t *testing.T) ([]byte, []byte) {
	t.Helper()
	testCredentials.Do(func() {
		state := t.TempDir()
		c := testutil.Config()
		c.SingleNodeLab = true
		c.Nodes = c.Nodes[:1]
		if testCredentials.err = pki.Create(c, state); testCredentials.err != nil {
			return
		}
		testCredentials.raw, testCredentials.err = pki.BootstrapCredential(state)
		if testCredentials.err == nil {
			testCredentials.ca, testCredentials.err = os.ReadFile(filepath.Join(state, "pki", "ca.crt"))
		}
	})
	if testCredentials.err != nil {
		t.Fatal(testCredentials.err)
	}
	return testCredentials.raw, testCredentials.ca
}
func worker(t *testing.T) (*Worker, *dbRunner, *memoryStore) {
	s := &memoryStore{blobs: map[string][]byte{}}
	r := &dbRunner{t: t, store: s, target: "192.168.1.101"}
	c := testutil.Config()
	w := &Worker{Config: Config{Cluster: c, Node: c.Servers()[0].Name, Account: "testaccount", APIEndpoint: "https://regional.example.test:6443", DeploymentID: "test"}, Runner: r, Store: s, Reachable: func(context.Context, string) bool { return true }}
	root := t.TempDir()
	w.CredentialDir, w.RuntimeCredentialDir, w.CAPath = filepath.Join(root, "source"), filepath.Join(root, "runtime"), filepath.Join(root, "ca.crt")
	raw, ca := credentialFixture(t)
	var credentials struct{ Certificate, Key []byte }
	if err := json.Unmarshal(raw, &credentials); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(w.CredentialDir, 0700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string][]byte{filepath.Join(w.CredentialDir, "client.root.crt"): credentials.Certificate, filepath.Join(w.CredentialDir, "client.root.key"): credentials.Key, w.CAPath: ca} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return w, r, s
}

func TestSQLSetupRetriesThenRemovesTemporaryCredentials(t *testing.T) {
	w, r, s := worker(t)
	r.failSQL = true
	if _, err := w.Database(context.Background()); err == nil {
		t.Fatal("SQL setup failure accepted")
	}
	if r.inits != 1 {
		t.Fatal("initial init missing")
	}
	if _, err := os.Stat(filepath.Join(w.CredentialDir, "client.root.key")); err != nil {
		t.Fatal("credential removed before SQL setup succeeded")
	}
	r.failSQL = false
	if _, err := w.Database(context.Background()); err != nil {
		t.Fatal(err)
	}
	if r.inits != 1 {
		t.Fatal("SQL retry reinitialized database")
	}
	var marker bootstrap.Marker
	json.Unmarshal(s.blobs["bootstrap-state.json"], &marker)
	if !marker.SQLReady {
		t.Fatal("SQL success not persisted before cleanup")
	}
	for _, path := range []string{filepath.Join(w.CredentialDir, "client.root.key"), filepath.Join(w.CredentialDir, "client.root.crt"), filepath.Join(w.RuntimeCredentialDir, "client.root.key"), filepath.Join(w.RuntimeCredentialDir, "client.root.crt")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("temporary SQL credential retained")
		}
	}
	if _, err := w.Database(context.Background()); err != nil {
		t.Fatalf("startup should use Kine after temporary credential deletion: %v", err)
	}
}

func TestBadCredentialStopsBeforeInitialization(t *testing.T) {
	w, r, _ := worker(t)
	if err := os.WriteFile(filepath.Join(w.CredentialDir, "client.root.key"), []byte("PRIVATE-INVALID-KEY"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Database(context.Background()); err == nil || r.inits != 0 || strings.Contains(err.Error(), "PRIVATE-INVALID-KEY") {
		t.Fatalf("invalid key accepted or exposed: %v", err)
	}
}

func TestExpiredAndLongLivedCredentialsStopBeforeInit(t *testing.T) {
	state := t.TempDir()
	c := testutil.Config()
	c.SingleNodeLab = true
	c.Nodes = c.Nodes[:1]
	if err := pki.Create(c, state); err != nil {
		t.Fatal(err)
	}
	raw, err := pki.BootstrapCredential(state)
	if err != nil {
		t.Fatal(err)
	}
	var credential struct{ Certificate, Key []byte }
	json.Unmarshal(raw, &credential)
	pair, err := tls.X509KeyPair(credential.Certificate, credential.Key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	caPEM, _ := os.ReadFile(filepath.Join(state, "pki", "ca.crt"))
	caBlock, _ := pem.Decode(caPEM)
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM, _ := os.ReadFile(filepath.Join(state, "pki", "ca.key"))
	keyBlock, _ := pem.Decode(keyPEM)
	caKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, expiry := range []time.Time{time.Now().Add(-time.Hour), time.Now().Add(365 * 24 * time.Hour)} {
		w, r, _ := worker(t)
		candidate := *leaf
		candidate.NotBefore = time.Now().Add(-2 * time.Hour)
		candidate.NotAfter = expiry
		der, err := x509.CreateCertificate(rand.Reader, &candidate, ca, leaf.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		for path, data := range map[string][]byte{w.CAPath: caPEM, filepath.Join(w.CredentialDir, "client.root.crt"): pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), filepath.Join(w.CredentialDir, "client.root.key"): credential.Key} {
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := w.Database(context.Background()); err == nil || r.inits != 0 {
			t.Fatal("expired or long-lived administrator credential accepted")
		}
	}
}

func TestOnlyOrdinalZeroInitializes(t *testing.T) {
	w, r, _ := worker(t)
	w.Config.Cluster.Nodes[0], w.Config.Cluster.Nodes[2] = w.Config.Cluster.Nodes[2], w.Config.Cluster.Nodes[0]
	if _, err := w.Database(context.Background()); err != nil || r.inits != 1 {
		t.Fatalf("ordinal zero must initialize regardless of order: %v", err)
	}
	for _, name := range []string{"kube-1", "kube-2", "worker-0", "unknown"} {
		w, r, s := worker(t)
		w.Config.Node = name
		if _, err := w.Database(context.Background()); err == nil || r.inits != 0 || s.writes != 0 {
			t.Fatalf("%s attempted initialization", name)
		}
	}
}
func TestInitAndRepeatedStartup(t *testing.T) {
	w, r, s := worker(t)
	for i := 0; i < 2; i++ {
		id, err := w.Database(context.Background())
		if err != nil || id != identity {
			t.Fatalf("bootstrap failed: %v", err)
		}
	}
	if r.inits != 1 {
		t.Fatal("initialization repeated")
	}
	r.initialized = false
	if _, err := w.Database(context.Background()); err == nil || r.inits != 1 {
		t.Fatal("quorum loss caused a new cluster")
	}
	var marker bootstrap.Marker
	json.Unmarshal(s.blobs["bootstrap-state.json"], &marker)
	if marker.ClusterID != identity {
		t.Fatal("identity not persisted")
	}
}
func TestAmbiguousInitAndUnavailablePeersNeverReinitialize(t *testing.T) {
	w, r, _ := worker(t)
	r.failInit = true
	for i := 0; i < 2; i++ {
		if _, err := w.Database(context.Background()); err == nil {
			t.Fatal("ambiguous init accepted")
		}
	}
	if r.inits != 1 {
		t.Fatal("ambiguous init repeated")
	}
	w, r, s := worker(t)
	s.failCreate = true
	if _, err := w.Database(context.Background()); err == nil || r.inits != 0 {
		t.Fatal("initialized without durable intent")
	}
	w, r, _ = worker(t)
	w.Reachable = func(context.Context, string) bool { return false }
	if _, err := w.Database(context.Background()); err == nil || r.inits != 0 {
		t.Fatal("initialized before peers listened")
	}
}
func TestPublicationAndRenewalDoNotNeedManagementTransport(t *testing.T) {
	w, r, s := worker(t)
	path := filepath.Join(t.TempDir(), "kubeconfig")
	raw := `apiVersion: v1
kind: Config
clusters:
- name: default
  cluster:
    server: https://127.0.0.1:6443
    certificate-authority-data: dGVzdC1jYQ==
users:
- name: default
  user:
    client-certificate-data: dGVzdC1jZXJ0
    client-key-data: dGVzdC1rZXk=
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	w.KubeconfigPath = path
	for i := 0; i < 2; i++ {
		if err := w.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if r.inits != 1 || !strings.Contains(string(s.blobs["kubeconfig.json"]), w.Config.APIEndpoint) || len(s.blobs["bootstrap-status.json"]) == 0 {
		t.Fatal("publication or initialization failed")
	}
}

type labelRunner struct {
	nodes []string
	fail  bool
}

func (r *labelRunner) Run(_ context.Context, name string, args ...string) executil.Result {
	joined := strings.Join(args, " ")
	if name != "k3s" || !strings.Contains(joined, "label node ") || !strings.Contains(joined, "node-role.kubernetes.io/worker=worker --overwrite") {
		return executil.Result{Code: 1}
	}
	for i, arg := range args {
		if arg == "node" && i+1 < len(args) {
			r.nodes = append(r.nodes, args[i+1])
		}
	}
	if r.fail {
		return executil.Result{Code: 1}
	}
	return executil.Result{}
}
func TestWorkerRoleDoesNotRelabelControlPlane(t *testing.T) {
	c := testutil.Config()
	c.Nodes = append(c.Nodes, cluster.Node{Name: "worker-0", IP: "192.168.1.10", Role: "worker"}, cluster.Node{Name: "worker-123", IP: "192.168.1.11", Role: "worker"})
	runner := &labelRunner{}
	w := Worker{Config: Config{Cluster: c}, Runner: runner}
	if err := w.labelWorkers(context.Background(), "/test/kubeconfig"); err != nil {
		t.Fatal(err)
	}
	if strings.Join(runner.nodes, ",") != "worker-0,worker-123" {
		t.Fatalf("wrong role targets: %v", runner.nodes)
	}
	runner.fail = true
	if w.labelWorkers(context.Background(), "/test/kubeconfig") == nil {
		t.Fatal("label failure ignored")
	}
}
