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
	"time"

	"infestation/internal/azureconfig"
	"infestation/internal/bootstrap"
	"infestation/internal/cluster"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
	"infestation/internal/ignition"
	"infestation/internal/image"
	"infestation/internal/nodebootstrap"
	"infestation/internal/pki"
)

type ClusterOptions struct {
	Options
	Domain, NodeHelper                  string
	HelperSourceRef, HelperBuilderImage string
	AzureLocation, AzureVMSize          string
	Provider                            string
	Size                                int
	Init                                bool
}

func clusterConfig(o ClusterOptions) cluster.Config {
	c := cluster.Config{DiscoverPeers: true, CockroachImage: "cockroachdb/cockroach:v26.2.7", K3sExtensionVersion: "v1.35.8+k3s1", Architecture: "x86-64", CockroachStorage: "persistent", CockroachCache: "256MiB", CockroachSQLMemory: "256MiB", Tailscale: &cluster.Tailscale{Version: TailscaleVersion, Domain: o.Domain}}
	for i := 0; i < o.Size; i++ {
		c.Nodes = append(c.Nodes, cluster.Node{Init: o.Init && i == 0, Name: fmt.Sprintf("%s-%d", o.Name, i), IP: fmt.Sprintf("192.0.2.%d", i+1), Locality: fmt.Sprintf("region=lab,zone=node%d", i)})
	}
	c.Tailscale.SSH = true
	c.APITailnetHostname = "k8s-api." + o.Domain
	return c
}

// CreateCluster creates independent node profiles and compute on one provider.
// Only --init creates CA/token/bootstrap credentials. All other nodes enroll
// through the tailnet after boot and never need the initializer's workstation.
func CreateCluster(ctx context.Context, o ClusterOptions, out io.Writer) error {
	if o.Provider == "" {
		o.Provider = "qemu"
	}
	if o.Provider != "qemu" && o.Provider != "azure" {
		return fmt.Errorf("unknown compute provider")
	}
	if o.Size < 1 || o.Size > 32 {
		return fmt.Errorf("--size must be between 1 and 32")
	}
	if o.Provider == "azure" {
		if o.Size != 1 {
			return fmt.Errorf("Azure enrollment batches currently support --size 1")
		}
		in, err := (azureconfig.Inputs{Region: o.AzureLocation, SKU: o.AzureVMSize}).Defaults()
		if err != nil {
			return err
		}
		o.AzureLocation, o.AzureVMSize = in.Region, in.SKU
		if o.HelperSourceRef == "" {
			return fmt.Errorf("Azure requires --helper-source-ref for guest builds")
		}
	}
	dir, err := directory(o.Options)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("provider state already exists; use this provider's resume command")
	} else if !os.IsNotExist(err) {
		return err
	}
	c := clusterConfig(o)
	if err := c.Validate(); err != nil {
		return err
	}
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
	for _, n := range c.Nodes {
		if o.Provider == "azure" {
			if err := createAzureNode(ctx, o, dir, n.Name, out); err != nil {
				return err
			}
		} else {
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
	var identity []byte
	if o.HelperSourceRef != "" {
		b.HelperSourceCommit, err = ignition.ResolveHelperSource(ctx, o.HelperSourceRef)
		if err != nil {
			return err
		}
		identity = []byte(ignition.HelperBuildIdentity(b.HelperSourceCommit, b.HelperBuilderImage))
		fmt.Fprintln(out, "Guest helper source commit:", b.HelperSourceCommit)
	} else {
		identity, err = os.ReadFile(o.NodeHelper)
		if err != nil {
			return fmt.Errorf("build the Linux node helper first: %w", err)
		}
	}
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
	if b.HelperSourceCommit == "" {
		artifact, err := startArtifacts(ctx, dir, o.NodeHelper)
		if err != nil {
			return err
		}
		b.LocalHelper = o.NodeHelper
		b.LocalHelperURL = fmt.Sprintf("http://10.0.2.2:%d/infestation-node", artifact.Port)
		b.LocalHelperSHA512 = artifact.SHA512
	}
	id := sha256.Sum256(identity)
	b.DeploymentID = fmt.Sprintf("%x", id)
	if err := ignition.Generate(c, dir, b); err != nil {
		return err
	}
	if err := fsutil.WriteJSON(filepath.Join(dir, "cluster.json"), c); err != nil {
		return err
	}
	return fsutil.WriteJSON(filepath.Join(dir, "provider.json"), o.Provider)
}

func loadCluster(o Options) (string, cluster.Config, error) {
	dir, err := directory(o)
	if err != nil {
		return "", cluster.Config{}, err
	}
	c, err := cluster.Load(filepath.Join(dir, "cluster.json"))
	if err != nil {
		return "", c, err
	}
	if c.Tailscale == nil || len(c.Nodes) == 0 || len(c.Servers()) != len(c.Nodes) {
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
	if action == "start" {
		if _, e := os.Stat(filepath.Join(dir, "artifacts", "infestation-node")); e == nil {
			if _, e = startArtifacts(ctx, dir, ""); e != nil {
				return e
			}
		}
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
	if action == "stop" {
		stopArtifacts(dir)
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
		found := false
		for _, n := range c.Servers() {
			raw, e := latestObject(filepath.Join(dir, n.Name, "access.json"))
			var data []byte
			if e == nil {
				data, _ = json.Marshal(raw)
			} else if c.DiscoverPeers {
				data, e = nodebootstrap.FetchKubeconfig(ctx, c.NodeAddress(n))
				if e != nil {
					continue
				}
			} else {
				continue
			}
			endpoint := "https://" + c.NodeAddress(n) + ":6443"
			if c.APITailnetHostname != "" {
				endpoint = "https://" + c.APITailnetHostname + ":443"
			}
			conf, e := bootstrap.PublicKubeconfig(data, endpoint)
			if e != nil {
				continue
			}
			if c.APITailnetHostname != "" {
				conf.Clusters[0].Cluster["tls-server-name"] = apiTLSName
			}
			encoded, _ := json.Marshal(conf)
			if e = fsutil.WriteFile(kube, encoded, 0600); e != nil {
				return e
			}
			found = true
			break
		}
		phase = "Waiting for independent node enrollment, Cockroach bootstrap and an API kubeconfig"
		if found {
			phase = "Kubeconfig received; waiting for this group and at least three Ready tailnet nodes"
			if clusterAPIsReady(ctx, c, kube) {
				stopArtifacts(dir)
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
			return fmt.Errorf("cluster is not ready: %w; preserve state and use vm cluster status/wait", ctx.Err())
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

// ResumeLocal launches only reserved QEMU nodes, using the original profiles.
func ResumeLocal(ctx context.Context, o ClusterOptions, out io.Writer) error {
	dir, c, err := loadCluster(o.Options)
	if err != nil {
		return err
	}
	provider, err := groupProvider(dir)
	if err != nil {
		return err
	}
	if provider != "qemu" {
		return fmt.Errorf("use azure resume for Azure-owned nodes")
	}
	if _, err := os.Stat(filepath.Join(dir, "artifacts", "infestation-node")); err == nil {
		if _, err := startArtifacts(ctx, dir, ""); err != nil {
			return err
		}
	}
	base := o.BaseImage
	if base == "" {
		base = filepath.Join(filepath.Dir(dir), "images", "flatcar-"+FlatcarVersion+".qcow2")
	}
	for _, n := range c.Nodes {
		if _, err := os.Stat(filepath.Join(dir, n.Name, "machine.json")); err == nil {
			continue
		}
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
	if o.Wait > 0 {
		return WaitCluster(ctx, o.Options, out)
	}
	return nil
}
