package pki

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"infestation/internal/testutil"
)

func TestNativeCertificatesAndNoOverwrite(t *testing.T) {
	c := testutil.Config()
	state := t.TempDir()
	if err := Create(c, state); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(state, "pki")
	caPEM, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	for _, entry := range []struct {
		file, cn string
		usage    x509.ExtKeyUsage
		dns      string
	}{{"kube1/node", "node", x509.ExtKeyUsageServerAuth, c.Nodes[0].IP}, {"kube1/node", "node", x509.ExtKeyUsageClientAuth, ""}, {"kube1/client.kine", "kine", x509.ExtKeyUsageClientAuth, ""}, {"admin/client.root", "root", x509.ExtKeyUsageClientAuth, ""}} {
		data, err := os.ReadFile(filepath.Join(dir, entry.file+".crt"))
		if err != nil {
			t.Fatal(err)
		}
		block, _ := pem.Decode(data)
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatal(err)
		}
		if cert.Subject.CommonName != entry.cn {
			t.Fatal("incorrect Cockroach identity")
		}
		if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: entry.dns, KeyUsages: []x509.ExtKeyUsage{entry.usage}}); err != nil {
			t.Fatal(err)
		}
		if _, err := tls.LoadX509KeyPair(filepath.Join(dir, entry.file+".crt"), filepath.Join(dir, entry.file+".key")); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(filepath.Join(dir, entry.file+".key"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("private key permissions")
		}
	}
	if err := Create(c, state); err == nil {
		t.Fatal("must not overwrite CA")
	}
	again, _ := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if string(again) != string(caPEM) {
		t.Fatal("existing CA replaced")
	}
}
