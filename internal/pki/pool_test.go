package pki

import (
	"crypto/x509"
	"encoding/pem"
	"infestation/internal/testutil"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWildcardPoolIdentity(t *testing.T) {
	state := t.TempDir()
	if err := Create(testutil.Config(), state); err != nil {
		t.Fatal(err)
	}
	files, err := PoolCertificate(state, "test.westus2.internal")
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(files["node.crt"])
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"api-0.test.westus2.internal", "worker-999999.test.westus2.internal", "127.0.0.1"} {
		if err := leaf.VerifyHostname(host); err != nil {
			t.Fatal(err)
		}
	}
	for _, host := range []string{"api-0.other.westus2.internal", "nested.api-0.test.westus2.internal", "192.168.1.11"} {
		if leaf.VerifyHostname(host) == nil {
			t.Fatalf("accepted %s", host)
		}
	}
	if leaf.IsCA || leaf.KeyUsage&x509.KeyUsageCertSign != 0 {
		t.Fatal("node can sign certificates")
	}
	again, err := PoolCertificate(state, "test.westus2.internal")
	if err != nil || string(again["node.key"]) != string(files["node.key"]) {
		t.Fatal("existing leaf replaced", err)
	}
	if _, err := PoolCertificate(state, "other.westus2.internal"); err == nil {
		t.Fatal("reused leaf for different DNS zone")
	}
}

func TestDisabledLoginKeyIsPublicOnly(t *testing.T) {
	state := t.TempDir()
	if err := Create(testutil.Config(), state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(filepath.Join(state, "pki"))
	if err != nil {
		t.Fatal(err)
	}
	key, err := DisabledLoginPublicKey(state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "ssh-ed25519 ") || strings.Contains(key, "PRIVATE KEY") {
		t.Fatal("not a public key")
	}
	again, err := DisabledLoginPublicKey(state)
	if err != nil || again != key {
		t.Fatal("public key replaced", err)
	}
	after, err := os.ReadDir(filepath.Join(state, "pki"))
	if err != nil || len(after) != len(before)+1 {
		t.Fatal("unexpected credential files", err)
	}
}
