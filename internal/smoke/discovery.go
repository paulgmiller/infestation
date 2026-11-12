package smoke

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"strings"

	"infestation/internal/executil"
)

// Node is discovered from the Kubernetes API, not provisioning state.
type Node struct {
	Name string `json:"name"`
	IP   string `json:"internal_ip,omitempty"`
}

var nodeLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

func validNodeName(name string) bool {
	if len(name) == 0 || len(name) > 253 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !nodeLabel.MatchString(label) {
			return false
		}
	}
	return true
}
func kubectlArgs(kube string, args ...string) []string {
	if kube == "" {
		return args
	} // Let kubectl honor KUBECONFIG/current context.
	return append([]string{"--kubeconfig", kube}, args...)
}
func discoverNodes(ctx context.Context, runner executil.Runner, kube string, latency bool) ([]Node, error) {
	result := runner.Run(ctx, "kubectl", kubectlArgs(kube, "get", "nodes", "-o", "json")...)
	if err := result.Err("discover Kubernetes nodes"); err != nil {
		return nil, err
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name              string
				DeletionTimestamp *string
			}
			Status struct {
				Conditions []struct{ Type, Status string }
				Addresses  []struct{ Type, Address string }
			}
		}
	}
	if err := json.Unmarshal([]byte(result.Stdout), &list); err != nil {
		return nil, fmt.Errorf("invalid Kubernetes node list")
	}
	var nodes []Node
	seen := map[string]bool{}
	for _, item := range list.Items {
		name := item.Metadata.Name
		if !validNodeName(name) || seen[name] {
			return nil, fmt.Errorf("invalid or duplicate Kubernetes node name")
		}
		seen[name] = true
		ready := false
		for _, c := range item.Status.Conditions {
			if c.Type == "Ready" && c.Status == "True" {
				ready = true
			}
		}
		if !ready || item.Metadata.DeletionTimestamp != nil {
			return nil, fmt.Errorf("node %s is not Ready or is being deleted; refusing incomplete coverage", name)
		}
		n := Node{Name: name}
		for _, a := range item.Status.Addresses {
			if a.Type != "InternalIP" {
				continue
			}
			ip, err := netip.ParseAddr(a.Address)
			if err != nil {
				continue
			}
			if n.IP == "" {
				n.IP = ip.String()
			}
			if netip.MustParsePrefix("100.64.0.0/10").Contains(ip) {
				n.IP = ip.String()
				break
			}
		}
		if latency {
			ip, err := netip.ParseAddr(n.IP)
			if err != nil || !netip.MustParsePrefix("100.64.0.0/10").Contains(ip) {
				return nil, fmt.Errorf("node %s has no Tailscale InternalIP for --tailnet-latency", name)
			}
		}
		nodes = append(nodes, n)
	}
	if len(nodes) < 2 {
		return nil, fmt.Errorf("cross-node checks require at least two Ready Kubernetes nodes")
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, nil
}
