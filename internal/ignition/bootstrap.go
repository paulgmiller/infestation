package ignition

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/coreos/ignition/v2/config/v3_3/types"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/nodebootstrap"
)

type Bootstrap struct {
	ScaleSet                               *nodebootstrap.ScaleSet
	JoinBundle                             []byte // private leaf credentials/token; never CA/admin signing keys
	HelperSourceCommit, HelperBuilderImage string
	// Render-only secret: never serialize into the node's bootstrap inventory.
	TailscaleAuthKey          string `json:"-"`
	DeploymentID, APIEndpoint string
	LocalHelper               string
	Credential                []byte
}

func bootstrapProfile(doc *types.Config, c cluster.Config, n cluster.Node, b Bootstrap) error {
	_, err := c.Initializer()
	if err != nil && !c.DiscoverPeers {
		return err
	}
	if n.Worker() && !c.DiscoverPeers {
		return nil
	}
	add := func(path, text string, mode int) {
		doc.Storage.Files = append(doc.Storage.Files, File(path, []byte(text), mode))
	}
	if n.Init {
		if c.DiscoverPeers && len(b.JoinBundle) == 0 {
			return fmt.Errorf("initializer requires a private enrollment bundle")
		}
		var credential struct{ Certificate, Key []byte }
		if json.Unmarshal(b.Credential, &credential) != nil || len(credential.Certificate) == 0 || len(credential.Key) == 0 {
			return fmt.Errorf("the initializer Ignition profile requires a short-lived SQL bootstrap credential")
		}
		add(nodebootstrap.BootstrapCredentialDir+"/client.root.crt", string(credential.Certificate), 0600)
		add(nodebootstrap.BootstrapCredentialDir+"/client.root.key", string(credential.Key), 0600)
	}
	endpoint := b.APIEndpoint
	if c.Tailscale != nil {
		endpoint = "https://" + net.JoinHostPort(c.NodeAddress(n), "6443")
	}
	nodeConfig := c
	if c.DiscoverPeers {
		nodeConfig.Nodes = []cluster.Node{n}
	}
	config, err := json.Marshal(nodebootstrap.Config{ScaleSet: b.ScaleSet, Cluster: nodeConfig, Node: n.Name, DeploymentID: b.DeploymentID, APIEndpoint: endpoint})
	if err != nil {
		return err
	}
	add("/etc/infestation/bootstrap.json", string(config), 0600)
	executable := "/opt/infestation/infestation-node"
	if b.HelperSourceCommit != "" {
		// The Ignition-installed build service supplies the executable before bootstrap.
	} else if b.LocalHelper != "" {
		binary, err := os.ReadFile(b.LocalHelper)
		if err != nil {
			return err
		}
		if c.Tailscale == nil {
			doc.Storage.Files = append(doc.Storage.Files, File(executable, binary, 0755))
		}
	} else {
		return fmt.Errorf("node helper source commit or local binary is required")
	}
	if n.Init {
		executable += " --init"
	}
	unit := "[Unit]\nDescription=Infestation node-local bootstrap\nWants=network-online.target docker.service\nAfter=network-online.target docker.service\nStartLimitIntervalSec=0\n\n[Service]\nStandardOutput=journal+console\nStandardError=journal+console\nType=oneshot\nTimeoutStartSec=0\nExecStart=" + executable + "\nRestart=on-failure\nRestartSec=15s\n\n[Install]\nWantedBy=multi-user.target\n"
	timer := "[Unit]\nDescription=Refresh Infestation kubeconfig publication\n[Timer]\nOnBootSec=5min\nOnUnitInactiveSec=5min\nUnit=infestation-bootstrap.service\n[Install]\nWantedBy=timers.target\n"
	if c.DiscoverPeers {
		unit = strings.Replace(unit, "Wants=network-online.target docker.service", "Requires=infestation-join.service\nAfter=infestation-join.service\nWants=network-online.target docker.service", 1)
	}
	doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "infestation-bootstrap.service", Enabled: ptr(true), Contents: &unit}, types.Unit{Name: "infestation-bootstrap.timer", Enabled: ptr(true), Contents: &timer})
	if n.Worker() || c.Tailscale != nil {
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
// Static/local inventories can use a server.
func ciliumAPIEndpoint(c cluster.Config, b Bootstrap) (string, string, error) {
	endpoint := b.APIEndpoint
	if endpoint == "" {
		if len(c.Servers()) == 0 {
			return "", "", fmt.Errorf("Cilium requires a direct HTTPS API endpoint")
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
