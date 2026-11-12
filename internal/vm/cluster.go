package vm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/paulgmiller/infestation/internal/azureconfig"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/fsutil"
	"github.com/paulgmiller/infestation/internal/ignition"
	"github.com/paulgmiller/infestation/internal/image"
	"github.com/paulgmiller/infestation/internal/nodebootstrap"
	"github.com/paulgmiller/infestation/internal/pki"
)

type ClusterOptions struct {
	Options
	Domain                              string
	HelperSourceRef, HelperBuilderImage string
	AzureLocation, AzureVMSize          string
	Provider                            string
	Host, Site                          string
	Size                                int
	Agent                               bool
	Init                                bool
}

func clusterConfig(o ClusterOptions) cluster.Config {
	c := cluster.Config{DiscoverPeers: true, CockroachImage: "cockroachdb/cockroach:v26.2.7", K3sExtensionVersion: "v1.35.8+k3s1", Architecture: "x86-64", CockroachStorage: "persistent", CockroachCache: "256MiB", CockroachSQLMemory: "256MiB", Tailscale: &cluster.Tailscale{Version: TailscaleVersion, Domain: o.Domain}}
	topology := cluster.Topology{Cloud: "qemu", Site: o.Site, Host: o.Host}
	if o.Provider == "azure" {
		// IMDS adds the actual zone and subscription before enrollment.
		topology = cluster.Topology{Cloud: "azure", Region: o.AzureLocation, ResourceGroup: o.Name + "-rg"}
	}
	for i := 0; i < o.Size; i++ {
		c.Nodes = append(c.Nodes, cluster.Node{Init: o.Init && i == 0, Name: fmt.Sprintf("%s-%d", o.Name, i), IP: fmt.Sprintf("192.0.2.%d", i+1), Locality: topology.Locality()})
	}
	if o.Agent {
		for i := range c.Nodes {
			c.Nodes[i].Role = "worker"
			c.Nodes[i].ServerURL = "https://k8s-api." + o.Domain + ":443"
		}
	}
	c.Tailscale.SSH = true
	c.APITailnetHostname = "k8s-api." + o.Domain
	return c
}

// CreateCluster creates independent node profiles and compute on one provider.
// Only --init creates CA/token/bootstrap credentials. All other nodes enroll
// through the tailnet after boot and never need the initializer's workstation.
func CreateCluster(ctx context.Context, o ClusterOptions, out io.Writer) error {
	if o.Init && o.Agent {
		return fmt.Errorf("--init requires an API server; it cannot be combined with --agent")
	}
	if o.Provider == "" {
		o.Provider = "qemu"
	}
	if o.Provider != "qemu" && o.Provider != "azure" {
		return fmt.Errorf("unknown compute provider")
	}
	for _, value := range []string{o.Host, o.Site} {
		if value != "" && !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(value) {
			return fmt.Errorf("host/site labels must contain only letters, digits, dots, underscores or dashes")
		}
	}
	if o.Provider == "azure" && (o.Host != "" || o.Site != "") {
		return fmt.Errorf("host/site labels apply only to QEMU; Azure locality comes from IMDS")
	}
	if o.Size < 1 || o.Size > 32 {
		return fmt.Errorf("--size must be between 1 and 32")
	}
	if o.Provider == "azure" {
		in, err := (azureconfig.Inputs{Region: o.AzureLocation, SKU: o.AzureVMSize}).Defaults()
		if err != nil {
			return err
		}
		o.AzureLocation, o.AzureVMSize = in.Region, in.SKU
	} else if o.Host == "" {
		// Host identity must be shared by separate creates on the same machine.
		// Never publish the workstation's raw machine-id in node profiles.
		raw, err := os.ReadFile("/etc/machine-id")
		if err != nil || len(strings.TrimSpace(string(raw))) == 0 {
			return fmt.Errorf("cannot determine QEMU host identity; supply --host with a stable unique host label")
		}
		id := sha256.Sum256([]byte("infestation-host:" + strings.TrimSpace(string(raw))))
		o.Host = fmt.Sprintf("host-%x", id[:16])
	}
	dir, err := directory(o.Options)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("provider state already exists; inspect existing resources and preserve their disks before a fresh create")
	} else if !os.IsNotExist(err) {
		return err
	}
	o.Domain, err = resolveDomain(ctx, o.Domain, executil.OSRunner{})
	if err != nil {
		return err
	}
	c := clusterConfig(o)
	if err := c.Validate(); err != nil {
		return err
	}
	fmt.Fprintln(out, "Tailnet domain:", o.Domain)
	base := o.BaseImage
	if o.Provider == "qemu" {
		if err := validateHost(o.Accel); err != nil {
			return err
		}
		if o.MemoryMiB < 2048 || o.MemoryMiB > 32768 {
			return fmt.Errorf("cluster VMs require 2048–32768 MiB each")
		}
		if o.Network != "" && o.Network != "slirp" {
			return fmt.Errorf("cluster VMs currently require slirp")
		}
		if base == "" {
			base, err = image.Prepare(ctx, filepath.Join(filepath.Dir(dir), "images"), FlatcarVersion)
			if err != nil {
				return err
			}
		}
	}
	if err := prepareCluster(ctx, o, dir, c, out); err != nil {
		return err
	}
	if o.Provider == "azure" {
		if err := createAzureNodes(ctx, o, dir, c.Nodes, out, executil.OSRunner{}); err != nil {
			return err
		}
	} else {
		for _, n := range c.Nodes {
			raw, err := os.ReadFile(filepath.Join(dir, "output", n.Name+".ign"))
			if err != nil {
				return err
			}
			raw, err = addClusterChannels(raw)
			if err != nil {
				return err
			}
			if err := Create(ctx, Options{Name: n.Name, StateDir: dir, BaseImage: base, Accel: o.Accel, Network: "slirp", MemoryMiB: o.MemoryMiB, profile: raw}, out); err != nil {
				return err
			}
		}
	}
	fmt.Fprintln(out, "Provider state:", dir)
	if o.Init && o.Size < 3 {
		fmt.Fprintln(out, "Initializer is waiting for two database peers from any provider")
		return nil
	}
	if o.Wait > 0 {
		return WaitCluster(ctx, o.Options, out)
	}
	return nil
}

