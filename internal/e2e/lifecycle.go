// Package e2e runs explicit live provisioning and recovery checks.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"time"

	"github.com/paulgmiller/infestation/internal/cli"
)

type commandRunner func(context.Context, []string, io.Writer) error

// Run provisions exactly three servers and shuts down all attempted groups on
// success, failure or cancellation. Disks and private state remain retained.
func Run(ctx context.Context, o Options, out io.Writer) error {
	return runLifecycle(ctx, o, out, cli.Run)
}

func runLifecycle(ctx context.Context, o Options, out io.Writer, invoke commandRunner) (result error) {
	if err := o.validate(); err != nil {
		return err
	}
	for _, tool := range []string{"kubectl", "tailscale"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%s is required", tool)
		}
	}
	state := o.StateDir
	var err error
	if state == "" {
		state, err = os.MkdirTemp("", "inf-life-")
	} else {
		err = os.Mkdir(state, 0700)
	}
	if err != nil {
		return fmt.Errorf("reserve private E2E state: %w", err)
	}
	fmt.Fprintln(out, "Private ownership state retained at", state)
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	name := o.Name
	if name == "" {
		name = fmt.Sprintf("life-%d", time.Now().Unix())
	}
	seed, peer, joining := name+"s", name+"p", name+"j"
	domain := o.Domain
	command := func(args ...string) error { return invoke(ctx, args, out) }
	var attempted []string
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 8*time.Minute)
		defer done()
		fmt.Fprintln(out, "Shutting down all test VMs; disks and private state retained")
		for _, group := range attempted {
			if _, err := os.Stat(filepath.Join(state, group, "nodes.json")); os.IsNotExist(err) {
				continue
			}
			if err := invoke(cleanup, []string{"vm", "stop", "--name", group, "--state-dir", state, "--allow-outage"}, out); err != nil {
				result = errors.Join(result, fmt.Errorf("shutdown %s failed; inspect retained state: %w", group, err))
			}
		}
		if result == nil {
			fmt.Fprintln(out, "PASS: VM lifecycle and final shutdown")
		} else {
			fmt.Fprintln(out, "FAIL: VM lifecycle or final shutdown; private state retained at", state)
		}
	}()
	create := func(group string, founding bool) error {
		attempted = append(attempted, group)
		args := []string{"vm", "create", "--name", group, "--state-dir", state, "--tailnet-domain", domain, "--accel", o.Accel, "--wait", "0"}
		if founding {
			args = append(args, "--init")
		} else if o.SSH != "" && group == joining {
			args = append(args, "--ssh", o.SSH)
			if o.SSHStateDir != "" {
				args = append(args, "--ssh-state-dir", o.SSHStateDir)
			}
		}
		if o.AuthKeyFile != "" {
			args = append(args, "--tailscale-auth-key-file", o.AuthKeyFile)
		}
		if o.HelperSourceRef != "" {
			args = append(args, "--helper-source-ref", o.HelperSourceRef)
		}
		image := o.BaseImage
		if group == joining && o.SSH != "" {
			image = o.SSHBaseImage
		}
		if image != "" {
			args = append(args, "--base-image", image)
		}
		if err := command(args...); err != nil {
			return fmt.Errorf("create %s: %w; state retained", group, err)
		}
		return nil
	}
	fmt.Fprintln(out, "Creating three independent servers, one with initialization authority")
	if err := create(seed, true); err != nil {
		return err
	}
	if err := create(peer, false); err != nil {
		return err
	}
	fmt.Fprintln(out, "Creating independent joining server (optionally on SSH host)")
	if err := create(joining, false); err != nil {
		return err
	}
	if err := command("vm", "wait", "--name", seed, "--state-dir", state, "--wait", "25m"); err != nil {
		return err
	}
	kube := filepath.Join(state, seed, "kubeconfig")
	certs := filepath.Join(state, seed, "pki", "admin")
	probe := joining + "-0"
	survivors := []string{seed + "-0", peer + "-0"}
	all := append(append([]string{}, survivors...), probe)
	run := func(tool string, args ...string) (string, error) {
		child, done := context.WithTimeout(ctx, 20*time.Second)
		defer done()
		output, err := exec.CommandContext(child, tool, args...).Output()
		// Do not log raw command output, kubeconfigs, or authentication failures.
		if err != nil {
			return "", fmt.Errorf("%s probe failed", tool)
		}
		return string(output), nil
	}
	kubectl := func(args ...string) (string, error) {
		return run("kubectl", append([]string{"--kubeconfig", kube, "--request-timeout=12s"}, args...)...)
	}
	sql := func(host, query string) ([][]string, error) {
		return lifecycleSQL(ctx, certs, host+"."+domain+":26257", query)
	}
	// Write unique test data through the node that will be stopped; access it
	// through survivors while it is off and through the same node after restart.
	if _, err := sql(probe, "CREATE DATABASE lifecycle; CREATE TABLE lifecycle.public.probe (id INT PRIMARY KEY, value STRING); INSERT INTO lifecycle.public.probe VALUES (1, 'before-stop')"); err != nil {
		return err
	}
	// The host-level database and the Kubernetes API/Kine must each serve writes.
	health := func(nodes []string) error {
		for _, node := range nodes {
			if _, err := kubectl("--server=https://"+node+"."+domain+":6443", "get", "--raw=/readyz"); err != nil {
				return err
			}
			result, err := sql(node, "SELECT value FROM lifecycle.public.probe WHERE id=1")
			if err != nil || !reflect.DeepEqual(result, [][]string{{"before-stop"}}) {
				return fmt.Errorf("SQL persisted-data probe failed on %s", node)
			}
			if _, err := sql(node, "UPSERT INTO lifecycle.public.probe VALUES (2, 'serving')"); err != nil {
				return err
			}
		}
		raw, err := kubectl("--server=https://"+nodes[0]+"."+domain+":6443", "get", "nodes", "-o", "json")
		if err != nil {
			return err
		}
		return lifecycleNodesReady([]byte(raw), nodes)
	}
	wait := func(check func() error) error {
		deadline := time.NewTimer(8 * time.Minute)
		defer deadline.Stop()
		for {
			if err := check(); err == nil {
				return nil
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("lifecycle context expired")
			case <-deadline.C:
				return fmt.Errorf("lifecycle health did not converge; inspect retained state")
			case <-time.After(5 * time.Second):
			}
		}
	}
	if err := wait(func() error { return health(all) }); err != nil {
		return err
	}
	// Capture the actual Tailscale identity and Cockroach node/store identities.
	identity := func() ([][]string, error) {
		return sql(probe, "SELECT node_id, store_id FROM crdb_internal.kv_store_status ORDER BY node_id, store_id")
	}
	before, err := identity()
	if err != nil || len(before) != 3 {
		return fmt.Errorf("expected three Cockroach node/store identities")
	}
	var tailBefore map[string]json.RawMessage
	raw, err := run("tailscale", "ssh", "core@"+probe+"."+domain, "tailscale", "status", "--json", "--peers=false")
	if err != nil || json.Unmarshal([]byte(raw), &tailBefore) != nil {
		return fmt.Errorf("cannot capture Tailscale identity")
	}
	selfBefore := tailBefore["Self"]
	// A ConfigMap update exercises K3s's persisted Kine datastore during outage.
	if _, err := kubectl("create", "configmap", "lifecycle-probe", "--from-literal=value=before-stop"); err != nil {
		return err
	}
	// Wait for actual three-voter placement on every range, including system
	// and Kine ranges, before taking one store offline. Ready nodes alone do
	// not establish that replication has converged.
	replicated := func() error {
		rows, err := sql(survivors[0], "SELECT count(*) AS total, count(*) FILTER (WHERE COALESCE(array_length(voting_replicas, 1), 0) = 3 AND COALESCE(array_length(learner_replicas, 1), 0) = 0) AS ready FROM crdb_internal.ranges_no_leases")
		if err != nil {
			return err
		}
		if len(rows) != 1 || len(rows[0]) != 2 {
			return fmt.Errorf("invalid range replication response")
		}
		total, err := strconv.Atoi(rows[0][0])
		if err != nil || total == 0 || rows[0][0] != rows[0][1] {
			return fmt.Errorf("three-voter replication has not converged")
		}
		return nil
	}
	if err := wait(replicated); err != nil {
		return err
	}
	fmt.Fprintln(out, "Stopping only the independent node; retaining the other two servers")
	if err := command("vm", "stop", "--name", joining, "--state-dir", state, "--allow-outage"); err != nil {
		return err
	}
	// Require repeated success throughout a minute, rather than one lucky probe.
	until := time.Now().Add(time.Minute)
	for time.Now().Before(until) {
		if err := health(survivors); err != nil {
			return fmt.Errorf("surviving cluster failed during outage: %w", err)
		}
		if _, err := sql(survivors[0], "UPSERT INTO lifecycle.public.probe VALUES (3, 'during-stop')"); err != nil {
			return err
		}
		if _, err := kubectl("--server=https://"+survivors[0]+"."+domain+":6443", "patch", "configmap", "lifecycle-probe", "--type=merge", "-p", `{"data":{"value":"during-stop"}}`); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	fmt.Fprintln(out, "Starting the same saved VM and checking recovery")
	if err := command("vm", "start", "--name", joining, "--state-dir", state, "--wait", "15m"); err != nil {
		return err
	}
	if err := wait(func() error { return health(all) }); err != nil {
		return err
	}
	if err := wait(replicated); err != nil {
		return err
	}
	after, err := identity()
	if err != nil || !reflect.DeepEqual(before, after) {
		return fmt.Errorf("Cockroach node/store identities changed across restart")
	}
	persisted, err := sql(probe, "SELECT value FROM lifecycle.public.probe WHERE id=3")
	if err != nil || !reflect.DeepEqual(persisted, [][]string{{"during-stop"}}) {
		return fmt.Errorf("SQL writes during outage not visible after restart")
	}
	raw, err = kubectl("--server=https://"+probe+"."+domain+":6443", "get", "configmap", "lifecycle-probe", "-o", "jsonpath={.data.value}")
	if err != nil || raw != "during-stop" {
		return fmt.Errorf("Kine write during outage not visible through restarted API")
	}
	raw, err = run("tailscale", "ssh", "core@"+probe+"."+domain, "tailscale", "status", "--json", "--peers=false")
	var tailAfter map[string]json.RawMessage
	if err != nil || json.Unmarshal([]byte(raw), &tailAfter) != nil {
		return fmt.Errorf("cannot capture restarted Tailscale identity")
	}
	// Self includes online/traffic fields; compare the stable ID separately.
	var oldSelf, newSelf struct {
		ID      string
		DNSName string
	}
	if json.Unmarshal(selfBefore, &oldSelf) != nil || json.Unmarshal(tailAfter["Self"], &newSelf) != nil || oldSelf.ID == "" || oldSelf != newSelf {
		return fmt.Errorf("Tailscale identity changed across restart")
	}
	fmt.Fprintln(out, "Lifecycle assertions passed; shutting down test VMs")
	return nil
}

func lifecycleNodesReady(raw []byte, expected []string) error {
	var result struct {
		Items []struct {
			Metadata struct{ Name string }
			Status   struct {
				Conditions []struct{ Type, Status string }
			}
		}
	}
	if json.Unmarshal(raw, &result) != nil {
		return fmt.Errorf("invalid Kubernetes node response")
	}
	for _, name := range expected {
		ready := false
		for _, node := range result.Items {
			if node.Metadata.Name == name {
				for _, condition := range node.Status.Conditions {
					if condition.Type == "Ready" && condition.Status == "True" {
						ready = true
					}
				}
			}
		}
		if !ready {
			return fmt.Errorf("node %s is not Ready", name)
		}
	}
	return nil
}
