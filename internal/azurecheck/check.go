// Package azurecheck runs a disposable Azure provision/smoke/teardown cycle.
package azurecheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"infestation/internal/azureplan"
	"infestation/internal/cluster"
	"infestation/internal/deploy"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
	"infestation/internal/smoke"
)

const ownershipTag = "infestation-check-id"
const gatewayCRDs = "https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.6.3/standard-install.yaml"

func Run(ctx context.Context, o Options) (err error) {
	o, err = o.withDefaults()
	if err != nil {
		return err
	}
	if err := prerequisites(); err != nil {
		return err
	}
	if o.Log == nil {
		o.Log = os.Stdout
	}
	// Only structured command output is captured; do not print Azure auth or tags.
	quiet := executil.OSRunner{}
	id := uuid.NewString()
	name := "infcheck-" + id[:12]
	state := filepath.Join(o.StateDir, name)
	if err := os.MkdirAll(o.StateDir, 0700); err != nil {
		return err
	}
	if err := os.Mkdir(state, 0700); err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(state, "check.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			fmt.Fprintln(logFile, "FAILED:", err)
		}
		logFile.Close()
	}()
	o.Log = io.MultiWriter(o.Log, logFile)
	runner := executil.OSRunner{Output: o.Log}
	fmt.Fprintln(o.Log, "Azure check state:", state)
	fmt.Fprintf(o.Log, "Versions: Flatcar %s, CockroachDB %s, K3s %s\n", o.FlatcarVersion, o.CockroachVersion, o.K3sVersion)
	c, err := cluster.Load(filepath.Join(o.RepoRoot, "examples", "cluster.json"))
	if err != nil {
		return err
	}
	c.CockroachImage = "cockroachdb/cockroach:" + o.CockroachVersion
	c.K3sExtensionVersion = o.K3sVersion
	c.SingleNodeLab = false
	c.SmokeTest = true
	c.Nodes = []cluster.Node{{Name: "api-0", IP: "192.168.1.101"}, {Name: "api-1", IP: "192.168.1.102"}, {Name: "worker-0", IP: "192.168.1.103", Role: "worker"}}
	input := filepath.Join(state, "cluster-input.json")
	if err := fsutil.WriteJSON(input, c); err != nil {
		return err
	}
	planDir := filepath.Join(state, "plan")
	plan, err := azureplan.Create(ctx, quiet, azureplan.Options{
		Name: name, Location: o.Location, ClusterPath: input, OutputDir: planDir, SKUsFile: o.SKUsFile, RatesFile: o.RatesFile, GatewayEnabled: true,
		Constraints: azureplan.Constraints{MinVCPUs: 2, MinMemoryGiB: 8, MinUsableDiskGiB: 64, IncludeBurstable: true},
	})
	if err != nil {
		return err
	}
	plan.PulumiConfig["checkID"] = id
	if err := fsutil.WriteJSON(filepath.Join(planDir, "plan.json"), plan); err != nil {
		return err
	}
	group := name + "-rg"
	exists, err := groupExists(ctx, quiet, plan.SubscriptionID, group)
	if err != nil {
		return err
	}
	if exists {
		return fmt.Errorf("refusing existing resource group %s", group)
	}
	return cycle(ctx, func(ctx context.Context) error {
		fmt.Fprintln(o.Log, "Provision:", group, "(two API nodes, one worker; all three host CockroachDB)")
		return deploy.Azure(ctx, deploy.Options{RepoRoot: o.RepoRoot, PlanDir: planDir, Stack: "check" + strings.ReplaceAll(id[:12], "-", ""), FlatcarVersion: o.FlatcarVersion, Yes: true, Log: o.Log})
	}, func(ctx context.Context) error {
		fmt.Fprintln(o.Log, "Smoke: internal DNS/Services and public LB/Cilium Gateway")
		kube := filepath.Join(planDir, "kubeconfig")
		if err := enableGateway(ctx, runner, o.RepoRoot, kube); err != nil {
			return err
		}
		c, err := cluster.Load(filepath.Join(planDir, "cluster.json"))
		if err != nil {
			return err
		}
		return smoke.Run(ctx, runner, c, planDir, kube, smoke.Options{PublicURL: "http://" + name + "-api." + o.Location + ".cloudapp.azure.com", Log: o.Log})
	}, func(ctx context.Context) error {
		fmt.Fprintln(o.Log, "Teardown:", group)
		if err := cleanup(ctx, quiet, plan.SubscriptionID, group, id); err != nil {
			return fmt.Errorf("teardown incomplete for %s: %w", group, err)
		}
		fmt.Fprintln(o.Log, "Teardown verified:", group, "is absent. Local state/evidence retained:", state)
		return nil
	})
}