func groupProvider(dir string) (string, error) {
	var provider string
	if err := fsutil.ReadJSON(filepath.Join(dir, "provider.json"), &provider); err == nil {
		if provider != "qemu" && provider != "azure" {
			return "", fmt.Errorf("invalid provider ownership")
		}
		return provider, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	// Old mixed state remains inspectable, but cannot trigger cross-provider actions.
	if _, mixed := loadAzureNode(dir); mixed {
		return "", fmt.Errorf("mixed provider state is read-only; use individual VM controls")
	}
	return "qemu", nil
}

func prepareCluster(ctx context.Context, o ClusterOptions, dir string, c cluster.Config, out io.Writer) error {
	key, err := authKey(o.Options)
	if err != nil {
		return err
	}
	b := ignition.Bootstrap{HelperBuilderImage: o.HelperBuilderImage, TailscaleAuthKey: key}
	if o.Provider == "azure" {
		b.ScaleSet = &nodebootstrap.ScaleSet{Name: o.Name, Group: o.Name + "-rg", Location: o.AzureLocation, Initialize: o.Init}
	}
	b.HelperSourceCommit, err = ignition.ResolveHelperSource(ctx, o.HelperSourceRef)
	if err != nil {
		return err
	}
	identity := []byte(ignition.HelperBuildIdentity(b.HelperSourceCommit, b.HelperBuilderImage))
	fmt.Fprintln(out, "Guest helper source commit:", b.HelperSourceCommit)
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("reserve cluster state: %w", err)
	}
	if o.Init {
		if err := pki.Create(c, dir); err != nil {
			return err
		}
		shared, err := pki.PoolCertificate(dir, c.Tailscale.Domain)
		if err != nil {
			return err
		}
		for name, data := range shared {
			if err := fsutil.WriteFile(filepath.Join(dir, "pki", c.Nodes[0].Name, name), data, 0600); err != nil {
				return err
			}
		}
		token, err := ignition.Token(c, dir)
		if err != nil {
			return err
		}
		b.JoinBundle, err = nodebootstrap.NewJoinBundle(c, c.Nodes[0], dir, token)
		if err != nil {
			return err
		}
		b.Credential, err = pki.BootstrapCredential(dir)
		if err != nil {
			return err
		}
	}
	id := sha256.Sum256(identity)
	b.DeploymentID = fmt.Sprintf("%x", id)
	if err := ignition.Generate(c, dir, b); err != nil {
		return err
	}
	if err := fsutil.WriteJSON(filepath.Join(dir, "nodes.json"), c); err != nil {
		return err
	}
	return fsutil.WriteJSON(filepath.Join(dir, "provider.json"), o.Provider)
}

// inventoryPath keeps existing provider state readable without rewriting it.
// An invalid or inaccessible nodes.json must never fall back to older data.
func inventoryPath(dir string) string {
	current := filepath.Join(dir, "nodes.json")
	if _, err := os.Stat(current); !os.IsNotExist(err) {
		return current
	}
	previous := filepath.Join(dir, "cluster.json")
	if _, err := os.Stat(previous); err == nil {
		return previous
	}
	return current
}

