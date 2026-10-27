package pki

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"infestation/internal/cluster"
	"infestation/internal/fsutil"
)

// PoolCertificate issues one cluster node identity for dynamic DNS names. Only
// this leaf key enters VMSS profiles; the CA and administrator keys stay local.
// Existing credentials are validated and reused, never silently replaced.
func PoolCertificate(state, zone string) (map[string][]byte, error) {
	if !cluster.ValidSAN(zone) || !strings.Contains(zone, ".") || net.ParseIP(zone) != nil {
		return nil, fmt.Errorf("invalid private DNS zone")
	}
	dir := filepath.Join(state, "pki", "pool")
	files := map[string][]byte{}
	for _, name := range []string{"ca.crt", "node.crt", "node.key"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			files[name] = data
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	if len(files) > 0 {
		if len(files) != 3 {
			return nil, fmt.Errorf("incomplete existing pool PKI; refusing replacement")
		}
		pair, err := tls.X509KeyPair(files["node.crt"], files["node.key"])
		if err != nil {
			return nil, err
		}
		leaf, err := x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, err
		}
		caPEM, err := os.ReadFile(filepath.Join(state, "pki", "ca.crt"))
		if err != nil {
			return nil, err
		}
		roots := x509.NewCertPool()
		roots.AppendCertsFromPEM(caPEM)
		if string(caPEM) != string(files["ca.crt"]) {
			return nil, fmt.Errorf("pool CA differs from cluster CA")
		}
		for _, usage := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
			if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: "worker-12345." + zone, KeyUsages: []x509.ExtKeyUsage{usage}}); err != nil {
				return nil, fmt.Errorf("existing pool certificate: %w", err)
			}
		}
		if leaf.IsCA || leaf.Subject.CommonName != "node" {
			return nil, fmt.Errorf("invalid pool identity")
		}
		return files, nil
	}
	caPEM, err := os.ReadFile(filepath.Join(state, "pki", "ca.crt"))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(state, "pki", "ca.key"))
	if err != nil {
		return nil, err
	}
	caBlock, _ := pem.Decode(caPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if caBlock == nil || keyBlock == nil {
		return nil, fmt.Errorf("invalid local CA")
	}
	ca, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		return nil, err
	}
	signer, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, err
	}
	private, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	id, err := serial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	leaf := &x509.Certificate{SerialNumber: id, Subject: pkix.Name{CommonName: "node"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(1, 0, 0), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, DNSNames: []string{"*." + zone, "localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &private.PublicKey, signer)
	if err != nil {
		return nil, err
	}
	files = map[string][]byte{"ca.crt": caPEM, "node.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "node.key": pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(private)})}
	if err := fsutil.PrivateDir(dir); err != nil {
		return nil, err
	}
	for name, data := range files {
		if err := fsutil.CreatePrivateFile(filepath.Join(dir, name), data); err != nil {
			return nil, err
		}
	}
	return files, nil
}
