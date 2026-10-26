// Package pki creates Cockroach-compatible certificates without Docker or OpenSSL.
package pki

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"infestation/internal/cluster"
	"infestation/internal/fsutil"
)

func serial() (*big.Int, error) { return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128)) }
func key(path string, k *rsa.PrivateKey) error {
	return fsutil.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), 0600)
}
func cert(path string, der []byte) error {
	return fsutil.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644)
}

// Create refuses to replace a CA, token, or any existing PKI directory.
func Create(c cluster.Config, state string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if len(c.Servers()) == 0 {
		return fmt.Errorf("workers-only PKI needs the existing cluster CA; refusing to create another identity")
	}
	if err := fsutil.PrivateDir(state); err != nil {
		return err
	}
	dir := filepath.Join(state, "pki")
	if err := os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("create new PKI (existing PKI is never overwritten): %w", err)
	}
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	id, err := serial()
	if err != nil {
		return err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: id, Subject: pkix.Name{CommonName: "Infestation Cockroach CA"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	if err = key(filepath.Join(dir, "ca.key"), caKey); err != nil {
		return err
	}
	if err = cert(filepath.Join(dir, "ca.crt"), der); err != nil {
		return err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	issue := func(dst, filename, cn string, hosts []string, server bool) error {
		if err := os.MkdirAll(dst, 0700); err != nil {
			return err
		}
		if err := fsutil.WriteFile(filepath.Join(dst, "ca.crt"), caPEM, 0644); err != nil {
			return err
		}
		leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			return err
		}
		id, err := serial()
		if err != nil {
			return err
		}
		leaf := &x509.Certificate{SerialNumber: id, Subject: pkix.Name{CommonName: cn}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(5, 0, 0), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		if server {
			leaf.ExtKeyUsage = append(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
		}
		for _, host := range hosts {
			if ip := net.ParseIP(host); ip != nil {
				leaf.IPAddresses = append(leaf.IPAddresses, ip)
			} else {
				leaf.DNSNames = append(leaf.DNSNames, host)
			}
		}
		encoded, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
		if err != nil {
			return err
		}
		if err = key(filepath.Join(dst, filename+".key"), leafKey); err != nil {
			return err
		}
		return cert(filepath.Join(dst, filename+".crt"), encoded)
	}
	for _, n := range c.Nodes {
		if !n.Database() && n.Worker() {
			continue
		}
		dst := filepath.Join(dir, n.Name)
		if n.Database() {
			if err = issue(dst, "node", "node", []string{n.IP, n.Name, "localhost", "127.0.0.1", "::1"}, true); err != nil {
				return err
			}
		}
		if !n.Worker() {
			if err = issue(dst, "client.kine", "kine", nil, false); err != nil {
				return err
			}
		}
	}
	return issue(filepath.Join(dir, "admin"), "client.root", "root", nil, false)
}
