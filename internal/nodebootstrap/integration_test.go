package nodebootstrap

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulgmiller/infestation/internal/bootstrap"
	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/pki"
	"github.com/paulgmiller/infestation/internal/testutil"
)

type containerRunner struct {
	name string
	t    *testing.T
}

func (r containerRunner) Run(ctx context.Context, command string, args ...string) executil.Result {
	args = append([]string{}, args...)
	for i, arg := range args {
		if arg == "cockroachdb" {
			args[i] = r.name
		}
	}
	result := (executil.OSRunner{}).Run(ctx, command, args...)
	if result.Code != 0 {
		r.t.Logf("test command failed: %s", result.Stderr)
	}
	return result
}

// Opt-in: use the cached pinned image, no network/host ports, and disposable data.
func TestPinnedSQLBootstrap(t *testing.T) {
	if os.Getenv("INFESTATION_DOCKER_TEST") != "1" {
		t.Skip("set INFESTATION_DOCKER_TEST=1 for isolated Docker verification")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root := t.TempDir()
	c := testutil.Config()
	c.SingleNodeLab = true
	c.Nodes = c.Nodes[:1]
	c.Nodes[0].IP = "127.0.0.1"
	c.Nodes[0].DNSName = "api-0.test.westus2.internal"
	if err := pki.Create(c, root); err != nil {
		t.Fatal(err)
	}
	pool, err := pki.PoolCertificate(root, "test.westus2.internal")
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range pool {
		if err := os.WriteFile(filepath.Join(root, "pki", "kube-0", name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	state := &memoryStore{blobs: map[string][]byte{}}
	name := fmt.Sprintf("infestation-sql-bootstrap-%d", time.Now().UnixNano())
	w := Worker{Init: true, Config: Config{Cluster: c, Node: "kube-0"}, Store: state, Runner: containerRunner{name: name, t: t}, CredentialDir: filepath.Join(root, "source"), RuntimeCredentialDir: filepath.Join(root, "runtime"), CAPath: filepath.Join(root, "pki", "kube-0", "ca.crt"), Reachable: func(context.Context, string) bool { return true }}
	for _, dir := range []string{w.CredentialDir, w.RuntimeCredentialDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := pki.BootstrapCredential(root)
	if err != nil {
		t.Fatal(err)
	}
	var credential struct{ Certificate, Key []byte }
	if err := json.Unmarshal(raw, &credential); err != nil {
		t.Fatal(err)
	}
	for file, data := range map[string][]byte{"client.root.crt": credential.Certificate, "client.root.key": credential.Key} {
		if err := os.WriteFile(filepath.Join(w.CredentialDir, file), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) executil.Result { return (executil.OSRunner{}).Run(ctx, "docker", args...) }
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		if r := (executil.OSRunner{}).Run(cleanup, "docker", "rm", "--force", name); r.Code != 0 {
			t.Errorf("test container cleanup failed: %s", r.Stderr)
		}
	}()
	r := run("run", "--detach", "--pull", "never", "--name", name, "--network", "none", "--add-host", "api-0.test.westus2.internal:127.0.0.1", "--memory", "2g", "--cpus", "1", "--entrypoint", "/cockroach/cockroach", "--mount", "type=bind,src="+filepath.Join(root, "pki", "kube-0")+",dst=/cockroach/certs,readonly", "--mount", "type=bind,src="+w.RuntimeCredentialDir+",dst=/run/infestation,readonly", "--mount", "type=bind,src="+filepath.Join(root, "pki", "admin")+",dst=/run/test-admin,readonly", c.CockroachImage, "start", "--certs-dir=/cockroach/certs", "--join=127.0.0.1:26257", "--listen-addr=127.0.0.1:26257", "--http-addr=127.0.0.1:8080", "--store=type=mem,size=1GiB", "--cache=64MiB", "--max-sql-memory=64MiB")
	if err := r.Err("start isolated test database"); err != nil {
		t.Fatal(err)
	}
	for {
		logs := run("logs", "--tail", "100", name)
		if strings.Contains(logs.Stdout+logs.Stderr, "awaiting `cockroach init`") {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("test database did not wait for init")
		case <-time.After(time.Second):
		}
	}
	id, err := w.Database(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var marker bootstrap.Marker
	json.Unmarshal(state.blobs["bootstrap-state.json"], &marker)
	if marker.ClusterID != id || !marker.SQLReady {
		t.Fatal("SQL readiness/identity not persisted")
	}
	if _, err := os.Stat(filepath.Join(w.RuntimeCredentialDir, "client.root.key")); !os.IsNotExist(err) {
		t.Fatal("temporary credential not removed")
	}
	again, err := w.Database(ctx)
	if err != nil || again != id {
		t.Fatalf("normal Kine credentials cannot resume after credential cleanup: %v", err)
	}
	// Exercise the real SQL parser, configured zones and Console grant after replay.
	r = run("exec", "--user", "0", name, "/cockroach/cockroach", "sql", "--url=postgresql://root@127.0.0.1:26257/defaultdb?sslmode=verify-full&sslrootcert=/run/test-admin/ca.crt&sslcert=/run/test-admin/client.root.crt&sslkey=/run/test-admin/client.root.key", "--format=csv", "--execute", bootstrap.BootstrapSQL+"\nSHOW ZONE CONFIGURATIONS;\nSHOW SYSTEM GRANTS FOR dashboard;")
	if err := r.Err("verify three-voter bootstrap zones"); err != nil {
		t.Fatal(err)
	}
	reader := csv.NewReader(strings.NewReader(r.Stdout))
	reader.FieldsPerRecord = -1 // SQL command tags precede the two-column result.
	rows, err := reader.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	var dashboardSQLBlocked bool
	for _, row := range rows {
		if len(row) == 3 && row[0] == "dashboard" && row[1] == "NOSQLLOGIN" {
			dashboardSQLBlocked = true
		}
	}
	if !dashboardSQLBlocked {
		t.Fatal("dashboard account is missing NOSQLLOGIN after bootstrap replay")
	}
	for _, target := range []string{"RANGE default", "DATABASE system", "RANGE meta", "RANGE system", "RANGE liveness", "RANGE timeseries"} {
		found := false
		for _, row := range rows {
			if len(row) == 2 && row[0] == target {
				found = strings.Contains(row[1], "num_replicas = 3") && strings.Contains(row[1], "num_voters = 3")
			}
		}
		if !found {
			t.Fatalf("zone %s did not retain three replicas and three voters", target)
		}
	}
	t.Log("Initialized a fresh secure Cockroach store, created Kine access, removed temporary credentials, and resumed using Kine.")
}
