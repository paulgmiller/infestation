package deploy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"infestation/internal/azureplan"
	"infestation/internal/blobstore"
	"infestation/internal/bootstrap"
	"infestation/internal/cluster"
	"infestation/internal/executil"
	"infestation/internal/ignition"
	"infestation/internal/kubeconfigstore"
	"infestation/internal/nodebootstrap"
	"infestation/internal/pki"
)

type nodeAssets struct {
	Profile  ignition.Bootstrap
	Binary   []byte
	Location blobstore.Location
}

func BuildNode(ctx context.Context, repo, destination, architecture string) error {
	arch := "amd64"
	if architecture == "aarch64" {
		arch = "arm64"
	}
	env := setEnv(os.Environ(), "GOOS", "linux")
	env = setEnv(env, "GOARCH", arch)
	env = setEnv(env, "CGO_ENABLED", "0")
	r := (executil.OSRunner{Dir: repo, Env: env}).Run(ctx, "go", "build", "-trimpath", "-o", destination, "./cmd/infestation-node")
	return r.Err("build node-local bootstrap helper")
}

func prepareNode(ctx context.Context, repo, state, binary string, plan azureplan.Plan, c cluster.Config) (nodeAssets, error) {
	var a nodeAssets
	if _, err := c.Initializer(); err != nil {
		return a, err
	}
	if binary == "" {
		binary = filepath.Join(state, "infestation-node")
		if err := BuildNode(ctx, repo, binary, c.Architecture); err != nil {
			return a, err
		}
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		return a, err
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	name, ok := plan.PulumiConfig["name"].(string)
	if !ok || name == "" {
		return a, fmt.Errorf("plan is missing its resource name")
	}
	location, _ := plan.PulumiConfig["location"].(string)
	encoded, _ := json.Marshal(c)
	identity := sha256.Sum256(append(encoded, digest[:]...))
	account := blobstore.AccountName(plan.SubscriptionID, name+"-rg")
	credential, err := pki.BootstrapCredential(state)
	if err != nil {
		return a, err
	}
	a.Profile = ignition.Bootstrap{Account: account, HelperSHA256: hash, DeploymentID: hex.EncodeToString(identity[:]), APIEndpoint: "https://" + name + "-api." + location + ".cloudapp.azure.com:6443", Credential: credential}
	if uniform, _ := plan.PulumiConfig["uniform"].(bool); uniform {
		zone, _ := plan.PulumiConfig["privateDNSZone"].(string)
		workerDB := true
		workers := 0
		for _, n := range c.Nodes {
			if n.Worker() {
				workerDB = n.Database()
				workers++
			}
		}
		a.Profile.Pool = &nodebootstrap.AzurePool{Name: name, Role: "api", Zone: zone, Location: location, Subnet: "192.168.1.0/24", APICount: 3, WorkerCount: workers, WorkerDatabase: workerDB}
	}
	a.Binary = data
	a.Location = blobstore.Location{Account: account, Container: "bootstrap-assets", Blob: "infestation-node-" + hash, SubscriptionID: plan.SubscriptionID}
	return a, nil
}

func finishNodeBootstrap(ctx context.Context, assets nodeAssets, outputs bootstrap.Outputs, state string, log io.Writer) error {
	client := blobstore.Client{MaxSize: 32 * 1024 * 1024}
	wait, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	fmt.Fprintln(log, "Publishing node helper; waiting for Ignition-driven bootstrap (no management connection).")
	for {
		if err := client.Put(wait, assets.Location, assets.Binary); err == nil {
			break
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("node helper publication failed: %w", wait.Err())
		case <-time.After(5 * time.Second):
		}
	}
	statusLocation := blobstore.Location{Account: assets.Location.Account, Container: "access", Blob: "bootstrap-status.json", SubscriptionID: assets.Location.SubscriptionID}
	for {
		raw, err := client.Get(wait, statusLocation)
		var status struct {
			DeploymentID string `json:"deploymentID"`
			Ready        bool   `json:"ready"`
		}
		if err == nil && json.Unmarshal(raw, &status) == nil && status.Ready && status.DeploymentID == assets.Profile.DeploymentID {
			break
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("node-local bootstrap did not report readiness; inspect boot diagnostics or serial console: %w", wait.Err())
		case <-time.After(5 * time.Second):
		}
	}
	if err := kubeconfigstore.Fetch(ctx, blobstore.Client{}, outputs, filepath.Join(state, "kubeconfig"), true); err != nil {
		return err
	}
	fmt.Fprintln(log, "Cluster ready. Kubeconfig:", filepath.Join(state, "kubeconfig"))
	return nil
}
