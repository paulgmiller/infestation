package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulgmiller/infestation/internal/fsutil"
)

func TestKubeconfigWritesPrivateVerifiedTLSConfigWithoutPrintingCredentials(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "access")
	c := clusterConfig(ClusterOptions{Options: Options{Name: "access"}, Size: 1, Domain: "example.ts.net"})
	if err := fsutil.WriteJSON(filepath.Join(dir, "nodes.json"), c); err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{"apiVersion": "v1", "kind": "Config", "clusters": []any{map[string]any{"name": "default", "cluster": map[string]any{"server": "https://127.0.0.1:6443", "certificate-authority-data": "dGVzdC1jYQ==", "insecure-skip-tls-verify": true}}}, "users": []any{map[string]any{"name": "default", "user": map[string]any{"client-certificate-data": "dGVzdC1jZXJ0", "client-key-data": "DISPOSABLE-ADMIN-KEY"}}}}
	if err := fsutil.WriteJSON(filepath.Join(dir, "access-0", "access.json"), raw); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "client", "config")
	var out bytes.Buffer
	if err := Kubeconfig(context.Background(), Options{Name: "access", StateDir: root}, destination, false, &out); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(destination)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("kubeconfig is not private", err)
	}
	data, _ := os.ReadFile(destination)
	var conf map[string]any
	if err := json.Unmarshal(data, &conf); err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "https://k8s-api.example.ts.net:443") || !strings.Contains(text, "kubernetes.default.svc.cluster.local") || strings.Contains(text, "insecure-skip-tls-verify") {
		t.Fatal("kubeconfig lost endpoint or TLS validation")
	}
	if strings.Contains(out.String(), "DISPOSABLE-ADMIN-KEY") || strings.TrimSpace(out.String()) != destination {
		t.Fatal("CLI exposed admin credentials")
	}
	if err := Kubeconfig(context.Background(), Options{Name: "access", StateDir: root}, destination, false, &out); err == nil {
		t.Fatal("existing credentials overwritten without --force")
	}
	after, _ := os.ReadFile(destination)
	if !bytes.Equal(after, data) {
		t.Fatal("failed retrieval changed credentials")
	}
	if err := Kubeconfig(context.Background(), Options{Name: "access", StateDir: root}, destination, true, &out); err != nil {
		t.Fatal("explicit refresh failed", err)
	}
}
