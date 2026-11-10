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
	"infestation/internal/pki"
)

type ClusterOptions struct {
	Options
	Domain, NodeHelper                  string
	HelperSourceRef, HelperBuilderImage string
	AzureLocation, AzureVMSize          string
}

func clusterConfig(o ClusterOptions) cluster.Config {
	c := cluster.Config{CockroachImage: "cockroachdb/cockroach:v26.2.7", K3sExtensionVersion: "v1.35.8+k3s1", Architecture: "x86-64", CockroachStorage: "persistent", CockroachCache: "256MiB", CockroachSQLMemory: "256MiB", Tailscale: &cluster.Tailscale{Version: TailscaleVersion, Domain: o.Domain}}
	for i := 0; i < 3; i++ {
		c.Nodes = append(c.Nodes, cluster.Node{Name: fmt.Sprintf("%s-%d", o.Name, i), IP: fmt.Sprintf("192.0.2.%d", i+1), Locality: fmt.Sprintf("region=lab,zone=node%d", i)})
	}
	if o.AzureLocation != "" {
		c.Tailscale.SSH = true
		c.APITailnetHostname = "k8s-api." + o.Domain
		c.Nodes[0].Locality = "region=local,zone=vm0"
		c.Nodes[1].Locality = "region=local,zone=vm1"
		c.Nodes[2].Locality = "region=azure,zone=" + o.AzureLocation
	}
	return c
}

