package kubeconfigstore

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"infestation/internal/blobstore"
	"infestation/internal/bootstrap"
	"infestation/internal/executil"
)

const config = `apiVersion: v1
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

type auth struct{}

func (auth) Run(context.Context, string, ...string) executil.Result {
	return executil.Result{Stdout: `{"accessToken":"test"}`}
}

type transport func(*http.Request) (*http.Response, error)

func (t transport) RoundTrip(r *http.Request) (*http.Response, error) { return t(r) }
func client(data string) blobstore.Client {
	return blobstore.Client{Runner: auth{}, HTTP: &http.Client{Transport: transport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data))}, nil
	})}}
}
func outputs() bootstrap.Outputs {
	return bootstrap.Outputs{APIEndpoint: "https://regional-api.example.test:6443", KubeconfigBlob: &blobstore.Location{Account: "testaccount", Container: "access", Blob: "kubeconfig.json"}}
}
func TestFetchWithoutSSHAndProtectExistingFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "kubeconfig")
	if err := Fetch(context.Background(), client(config), outputs(), path, false); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("unsafe kubeconfig permissions")
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), outputs().APIEndpoint) || !strings.Contains(string(data), "dGVzdC1rZXk=") {
		t.Fatal("endpoint or credentials lost")
	}
	if err := Fetch(context.Background(), client(config), outputs(), path, false); err == nil {
		t.Fatal("existing kubeconfig overwritten")
	}
	if err := Fetch(context.Background(), client(config), outputs(), path, true); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(path), "symlink")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := Fetch(context.Background(), client(config), outputs(), link, false); err == nil {
		t.Fatal("symlink accepted without --force")
	}
}
func TestInvalidBlobNeverReplacesExistingKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{
		"PRIVATE-NOT-A-KUBECONFIG",
		strings.Replace(config, "kind: Config", "kind: Wrong", 1),
		config + "    exec:\n      command: evil\n",
		strings.Replace(config, "certificate-authority-data: dGVzdC1jYQ==", "insecure-skip-tls-verify: true", 1),
		strings.Replace(config, "client-certificate-data: dGVzdC1jZXJ0", "client-certificate: /some/file", 1),
	} {
		if err := Fetch(context.Background(), client(data), outputs(), path, true); err == nil || strings.Contains(err.Error(), "PRIVATE-NOT") {
			t.Fatalf("invalid data accepted or exposed: %v", err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != "original" {
			t.Fatal("failed fetch damaged existing file")
		}
	}
	o := outputs()
	o.APIEndpoint = "http://insecure.example"
	if err := Fetch(context.Background(), client(config), o, path, true); err == nil {
		t.Fatal("insecure endpoint accepted")
	}
	o.KubeconfigBlob = nil
	if err := Fetch(context.Background(), client(config), o, path, true); err == nil {
		t.Fatal("legacy deployment silently accepted")
	}
}
