// Package azureconfig holds explicit Azure inputs. It performs no SKU, quota,
// price or placement discovery.
package azureconfig

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/executil"
)

const DefaultRegion = "westus2"
const DefaultSKU = "Standard_B2s"

type Inputs struct{ Region, SKU string }

func (in Inputs) Defaults() (Inputs, error) {
	if in.Region == "" {
		in.Region = DefaultRegion
	}
	if in.SKU == "" {
		in.SKU = DefaultSKU
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9]{1,31}$`).MatchString(in.Region) {
		return in, fmt.Errorf("invalid Azure region")
	}
	if !regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{1,79}$`).MatchString(in.SKU) {
		return in, fmt.Errorf("invalid Azure SKU")
	}
	return in, nil
}

func Subscription(ctx context.Context, runner executil.Runner) (string, error) {
	result := runner.Run(ctx, "az", "account", "show", "--query", "id", "-o", "tsv")
	if err := result.Err("read Azure subscription"); err != nil {
		return "", err
	}
	id := strings.TrimSpace(result.Stdout)
	if id == "" {
		return "", fmt.Errorf("Azure subscription is empty")
	}
	return id, nil
}

// Configuration records requested inputs for safe retries, not a selected plan.
type Configuration struct {
	SubscriptionID string         `json:"subscriptionID"`
	Parameters     map[string]any `json:"parameters"`
}

func Configure(c cluster.Config, name string, in Inputs) (cluster.Config, map[string]any, error) {
	in, err := in.Defaults()
	if err != nil {
		return c, nil, err
	}
	if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,38}[a-z0-9]$`).MatchString(name) {
		return c, nil, fmt.Errorf("use an Azure resource name of 2–40 lowercase letters, digits or dashes")
	}
	if err := c.Validate(); err != nil {
		return c, nil, err
	}
	if c.SingleNodeLab {
		return c, nil, fmt.Errorf("Azure deployment requires the configured multi-node topology")
	}
	c.Nodes = append([]cluster.Node(nil), c.Nodes...)
	c.TLSSANs = append([]string(nil), c.TLSSANs...)
	hostname := name + "-api." + in.Region + ".cloudapp.azure.com"
	found := false
	for _, san := range c.TLSSANs {
		found = found || san == hostname
	}
	if !found {
		c.TLSSANs = append(c.TLSSANs, hostname)
	}
	// Managed persistent storage works with the explicit B-series default. Do not
	// infer temporary-disk capacity or choose another SKU on the user's behalf.
	if c.CockroachStorage == "" {
		c.CockroachStorage = "persistent"
	}
	api, worker := 0, 0
	for i := range c.Nodes {
		n := &c.Nodes[i]
		n.Init = !n.Worker() && api == 0
		if n.Worker() {
			n.Name = fmt.Sprintf("worker-%d", worker)
			worker++
		} else {
			n.Name = fmt.Sprintf("api-%d", api)
			api++
		}
		n.Locality = ""
		n.ClusterMAC = ""
	}
	settings := map[string]any{"name": name, "location": in.Region, "vmSize": in.SKU, "uniform": true, "privateDNSZone": name + "." + in.Region + ".internal", "apiCount": api, "workerCount": worker, "apiLoadBalancer": true, "apiDNSLabel": name + "-api", "apiAdminCIDRs": []string{"0.0.0.0/0"}, "vnetCIDR": "192.168.0.0/16", "osDiskSizeGB": 32, "osDiskStorageAccountType": "StandardSSD_LRS"}
	return c, settings, c.Validate()
}
