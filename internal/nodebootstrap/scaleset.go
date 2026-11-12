package nodebootstrap

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/fsutil"
	"github.com/paulgmiller/infestation/internal/tailnet"
)

// ScaleSet is the Azure-only identity contract for a shared Ignition profile.
// It does not depend on Azure DNS, storage or a control-plane API credential.
type ScaleSet struct {
	Name       string `json:"name"`
	Group      string `json:"group"`
	Location   string `json:"location"`
	Initialize bool   `json:"initialize,omitempty"`
}

func (s ScaleSet) node(m Metadata, template cluster.Node) (cluster.Node, error) {
	c := m.Compute
	if !cluster.ValidSAN(s.Name) || strings.Contains(s.Name, ".") || s.Group != s.Name+"-rg" || c.SubscriptionID == "" || c.VMScaleSetName != s.Name || !strings.EqualFold(c.ResourceGroupName, s.Group) || c.Location != s.Location {
		return cluster.Node{}, fmt.Errorf("IMDS identity does not match the scale-set profile")
	}
	parts := strings.Split(strings.Trim(c.ResourceID, "/"), "/")
	if len(parts) != 10 || !strings.EqualFold(parts[0], "subscriptions") || parts[1] != c.SubscriptionID || !strings.EqualFold(parts[2], "resourceGroups") || !strings.EqualFold(parts[3], s.Group) || !strings.EqualFold(parts[4], "providers") || !strings.EqualFold(parts[5], "Microsoft.Compute") || !strings.EqualFold(parts[6], "virtualMachineScaleSets") || parts[7] != s.Name || !strings.EqualFold(parts[8], "virtualMachines") || !regexp.MustCompile(`^(0|[1-9][0-9]*)$`).MatchString(parts[9]) {
		return cluster.Node{}, fmt.Errorf("invalid Uniform VMSS instance resource ID")
	}
	ordinal, err := strconv.ParseUint(parts[9], 10, 64)
	if err != nil {
		return cluster.Node{}, fmt.Errorf("invalid scale-set instance ordinal")
	}
	if s.Initialize && template.Worker() {
		return cluster.Node{}, fmt.Errorf("an agent scale set cannot initialize the database")
	}
	template.Name = s.Name + "-" + parts[9]
	if len(template.Name) > 63 {
		return cluster.Node{}, fmt.Errorf("scale-set node name is too long")
	}
	template.Init = s.Initialize && ordinal == 0
	template.IP = "192.0.2.1" // Tailscale preparation assigns the actual address later.
	template.Locality, err = azureLocality(m)
	if err != nil {
		return cluster.Node{}, err
	}
	return template, nil
}
func PrepareScaleSet(ctx context.Context, c Config, root string, runner executil.Runner) error {
	m, err := ReadMetadata(ctx)
	if err != nil {
		return err
	}
	return prepareScaleSet(ctx, c, m, root, runner)
}
func prepareScaleSet(ctx context.Context, c Config, m Metadata, root string, runner executil.Runner) error {
	if c.ScaleSet == nil || c.Cluster.Tailscale == nil {
		return fmt.Errorf("missing scale-set identity configuration")
	}
	var template cluster.Node
	for _, n := range c.Cluster.Nodes {
		if n.Name == c.Node {
			template = n
		}
	}
	if template.Name == "" {
		return fmt.Errorf("missing scale-set node template")
	}
	node, err := c.ScaleSet.node(m, template)
	if err != nil {
		return err
	}
	c.Node = node.Name
	c.Cluster.Nodes = []cluster.Node{node}
	if !node.Init {
		w := Worker{CredentialDir: filepath.Join(root, BootstrapCredentialDir), RuntimeCredentialDir: filepath.Join(root, RuntimeCredentialDir)}
		if err := w.cleanupCredentials(); err != nil {
			return err
		}
	}
	// The shared unit can contain --init. Override it on every instance before
	// the unit may start, so only actual instance zero can exercise that authority.
	command := "/opt/infestation/infestation-node"
	if node.Init {
		command += " --init"
	}
	if err := fsutil.WriteFile(filepath.Join(root, "/etc/systemd/system/infestation-bootstrap.service.d/20-instance-authority.conf"), []byte("[Service]\nExecStart=\nExecStart="+command+"\n"), 0644); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(root, tailnet.K3sPath))
	if err != nil {
		return err
	}
	var k3s map[string]any
	if err := yaml.Unmarshal(raw, &k3s); err != nil {
		return err
	}
	k3s["node-name"] = node.Name
	if !node.Worker() {
		sans, _ := k3s["tls-san"].([]any)
		k3s["tls-san"] = append(sans, c.Cluster.NodeAddress(node))
	}
	raw, err = yaml.Marshal(k3s)
	if err != nil {
		return err
	}
	if err := fsutil.WriteFile(filepath.Join(root, tailnet.K3sPath), raw, 0600); err != nil {
		return err
	}
	if err := fsutil.WriteJSON(filepath.Join(root, tailnet.ConfigPath), tailnet.Config{Name: node.Name, DNSName: c.Cluster.NodeAddress(node), Server: !node.Worker()}); err != nil {
		return err
	}
	if err := fsutil.WriteFile(filepath.Join(root, "/etc/hostname"), []byte(node.Name+"\n"), 0644); err != nil {
		return err
	}
	if err := fsutil.WriteJSON(filepath.Join(root, "/etc/infestation/bootstrap.json"), c); err != nil {
		return err
	}
	for _, command := range [][]string{{"hostnamectl", "set-hostname", node.Name}, {"systemctl", "daemon-reload"}} {
		if err := runner.Run(ctx, command[0], command[1:]...).Err("prepare scale-set host identity"); err != nil {
			return err
		}
	}
	return nil
}
