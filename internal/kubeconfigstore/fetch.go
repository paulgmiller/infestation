// Package kubeconfigstore retrieves admin credentials without SSH or local PKI.
package kubeconfigstore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
	"infestation/internal/blobstore"
	"infestation/internal/bootstrap"
	"infestation/internal/fsutil"
)

func Fetch(ctx context.Context, client blobstore.Client, outputs bootstrap.Outputs, path string, overwrite bool) error {
	if outputs.KubeconfigBlob == nil {
		return fmt.Errorf("deployment outputs have no kubeconfigBlob; update the Azure stack and complete bootstrap first")
	}
	if path == "" || path == "-" {
		return fmt.Errorf("choose a local kubeconfig file with --out")
	}
	if _, err := os.Lstat(path); err == nil && !overwrite {
		return fmt.Errorf("destination exists; use --force to replace it")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	data, err := client.Get(ctx, *outputs.KubeconfigBlob)
	if err != nil {
		return err
	}
	// Reject executable authentication and external credential file references.
	// This path only accepts the self-contained admin credentials we publish.
	var raw struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Clusters   []struct {
			Cluster map[string]any `yaml:"cluster"`
		} `yaml:"clusters"`
		Users []struct {
			User map[string]any `yaml:"user"`
		} `yaml:"users"`
	}
	if yaml.Unmarshal(data, &raw) != nil || raw.APIVersion != "v1" || raw.Kind != "Config" || len(raw.Clusters) != 1 || len(raw.Users) != 1 {
		return fmt.Errorf("blob does not contain a valid admin kubeconfig")
	}
	for _, key := range []string{"exec", "auth-provider", "token", "tokenFile", "client-certificate", "client-key"} {
		if _, exists := raw.Users[0].User[key]; exists {
			return fmt.Errorf("blob kubeconfig contains unsupported authentication")
		}
	}
	for _, key := range []string{"proxy-url", "certificate-authority", "insecure-skip-tls-verify"} {
		if _, exists := raw.Clusters[0].Cluster[key]; exists {
			return fmt.Errorf("blob kubeconfig contains unsupported TLS or proxy configuration")
		}
	}
	for _, pair := range []struct {
		values map[string]any
		key    string
	}{
		{raw.Clusters[0].Cluster, "certificate-authority-data"},
		{raw.Users[0].User, "client-certificate-data"},
		{raw.Users[0].User, "client-key-data"},
	} {
		if value, ok := pair.values[pair.key].(string); !ok || value == "" {
			return fmt.Errorf("blob kubeconfig is missing embedded credentials")
		}
	}
	conf, err := bootstrap.PublicKubeconfig(data, outputs.APIEndpoint)
	if err != nil {
		return fmt.Errorf("blob does not contain a valid admin kubeconfig for the deployment endpoint")
	}
	encoded, err := json.MarshalIndent(conf, "", "  ")
	if err != nil {
		return fmt.Errorf("encode kubeconfig failed")
	}
	encoded = append(encoded, '\n')
	if overwrite {
		return fsutil.WriteFile(path, encoded, 0600)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	// O_EXCL protects an existing file (including symlinks) even during a race.
	return fsutil.CreatePrivateFile(path, encoded)
}
