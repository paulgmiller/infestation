package nodebootstrap

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"infestation/internal/fsutil"
)

const BootstrapCredentialDir = "/etc/infestation/bootstrap-client"
const RuntimeCredentialDir = "/run/infestation"

func (w Worker) credentialPaths() (source, runtime, ca string) {
	source, runtime, ca = BootstrapCredentialDir, RuntimeCredentialDir, "/etc/cockroach/certs/ca.crt"
	if w.CredentialDir != "" {
		source = w.CredentialDir
	}
	if w.RuntimeCredentialDir != "" {
		runtime = w.RuntimeCredentialDir
	}
	if w.CAPath != "" {
		ca = w.CAPath
	}
	return
}

func (w Worker) prepareCredentials() error {
	source, runtime, caPath := w.credentialPaths()
	certificate, err := os.ReadFile(filepath.Join(source, "client.root.crt"))
	if err != nil {
		return fmt.Errorf("bootstrap SQL certificate is missing; fresh bootstrap credentials must be supplied to the -0 VM")
	}
	key, err := os.ReadFile(filepath.Join(source, "client.root.key"))
	if err != nil {
		return fmt.Errorf("bootstrap SQL private key is missing")
	}
	ca, err := os.ReadFile(caPath)
	if err != nil {
		return fmt.Errorf("read Cockroach CA for bootstrap: %w", err)
	}
	pair, err := tls.X509KeyPair(certificate, key)
	if err != nil {
		return fmt.Errorf("invalid bootstrap SQL certificate/key pair")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || leaf.Subject.CommonName != "root" || leaf.IsCA {
		return fmt.Errorf("bootstrap SQL certificate must be a root client certificate")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return fmt.Errorf("invalid Cockroach CA")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("bootstrap SQL certificate is expired, not yet valid or not signed by this cluster CA")
	}
	if leaf.NotAfter.After(time.Now().Add(65 * time.Minute)) {
		return fmt.Errorf("bootstrap SQL certificate must be short-lived; refusing a long-lived administrator credential")
	}
	if err := fsutil.PrivateDir(runtime); err != nil {
		return err
	}
	for name, data := range map[string][]byte{"ca.crt": ca, "client.root.crt": certificate, "client.root.key": key} {
		if err := fsutil.WriteFile(filepath.Join(runtime, name), data, 0600); err != nil {
			return err
		}
	}
	return nil
}

func (w Worker) cleanupCredentials() error {
	source, runtime, _ := w.credentialPaths()
	var result error
	for _, path := range []string{
		filepath.Join(source, "client.root.crt"), filepath.Join(source, "client.root.key"),
		filepath.Join(runtime, "client.root.crt"), filepath.Join(runtime, "client.root.key"), filepath.Join(runtime, "ca.crt"),
	} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			result = errors.Join(result, err)
		}
	}
	return result
}
