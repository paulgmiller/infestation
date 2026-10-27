package pki

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"infestation/internal/testutil"
)

func TestBootstrapCredentialIsShortLivedAndSeparate(t *testing.T) {
	state := t.TempDir()
	if err := Create(testutil.Config(), state); err != nil {
		t.Fatal(err)
	}
	caBefore, _ := os.ReadFile(filepath.Join(state, "pki", "ca.key"))
	adminBefore, _ := os.ReadFile(filepath.Join(state, "pki", "admin", "client.root.key"))
	raw, err := BootstrapCredential(state)
	if err != nil {
		t.Fatal(err)
	}
	var credential struct{ Certificate, Key []byte }
	if err := json.Unmarshal(raw, &credential); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(credential.Certificate, credential.Key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.Subject.CommonName != "root" || leaf.IsCA || time.Until(leaf.NotAfter) > time.Hour || time.Until(leaf.NotAfter) < 59*time.Minute {
		t.Fatal("unexpected bootstrap certificate identity or lifetime")
	}
	caPEM, _ := os.ReadFile(filepath.Join(state, "pki", "ca.crt"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(credential.Key, caBefore) || bytes.Equal(credential.Key, adminBefore) {
		t.Fatal("bootstrap reused a long-lived private key")
	}
	caAfter, _ := os.ReadFile(filepath.Join(state, "pki", "ca.key"))
	adminAfter, _ := os.ReadFile(filepath.Join(state, "pki", "admin", "client.root.key"))
	if !bytes.Equal(caBefore, caAfter) || !bytes.Equal(adminBefore, adminAfter) {
		t.Fatal("credential issuance changed existing PKI")
	}
}

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
	}{{"kube-0/node", "node", x509.ExtKeyUsageServerAuth, c.Nodes[0].IP}, {"kube-0/node", "node", x509.ExtKeyUsageClientAuth, ""}, {"kube-0/client.kine", "kine", x509.ExtKeyUsageClientAuth, ""}, {"admin/client.root", "root", x509.ExtKeyUsageClientAuth, ""}} {
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