// CreateCluster renders all credentials once, then boots three distinct QEMU
// user NAT backends. No network, TAP, bridge, hostfwd or underlay peer IP is shared.
func CreateCluster(ctx context.Context, o ClusterOptions, out io.Writer) error {
	if o.AzureLocation != "" {
		in, err := (azureconfig.Inputs{Region: o.AzureLocation, SKU: o.AzureVMSize}).Defaults()
		if err != nil {
			return err
		}
		o.AzureLocation, o.AzureVMSize = in.Region, in.SKU
	}
	dir, err := directory(o.Options)
	if err != nil {
		return err
	}
	c := clusterConfig(o)
	if err = c.Validate(); err != nil {
		return err
	}
	if o.Network != "" && o.Network != "slirp" {
		return fmt.Errorf("the isolated cluster test currently uses --network slirp")
	}
	if err = validateHost(o.Accel); err != nil {
		return err
	}
	if o.AzureLocation != "" && o.HelperSourceRef == "" {
		return fmt.Errorf("mixed test requires --helper-source-ref for cloud-independent guest builds")
	}
	key, err := authKey(o.Options)
	if err != nil {
		return err
	}
	var helper []byte
	sourceCommit := ""
	if o.HelperSourceRef != "" {
		sourceCommit, err = ignition.ResolveHelperSource(ctx, o.HelperSourceRef)
		if err != nil {
			return err
		}
		helper = []byte(ignition.HelperBuildIdentity(sourceCommit, o.HelperBuilderImage))
		fmt.Fprintln(out, "Guest helper source commit:", sourceCommit)
	} else {
		helper, err = os.ReadFile(o.NodeHelper)
		if err != nil {
			return fmt.Errorf("build the Linux node helper first: %w", err)
		}
	}
	if _, err = os.Lstat(dir); err == nil {
		return fmt.Errorf("cluster state already exists; use vm cluster wait/status/start/stop")
	} else if !os.IsNotExist(err) {
		return err
	}
	if o.MemoryMiB < 2048 || o.MemoryMiB > 32768 {
		return fmt.Errorf("cluster VMs require 2048–32768 MiB each")
	}
	base := o.BaseImage
	if base == "" {
		fmt.Fprintln(out, "Downloading and verifying Flatcar image…")
		base, err = image.Prepare(ctx, filepath.Join(filepath.Dir(dir), "images"), FlatcarVersion)
		if err != nil {
			return err
		}
	}
	for _, n := range c.Nodes {
		if _, err = directory(Options{Name: n.Name, StateDir: dir}); err != nil {
			return err
		}
	}
	if err = os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return err
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("reserve new cluster state: %w", err)
	}
	if err = pki.Create(c, dir); err != nil {
		return err
	}
	b := ignition.Bootstrap{HelperSourceCommit: sourceCommit, HelperBuilderImage: o.HelperBuilderImage, TailscaleAuthKey: key}
	if sourceCommit == "" {
		artifact, err := startArtifacts(ctx, dir, o.NodeHelper)
		if err != nil {
			return err
		}
		b.LocalHelper = o.NodeHelper
		b.LocalHelperURL = fmt.Sprintf("http://10.0.2.2:%d/infestation-node", artifact.Port)
		b.LocalHelperSHA512 = artifact.SHA512
	}
	// Issue the one-hour SQL credential after image acquisition, immediately
	// before first boot. Existing PKI/token/state are never regenerated on resume.
	credential, err := pki.BootstrapCredential(dir)
	if err != nil {
		return err
	}
	id := sha256.Sum256(helper)
	b.Credential = credential
	b.DeploymentID = fmt.Sprintf("%x", id)
	if err = ignition.Generate(c, dir, b); err != nil {
		return err
	}
	if err = fsutil.WriteJSON(filepath.Join(dir, "cluster.json"), c); err != nil {
		return err
	}
	if o.AzureLocation != "" {
		if err := createAzureNode(ctx, o, dir, c.Nodes[2].Name, out); err != nil {
			return err
		}
	}
	for i, n := range c.Nodes {
		if o.AzureLocation != "" && i == 2 {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, "output", n.Name+".ign"))
		if err != nil {
			return err
		}
		doc, err := addClusterChannels(raw)
		if err != nil {
			return err
		}
		if err = Create(ctx, Options{Name: n.Name, StateDir: dir, BaseImage: base, Accel: o.Accel, Network: "slirp", MemoryMiB: o.MemoryMiB, profile: doc}, out); err != nil {
			return err
		}
	}
	fmt.Fprintln(out, "Cluster state:", dir)
	if o.Wait == 0 {
		return nil
	}
	return WaitCluster(ctx, o.Options, out)
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
	if c.Tailscale == nil || len(c.Nodes) != 3 || len(c.Servers()) != 3 {
		return "", c, fmt.Errorf("expected an owned three-server tailnet cluster inventory")
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
	if action == "start" {
		if _, e := os.Stat(filepath.Join(dir, "artifacts", "infestation-node")); e == nil {
			if _, e = startArtifacts(ctx, dir, ""); e != nil {
				return e
			}
		}
	}
	azure, mixed := loadAzureNode(dir)
	if mixed && (action == "start" || action == "stop") {
		if err := azureAction(ctx, azure, action, out); err != nil {
			return err
		}
	}
	for _, n := range c.Nodes {
		if mixed && n.Name == azure.Node {
			if action == "status" {
				fmt.Fprintln(out, n.Name, "Azure VM", azure.Group)
			}
			continue
		}
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
			if e != nil {
				continue
			}
			data, _ := json.Marshal(raw)
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
		phase = "Waiting for node enrollment, Cockroach bootstrap and the first API kubeconfig"
		if found {
			phase = "Kubeconfig received; waiting for all three API endpoints and Ready tailnet nodes"
			if clusterAPIsReady(ctx, c, kube) {
				stopArtifacts(dir)
				fmt.Fprintln(out, "Three API endpoints and three Ready nodes verified over Tailscale")
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
	if json.Unmarshal([]byte(result.Stdout), &nodes) != nil || len(nodes.Items) != 3 {
		return false
	}
	expected := map[string]bool{}
	for _, n := range c.Nodes {
		expected[n.Name] = true
	}
	for _, n := range nodes.Items {
		if !expected[n.Metadata.Name] {
			return false
		}
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
	return true
}
