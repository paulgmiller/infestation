package bootstrap

import (
	"fmt"
	"infestation/internal/fsutil"
	"os"
	"path/filepath"
	"testing"
)

const testID = "96c1d420-f94b-43e3-845d-e946b2fcce1b"

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

func TestInitializationIdentityGuard(t *testing.T) {
	var m Marker
	if !m.CanInitialize(true) || m.CanInitialize(false) {
		t.Fatal("initialization authorization")
	}
	m.InitializationAttempted = true
	if m.CanInitialize(true) {
		t.Fatal("ambiguous initialization retried")
	}
	m.ClusterID = testID
	if _, err := m.WithIdentity("e9c1d420-f94b-43e3-845d-e946b2fcce1b"); err == nil {
		t.Fatal("replacement identity accepted")
	}
}
