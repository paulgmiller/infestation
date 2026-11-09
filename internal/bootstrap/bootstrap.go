// Package bootstrap completes database and Kubernetes setup after VM provisioning.
package bootstrap

import (
	_ "embed"
	"encoding/csv"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
	"infestation/internal/blobstore"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
)

type Outputs struct {
	PublicIPs      map[string]string   `json:"publicIPs"`
	APIEndpoint    string              `json:"apiEndpoint"`
	KubeconfigBlob *blobstore.Location `json:"kubeconfigBlob,omitempty"`
}
type Marker struct {
	ClusterID               string `json:"clusterID,omitempty"`
	InitializationAttempted bool   `json:"initializationAttempted,omitempty"`
	KubernetesReady         bool   `json:"kubernetesReady,omitempty"`
	SQLReady                bool   `json:"sqlReady,omitempty"`
}

func (m Marker) CanInitialize(allow bool) bool {
	return allow && m.ClusterID == "" && !m.InitializationAttempted
}
func (m Marker) WithIdentity(id string) (Marker, error) {
	if _, err := uuid.Parse(id); err != nil {
		return m, fmt.Errorf("invalid database identity")
	}
	if m.ClusterID != "" && m.ClusterID != id {
		return m, fmt.Errorf("database identity changed; refusing another cluster")
	}
	m.ClusterID = id
	return m, nil
}
func ParseIdentity(result executil.Result) (string, error) {
	if result.Code != 0 {
		return "", nil
	}
	rows, err := csv.NewReader(strings.NewReader(result.Stdout)).ReadAll()
	if err != nil || len(rows) != 2 || len(rows[1]) != 1 {
		return "", fmt.Errorf("unexpected database identity response")
	}
	if _, err = uuid.Parse(rows[1][0]); err != nil {
		return "", err
	}
	return rows[1][0], nil
}

func Healthy(data string, count int) error {
	rows, err := csv.NewReader(strings.NewReader(data)).ReadAll()
	if err != nil || len(rows) < 2 {
		return fmt.Errorf("invalid database status")
	}
	cols := map[string]int{}
	for i, k := range rows[0] {
		cols[k] = i
	}
	for _, k := range []string{"is_live", "ranges_unavailable", "ranges_underreplicated"} {
		if _, ok := cols[k]; !ok {
			return fmt.Errorf("missing database status column %s", k)
		}
	}
	live := 0
	for _, row := range rows[1:] {
		if len(row) != len(rows[0]) {
			return fmt.Errorf("malformed database status row")
		}
		if row[cols["is_live"]] == "true" {
			live++
			if row[cols["ranges_unavailable"]] != "0" || row[cols["ranges_underreplicated"]] != "0" {
				return fmt.Errorf("database replicas not healthy")
			}
		}
	}
	if live < count {
		return fmt.Errorf("not all database nodes are live")
	}
	return nil
}

// BootstrapSQL is idempotent and embedded so binaries work outside the checkout.
//
//go:embed bootstrap.sql
var BootstrapSQL string

type Kubeconfig struct {
	APIVersion string `json:"apiVersion" yaml:"apiVersion"`
	Kind       string `json:"kind" yaml:"kind"`
	Clusters   []struct {
		Name    string         `json:"name" yaml:"name"`
		Cluster map[string]any `json:"cluster" yaml:"cluster"`
	} `json:"clusters" yaml:"clusters"`
	Users []struct {
		Name string         `json:"name" yaml:"name"`
		User map[string]any `json:"user" yaml:"user"`
	} `json:"users" yaml:"users"`
	Contexts       []map[string]any `json:"contexts" yaml:"contexts"`
	CurrentContext string           `json:"current-context" yaml:"current-context"`
}

func PublicKubeconfig(data []byte, endpoint string) (Kubeconfig, error) {
	var c Kubeconfig
	if err := yaml.Unmarshal(data, &c); err != nil {
		return c, err
	}
	u, urlErr := url.Parse(endpoint)
	if len(c.Clusters) != 1 || len(c.Users) != 1 || urlErr != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return c, fmt.Errorf("expected one admin kubeconfig and HTTPS endpoint")
	}
	if _, ok := c.Clusters[0].Cluster["certificate-authority-data"]; !ok {
		return c, fmt.Errorf("missing API CA")
	}
	if _, ok := c.Users[0].User["client-key-data"]; !ok {
		return c, fmt.Errorf("missing admin client key")
	}
	delete(c.Clusters[0].Cluster, "insecure-skip-tls-verify")
	c.Clusters[0].Name = "infestation"
	c.Clusters[0].Cluster["server"] = endpoint
	c.Users[0].Name = "infestation-admin"
	c.Contexts = []map[string]any{{"name": "infestation", "context": map[string]string{"cluster": "infestation", "user": "infestation-admin"}}}
	c.CurrentContext = "infestation"
	return c, nil
}

// LoadOutputs parses Pulumi's non-secret stack outputs without logging them.
func LoadOutputs(path string) (Outputs, error) {
	var o Outputs
	err := fsutil.ReadJSON(path, &o)
	return o, err
}
