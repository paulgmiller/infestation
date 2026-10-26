package bootstrap

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"infestation/internal/executil"
	"infestation/internal/fsutil"
	"infestation/internal/testutil"
)

const testID = "96c1d420-f94b-43e3-845d-e946b2fcce1b"

type fakeRemote struct{}

func (fakeRemote) Run(context.Context, string, string) executil.Result {
	return executil.Result{Stdout: "active\n"}
}
func (fakeRemote) Tunnel(context.Context, string, string, int) (int, func(), error) {
	return 12345, func() {}, nil
}

type database struct {
	state             string
	initialized, fail bool
	inits             int
	t                 *testing.T
}

func (d *database) Run(ctx context.Context, name string, args ...string) executil.Result {
	cmd := strings.Join(args, " ")
	if strings.Contains(cmd, "--execute=SELECT") {
		if d.initialized {
			return executil.Result{Stdout: "cluster_id\n" + testID + "\n"}
		}
		return executil.Result{Code: 1}
	}
	for _, arg := range args {
		if arg == "init" {
			d.inits++
			var m Marker
			if err := fsutil.ReadJSON(filepath.Join(d.state, "bootstrap.json"), &m); err != nil || !m.InitializationAttempted {
				d.t.Fatal("intent must precede initialization")
			}
			if !d.fail {
				d.initialized = true
			}
			return executil.Result{}
		}
	}
	if strings.Contains(cmd, "--ranges") {
		return executil.Result{Stdout: "is_live,ranges_unavailable,ranges_underreplicated\ntrue,0,0\ntrue,0,0\ntrue,0,0\n"}
	}
	return executil.Result{}
}
func TestInitializeOnlyOnce(t *testing.T) {
	state := t.TempDir()
	db := &database{state: state, t: t}
	w := New(Options{StateDir: state, Cluster: testutil.Config(), Runner: db, Remote: fakeRemote{}, AllowInitialize: true, Log: io.Discard, Timeout: time.Millisecond})
	if err := w.Database(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := w.Database(context.Background()); err != nil {
		t.Fatal(err)
	}
	if db.inits != 1 {
		t.Fatal(db.inits)
	}
	var marker Marker
	fsutil.ReadJSON(filepath.Join(state, "bootstrap.json"), &marker)
	if marker.ClusterID != testID {
		t.Fatal(marker)
	}
	db.initialized = false
	if err := w.Database(context.Background()); err == nil {
		t.Fatal("lost quorum must not initialize a new database")
	}
	if db.inits != 1 {
		t.Fatal("second cluster initialized")
	}
}
func TestAmbiguousInitIsNeverRepeated(t *testing.T) {
	state := t.TempDir()
	db := &database{state: state, t: t, fail: true}
	w := New(Options{StateDir: state, Cluster: testutil.Config(), Runner: db, Remote: fakeRemote{}, AllowInitialize: true, Log: io.Discard, Timeout: time.Millisecond})
	if err := w.Database(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if err := w.Database(context.Background()); err == nil {
		t.Fatal("must require recovery")
	}
	if db.inits != 1 {
		t.Fatal("init repeated")
	}
}
func TestKubeconfigPreservesTLS(t *testing.T) {
	raw := []byte(`apiVersion: v1
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
`)
	c, err := PublicKubeconfig(raw, "https://api.example.test:6443")
	if err != nil {
		t.Fatal(err)
	}
	if c.Clusters[0].Cluster["certificate-authority-data"] != "dGVzdC1jYQ==" || c.Users[0].User["client-key-data"] != "dGVzdC1rZXk=" {
		t.Fatal("credentials changed")
	}
	if _, err = PublicKubeconfig(raw, "http://api.example.test"); err == nil {
		t.Fatal("insecure endpoint accepted")
	}
	state := t.TempDir()
	path := filepath.Join(state, "kubeconfig")
	if err = fsutil.WriteJSON(path, c); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatal("kubeconfig permissions")
	}
	if _, err = (Marker{ClusterID: testID}).WithIdentity("e9c1d420-f94b-43e3-845d-e946b2fcce1b"); err == nil {
		t.Fatal("changed cluster identity accepted")
	}
}
func TestMalformedStatusFailsClosed(t *testing.T) {
	for _, data := range []string{"garbage", "is_live,ranges_unavailable,ranges_underreplicated\ntrue,1,0\n", "is_live,ranges_unavailable,ranges_underreplicated\ntrue,0,0\n"} {
		if err := Healthy(data, 3); err == nil {
			t.Fatal(fmt.Sprint("accepted ", data))
		}
	}
}
