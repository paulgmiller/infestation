package deploy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
	"infestation/internal/azureplan"
	"infestation/internal/fsutil"
	"infestation/internal/testutil"
)

func TestPreviewPreparesNativeStateAndPreservesIdentity(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "infra/azure")
	os.MkdirAll(project, 0700)
	os.WriteFile(filepath.Join(project, "Pulumi.yaml"), []byte("name: fixture\nruntime: go\n"), 0600)
	tools := filepath.Join(root, "tools")
	os.Mkdir(tools, 0700)
	// Only these non-provisioning calls are accepted by the fake Pulumi executable.
	script := `#!/bin/sh
if [ "$1" = stack ] && [ "$2" = init ]; then
  printf 'encryptionsalt: fixture\n' > "Pulumi.$3.yaml"
  exit 0
fi
if [ "$1" = preview ]; then exit 0; fi
echo 'unexpected provisioning call' >&2
exit 1
`
	os.WriteFile(filepath.Join(tools, "pulumi"), []byte(script), 0700)
	t.Setenv("PATH", tools)
	state := filepath.Join(root, "plan")
	os.Mkdir(state, 0700)
	c := testutil.Config()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, _ := ssh.NewPublicKey(private.Public())
	c.SSHAuthorizedKey = string(ssh.MarshalAuthorizedKey(public))
	block, err := ssh.MarshalPrivateKey(private, "test-only")
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(root, "key")
	os.WriteFile(key, pem.EncodeToMemory(block), 0600)
	if err = fsutil.WriteJSON(filepath.Join(state, "cluster.json"), c); err != nil {
		t.Fatal(err)
	}
	if err = fsutil.WriteJSON(filepath.Join(state, "plan.json"), azureplan.Plan{SubscriptionID: "fixture", PulumiConfig: map[string]any{"name": "fixture"}}); err != nil {
		t.Fatal(err)
	}
	o := Options{RepoRoot: root, PlanDir: state, Stack: "fixture", PrivateKey: key, FlatcarVersion: "4757.2.1", PreviewOnly: true, Log: io.Discard}
	if err = Azure(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	ca, _ := os.ReadFile(filepath.Join(state, "pki/ca.crt"))
	token, _ := os.ReadFile(filepath.Join(state, "pki/k3s-token"))
	pass, _ := os.ReadFile(filepath.Join(state, "passphrase"))
	if err = Azure(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{"pki/ca.crt": ca, "pki/k3s-token": token, "passphrase": pass} {
		got, _ := os.ReadFile(filepath.Join(state, path))
		if string(got) != string(want) {
			t.Fatal("existing identity changed:", path)
		}
	}
	o.FlatcarVersion = "4757.2.2"
	if err = Azure(context.Background(), o); err == nil {
		t.Fatal("changed pinned image accepted")
	}
}
