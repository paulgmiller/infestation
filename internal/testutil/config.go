// Package testutil supplies public, disposable inventories to package tests.
package testutil

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"infestation/internal/cluster"
)

func Config() cluster.Config {
	public, _, _ := ed25519.GenerateKey(rand.Reader)
	key, _ := ssh.NewPublicKey(public)
	return cluster.Config{SSHAuthorizedKey: string(ssh.MarshalAuthorizedKey(key)), CockroachImage: "cockroachdb/cockroach:v26.2.7", K3sExtensionVersion: "v1.35.8+k3s1", Architecture: "x86-64", Nodes: []cluster.Node{{Name: "kube1", IP: "192.168.1.101"}, {Name: "kube2", IP: "192.168.1.102"}, {Name: "kube3", IP: "192.168.1.103"}}}
}
func FakePKI(t *testing.T, c cluster.Config, state string) {
	t.Helper()
	for _, n := range c.Nodes {
		dir := filepath.Join(state, "pki", n.Name)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"ca.crt", "node.crt", "node.key", "client.kine.crt", "client.kine.key"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte("FAKE-TEST-"+name), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := os.WriteFile(filepath.Join(state, "pki/k3s-token"), []byte("fixed-disposable-test-token-0123456789\n"), 0600); err != nil {
		t.Fatal(err)
	}
}
func JSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
