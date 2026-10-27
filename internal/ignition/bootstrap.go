package ignition

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"

	"github.com/coreos/ignition/v2/config/v3_3/types"
	"infestation/internal/cluster"
	"infestation/internal/nodebootstrap"
)

type Bootstrap struct {
	Account, HelperSHA256, DeploymentID, APIEndpoint string
	LocalHelper                                      string
	Credential                                       []byte
	Pool                                             *nodebootstrap.AzurePool
}

func bootstrapProfile(doc *types.Config, c cluster.Config, n cluster.Node, b Bootstrap) error {
	initializer, err := c.Initializer()
	if err != nil {
		return err
	}
	if n.Worker() && b.Pool == nil {
		return nil
	}
	add := func(path, text string, mode int) {
		doc.Storage.Files = append(doc.Storage.Files, File(path, []byte(text), mode))
	}
	if n.Name == initializer.Name {
		var credential struct{ Certificate, Key []byte }
		if json.Unmarshal(b.Credential, &credential) != nil || len(credential.Certificate) == 0 || len(credential.Key) == 0 {
			return fmt.Errorf("the -0 Ignition profile requires a short-lived SQL bootstrap credential")
		}
		add(nodebootstrap.BootstrapCredentialDir+"/client.root.crt", string(credential.Certificate), 0600)
		add(nodebootstrap.BootstrapCredentialDir+"/client.root.key", string(credential.Key), 0600)
	}
	config, err := json.Marshal(nodebootstrap.Config{Cluster: c, Node: n.Name, Account: b.Account, DeploymentID: b.DeploymentID, APIEndpoint: b.APIEndpoint, Azure: b.Pool})
	if err != nil {
		return err
	}
	add("/etc/infestation/bootstrap.json", string(config), 0600)
	executable := "/opt/infestation/infestation-node"
	if b.LocalHelper != "" {
		binary, err := os.ReadFile(b.LocalHelper)
		if err != nil {
			return err
		}
		doc.Storage.Files = append(doc.Storage.Files, File(executable, binary, 0755))
	} else {
		if !regexp.MustCompile(`^[a-z0-9]{3,24}$`).MatchString(b.Account) || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(b.HelperSHA256) {
			return fmt.Errorf("invalid node helper storage or checksum")
		}
		// The shell is only a tiny authenticated downloader. All orchestration is Go.
		loader := fmt.Sprintf(`#!/bin/sh
set -eu
umask 077
mkdir -p /opt/infestation
if ! echo '%s  /opt/infestation/infestation-node' | sha256sum -c --status; then
  token=$(curl --noproxy '*' -fsS --connect-timeout 5 --max-time 20 -H Metadata:true 'http://169.254.169.254/metadata/identity/oauth2/token?api-version=2018-02-01&resource=https%%3A%%2F%%2Fstorage.azure.com%%2F' | sed -n 's/.*"access_token"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
  test -n "$token"
  tmp=$(mktemp /opt/infestation/.node-XXXXXX)
  trap 'rm -f "$tmp"' EXIT
  printf 'header = "Authorization: Bearer %%s"\n' "$token" | curl -fsS --connect-timeout 10 --max-time 120 --config - -H 'x-ms-version: 2023-11-03' 'https://%s.blob.core.windows.net/bootstrap-assets/infestation-node-%s' -o "$tmp"
  echo "%s  $tmp" | sha256sum -c --status
  chmod 0755 "$tmp"
  mv "$tmp" /opt/infestation/infestation-node
fi
exec /opt/infestation/infestation-node "$@"
`, b.HelperSHA256, b.Account, b.HelperSHA256, b.HelperSHA256)
		add("/opt/infestation/bootstrap", loader, 0755)
		executable = "/opt/infestation/bootstrap"
	}
	unit := "[Unit]\nDescription=Infestation node-local bootstrap\nWants=network-online.target docker.service\nAfter=network-online.target docker.service\nStartLimitIntervalSec=0\n\n[Service]\nStandardOutput=journal+console\nStandardError=journal+console\nType=oneshot\nTimeoutStartSec=0\nExecStart=" + executable + "\nRestart=on-failure\nRestartSec=15s\n\n[Install]\nWantedBy=multi-user.target\n"
	timer := "[Unit]\nDescription=Refresh Infestation kubeconfig publication\n[Timer]\nOnBootSec=5min\nOnUnitInactiveSec=5min\nUnit=infestation-bootstrap.service\n[Install]\nWantedBy=timers.target\n"
	doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "infestation-bootstrap.service", Enabled: ptr(true), Contents: &unit}, types.Unit{Name: "infestation-bootstrap.timer", Enabled: ptr(true), Contents: &timer})
	if n.Worker() {
		return nil
	}
	host, port, err := ciliumAPIEndpoint(c, b)
	if err != nil {
		return err
	}
	// K3s' bootstrap Helm job runs in the host network, before CNI exists.
	replicas := 2
	if c.SingleNodeLab {
		replicas = 1
	}
	add("/var/lib/rancher/k3s/server/manifests/infestation-cilium.yaml", fmt.Sprintf(`apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: cilium
  namespace: kube-system
spec:
  bootstrap: true
  repo: https://helm.cilium.io/
  chart: cilium
  version: 1.20.4
  targetNamespace: kube-system
  valuesContent: |-
    operator:
      replicas: %d
    kubeProxyReplacement: true
    k8sServiceHost: %s
    k8sServicePort: %s
    cni:
      binPath: /opt/cni/bin
      confPath: /etc/cni/net.d
    ipam:
      operator:
        clusterPoolIPv4PodCIDRList: [10.42.0.0/16]
`, replicas, quoted(host), port), 0600)
	return nil
}

// Cilium must reach the API before it can implement Kubernetes Service IPs.
// Azure pools use the public API LB; static/local inventories can use a server.
func ciliumAPIEndpoint(c cluster.Config, b Bootstrap) (string, string, error) {
	endpoint := b.APIEndpoint
	if endpoint == "" {
		if b.Pool != nil || len(c.Servers()) == 0 {
			return "", "", fmt.Errorf("Cilium requires a direct HTTPS API endpoint for Azure pools")
		}
		endpoint = "https://" + net.JoinHostPort(c.Servers()[0].IP, "6443")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return "", "", fmt.Errorf("Cilium requires a direct HTTPS API endpoint without credentials, path, query or fragment")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", "", fmt.Errorf("invalid Cilium API endpoint port")
	}
	return u.Hostname(), port, nil
}