// Check before creating state or querying Azure: missing local tools cannot
// create a cluster and should not produce provision/teardown progress messages.
func prerequisites() error {
	var missing []string
	for _, tool := range []string{"az", "pulumi", "go", "kubectl"} {
		if _, err := exec.LookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("azure check: missing required tools on PATH: %s; install the prerequisites listed in infra/azure/README.md (Pulumi CLI 3.272.0)", strings.Join(missing, ", "))
	}
	return nil
}

// Teardown runs even after failed provisioning, failed smoke, or cancellation.
// Its independent deadline allows Ctrl-C to stop tests without skipping cleanup.
func cycle(ctx context.Context, provision, test, teardown func(context.Context) error) (err error) {
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Minute)
		defer cancel()
		err = errors.Join(err, teardown(cleanupCtx))
	}()
	if err = provision(ctx); err != nil {
		return err
	}
	return test(ctx)
}

func enableGateway(ctx context.Context, runner executil.Runner, repo, kube string) error {
	run := func(args ...string) error {
		return runner.Run(ctx, "kubectl", append([]string{"--kubeconfig", kube}, args...)...).Err("enable Cilium Gateway")
	}
	commands := [][]string{
		{"apply", "--server-side", "-f", gatewayCRDs},
		{"apply", "-f", filepath.Join(repo, "examples", "cilium-gateway.yaml")},
		{"-n", "kube-system", "wait", "--for=jsonpath={.data.enable-gateway-api}=true", "configmap/cilium-config", "--timeout=300s"},
		{"-n", "kube-system", "wait", "--for=condition=complete", "job/helm-install-cilium", "--timeout=300s"},
		{"-n", "kube-system", "rollout", "restart", "daemonset/cilium", "deployment/cilium-operator"},
		{"-n", "kube-system", "rollout", "status", "daemonset/cilium", "--timeout=300s"},
		{"-n", "kube-system", "rollout", "status", "deployment/cilium-operator", "--timeout=300s"},
		{"-n", "kube-system", "rollout", "status", "daemonset/cilium-envoy", "--timeout=300s"},
	}
	for _, args := range commands {
		if err := run(args...); err != nil {
			return err
		}
	}
	return nil
}

func groupExists(ctx context.Context, r executil.Runner, subscription, group string) (bool, error) {
	result := r.Run(ctx, "az", "group", "exists", "--name", group, "--subscription", subscription, "--output", "json")
	if err := result.Err("check resource-group existence"); err != nil {
		return false, err
	}
	var exists *bool
	if err := json.Unmarshal([]byte(result.Stdout), &exists); err != nil || exists == nil {
		return false, fmt.Errorf("invalid resource-group existence response")
	}
	return *exists, nil
}

func cleanup(ctx context.Context, r executil.Runner, subscription, group, id string) error {
	exists, err := groupExists(ctx, r, subscription, group)
	if err != nil || !exists {
		return err
	}
	result := r.Run(ctx, "az", "group", "show", "--name", group, "--subscription", subscription, "--query", "tags", "--output", "json")
	if err := result.Err("verify check ownership"); err != nil {
		return err
	}
	var tags map[string]string
	if json.Unmarshal([]byte(result.Stdout), &tags) != nil || id == "" || tags[ownershipTag] != id {
		return fmt.Errorf("refusing to delete resource group without this check's ownership tag")
	}
	result = r.Run(ctx, "az", "lock", "list", "--resource-group", group, "--subscription", subscription, "--output", "json")
	if err := result.Err("list check deletion locks"); err != nil {
		return err
	}
	var locks []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(result.Stdout), &locks); err != nil {
		return err
	}
	prefix := "/subscriptions/" + subscription + "/resourceGroups/" + group + "/"
	for _, lock := range locks {
		if !strings.HasPrefix(strings.ToLower(lock.ID), strings.ToLower(prefix)) {
			return fmt.Errorf("lock is outside this check's resource group")
		}
		if err := r.Run(ctx, "az", "lock", "delete", "--ids", lock.ID, "--subscription", subscription).Err("remove check deletion lock"); err != nil {
			return err
		}
	}
	if err := r.Run(ctx, "az", "group", "delete", "--name", group, "--subscription", subscription, "--yes", "--no-wait").Err("delete check resource group"); err != nil {
		return err
	}
	for {
		exists, err := groupExists(ctx, r, subscription, group)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		timer := time.NewTimer(5 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