func loadCluster(o Options) (string, cluster.Config, error) {
	dir, err := directory(o)
	if err != nil {
		return "", cluster.Config{}, err
	}
	c, err := cluster.Load(inventoryPath(dir))
	if err != nil {
		return "", c, err
	}
	if c.Tailscale == nil || len(c.Nodes) == 0 {
		return "", c, fmt.Errorf("expected an owned tailnet provider inventory")
	}
	for i, n := range c.Nodes {
		if n.Name != fmt.Sprintf("%s-%d", o.Name, i) {
			return "", c, fmt.Errorf("cluster VM identity mismatch")
		}
	}
	return dir, c, nil
}

func ClusterAction(ctx context.Context, o Options, action string, out io.Writer) error {
	dir, c, err := loadCluster(o)
	if err != nil {
		return err
	}
	provider, err := groupProvider(dir)
	if err != nil {
		return err
	}
	if provider != "qemu" {
		return fmt.Errorf("use azure %s for Azure-owned nodes", action)
	}
	for _, n := range c.Nodes {
		node := Options{Name: n.Name, StateDir: dir, Accel: o.Accel}
		switch action {
		case "stop":
			err = Stop(ctx, node, out)
		case "start":
			err = Start(ctx, node, out)
		case "status":
			_, err = qmp(ctx, filepath.Join(dir, n.Name), "query-status")
			if err != nil {
				fmt.Fprintln(out, n.Name, "stopped or monitor unavailable")
				continue
			}
			report, e := latestObject(filepath.Join(dir, n.Name, "diagnostics.json"))
			if e != nil {
				fmt.Fprintln(out, n.Name, "running; diagnostics pending")
				continue
			}
			raw, _ := json.MarshalIndent(report, "", "  ")
			fmt.Fprintln(out, string(raw))
		default:
			return fmt.Errorf("unknown cluster action")
		}
		if err != nil {
			return fmt.Errorf("%s: %w", n.Name, err)
		}
	}
	if action == "start" && o.Wait > 0 {
		return WaitCluster(ctx, o, out)
	}
	return nil
}

func WaitCluster(ctx context.Context, o Options, out io.Writer) error {
	dir, c, err := loadCluster(o)
	if err != nil {
		return err
	}
	deadline := o.Wait
	if deadline <= 0 {
		deadline = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	kube := filepath.Join(dir, "kubeconfig")
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	phase, last := "", ""
	for {
		encoded, fetchErr := groupKubeconfig(ctx, dir, c)
		found := fetchErr == nil
		if found {
			if err := fsutil.WriteFile(kube, encoded, 0600); err != nil {
				return err
			}
		}
		phase = "Waiting for independent node enrollment, Cockroach bootstrap and an API kubeconfig"
		if found {
			phase = "Kubeconfig received; waiting for this group and at least three Ready tailnet nodes"
			if clusterAPIsReady(ctx, c, kube) {
				fmt.Fprintln(out, "Provider API endpoints and Ready tailnet nodes verified over Tailscale")
				fmt.Fprintln(out, "Kubeconfig:", kube)
				return nil
			}
		}
		if phase != last {
			fmt.Fprintln(out, phase)
			last = phase
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("cluster is not ready: %w; preserve state and use the provider status/wait commands", ctx.Err())
		case <-ticker.C:
		}
	}
}

func clusterAPIsReady(ctx context.Context, c cluster.Config, kube string) bool {
	run := func(args ...string) executil.Result {
		child, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		return executil.OSRunner{}.Run(child, "kubectl", append([]string{"--kubeconfig", kube, "--request-timeout=8s"}, args...)...)
	}
	for _, n := range c.Servers() {
		if run("--server=https://"+c.NodeAddress(n)+":6443", "get", "--raw=/readyz").Code != 0 {
			return false
		}
	}
	if run("get", "--raw=/readyz").Code != 0 {
		return false
	}
	result := run("get", "nodes", "-o", "json")
	if result.Code != 0 {
		return false
	}
	var nodes struct {
		Items []struct {
			Metadata struct{ Name string }
			Status   struct {
				Conditions []struct{ Type, Status string }
				Addresses  []struct{ Type, Address string }
			}
		}
	}
	if json.Unmarshal([]byte(result.Stdout), &nodes) != nil || len(nodes.Items) < 3 {
		return false
	}
	expected := map[string]bool{}
	for _, n := range c.Nodes {
		expected[n.Name] = true
	}
	for _, n := range nodes.Items {
		delete(expected, n.Metadata.Name)
		ready, tailIP := false, false
		for _, condition := range n.Status.Conditions {
			if condition.Type == "Ready" && condition.Status == "True" {
				ready = true
			}
		}
		for _, address := range n.Status.Addresses {
			addr, err := netip.ParseAddr(address.Address)
			if address.Type == "InternalIP" && err == nil && netip.MustParsePrefix("100.64.0.0/10").Contains(addr) {
				tailIP = true
			}
		}
		if !ready || !tailIP {
			return false
		}
	}
	return len(expected) == 0
}
