package cli

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Explicitly opt in on a dedicated test tailnet: this creates three real nodes,
// initializes a new database and temporarily powers off one node. All
// disks and state are retained on success or failure; test VMs are shut down.
func TestLiveVMLifecycle(t *testing.T) {
	if os.Getenv("INFESTATION_LIFECYCLE_TEST") != "1" {
		t.Skip("set INFESTATION_LIFECYCLE_TEST=1; see docs/vm-lifecycle-test.md")
	}
	domain := os.Getenv("INFESTATION_TEST_DOMAIN")
	if domain == "" || os.Getenv("TAILSCALE_AUTH_KEY") == "" {
		t.Fatal("INFESTATION_TEST_DOMAIN and TAILSCALE_AUTH_KEY are required")
	}
	for _, tool := range []string{"kubectl", "cockroach", "tailscale"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Fatalf("%s is required", tool)
		}
	}
	state, err := os.MkdirTemp("", "inf-life-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("Private ownership state retained at", state)
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Minute)
	defer cancel()
	name := fmt.Sprintf("life-%d", time.Now().Unix())
	seed, peer, joining := name+"s", name+"p", name+"j"
	accel := os.Getenv("INFESTATION_VM_ACCEL")
	if accel == "" {
		accel = "kvm"
	}
	command := func(args ...string) error { return Run(ctx, args, io.Discard) }
	var attempted []string
	t.Cleanup(func() {
		cleanup, done := context.WithTimeout(context.Background(), 8*time.Minute)
		defer done()
		t.Log("Shutting down all test VMs; disks and private state retained")
		for _, group := range attempted {
			if _, err := os.Stat(filepath.Join(state, group, "nodes.json")); os.IsNotExist(err) {
				continue
			}
			if err := Run(cleanup, []string{"vm", "stop", "--name", group, "--state-dir", state, "--allow-outage"}, io.Discard); err != nil {
				t.Errorf("shutdown %s failed; inspect retained state: %v", group, err)
			}
		}
	})
	create := func(group string, founding bool) {
		attempted = append(attempted, group)
		args := []string{"vm", "create", "--name", group, "--state-dir", state, "--tailnet-domain", domain, "--accel", accel, "--wait", "0"}
		if founding {
			args = append(args, "--init")
		} else if target := os.Getenv("INFESTATION_TEST_SSH"); target != "" && group == joining {
			args = append(args, "--ssh", target)
			if path := os.Getenv("INFESTATION_TEST_SSH_STATE_DIR"); path != "" {
				args = append(args, "--ssh-state-dir", path)
			}
		}
		if ref := os.Getenv("INFESTATION_HELPER_SOURCE_REF"); ref != "" {
			args = append(args, "--helper-source-ref", ref)
		}
		// A base image path must exist on the selected compute host.
		image := os.Getenv("INFESTATION_VM_BASE_IMAGE")
		if group == joining && os.Getenv("INFESTATION_TEST_SSH") != "" {
			image = os.Getenv("INFESTATION_TEST_SSH_BASE_IMAGE")
		}
		if image != "" {
			args = append(args, "--base-image", image)
		}
		if err := command(args...); err != nil {
			t.Fatalf("create %s: %v; state retained", group, err)
		}
	}
	t.Log("Creating three independent servers, one with initialization authority")
	create(seed, true)
	create(peer, false)
	t.Log("Creating independent joining server (optionally on SSH host)")
	create(joining, false)
	if err := command("vm", "wait", "--name", seed, "--state-dir", state, "--wait", "25m"); err != nil {
		t.Fatal(err)
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
	sql := func(host, query string) (string, error) {
		values := url.Values{"sslmode": {"verify-full"}, "sslrootcert": {filepath.Join(certs, "ca.crt")}, "sslcert": {filepath.Join(certs, "client.root.crt")}, "sslkey": {filepath.Join(certs, "client.root.key")}, "options": {"-c allow_unsafe_internals=true"}}
		return run("cockroach", "sql", "--url", "postgresql://root@"+host+"."+domain+":26257/defaultdb?"+values.Encode(), "--format=csv", "--execute", query)
	}
	// Write unique test data through the node that will be stopped; access it
	// through survivors while it is off and through the same node after restart.
	if _, err := sql(probe, "CREATE DATABASE lifecycle; CREATE TABLE lifecycle.public.probe (id INT PRIMARY KEY, value STRING); INSERT INTO lifecycle.public.probe VALUES (1, 'before-stop')"); err != nil {
		t.Fatal(err)
	}
	// The host-level database and the Kubernetes API/Kine must each serve writes.
	health := func(nodes []string) error {
		for _, node := range nodes {
			if _, err := kubectl("--server=https://"+node+"."+domain+":6443", "get", "--raw=/readyz"); err != nil {
				return err
			}
			result, err := sql(node, "SELECT value FROM lifecycle.public.probe WHERE id=1")
			if err != nil || !strings.Contains(result, "before-stop") {
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
	wait := func(check func() error) {
		t.Helper()
		deadline := time.NewTimer(8 * time.Minute)
		defer deadline.Stop()
		for {
			if err := check(); err == nil {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatal("lifecycle context expired")
			case <-deadline.C:
				t.Fatal("lifecycle health did not converge; inspect retained state")
			case <-time.After(5 * time.Second):
			}
		}
	}
	wait(func() error { return health(all) })
	// Capture the actual Tailscale identity and Cockroach node/store identities.
	identity := func() ([][]string, error) {
		raw, err := sql(probe, "SELECT node_id, store_id FROM crdb_internal.kv_store_status ORDER BY node_id, store_id")
		if err != nil {
			return nil, err
		}
		return csv.NewReader(strings.NewReader(raw)).ReadAll()
	}
	before, err := identity()
	if err != nil || len(before) != 4 {
		t.Fatal("expected three Cockroach node/store identities")
	}
	var tailBefore map[string]json.RawMessage
	raw, err := run("tailscale", "ssh", "core@"+probe+"."+domain, "tailscale", "status", "--json", "--peers=false")
	if err != nil || json.Unmarshal([]byte(raw), &tailBefore) != nil {
		t.Fatal("cannot capture Tailscale identity")
	}
	selfBefore := tailBefore["Self"]
	// A ConfigMap update exercises K3s's persisted Kine datastore during outage.
	if _, err := kubectl("create", "configmap", "lifecycle-probe", "--from-literal=value=before-stop"); err != nil {
		t.Fatal(err)
	}
	// Wait for actual three-voter placement on every range, including system
	// and Kine ranges, before taking one store offline. Ready nodes alone do
	// not establish that replication has converged.
	replicated := func() error {
		raw, err := sql(survivors[0], "SELECT count(*) AS total, count(*) FILTER (WHERE COALESCE(array_length(voting_replicas, 1), 0) = 3 AND COALESCE(array_length(learner_replicas, 1), 0) = 0) AS ready FROM crdb_internal.ranges_no_leases")
		if err != nil {
			return err
		}
		rows, err := csv.NewReader(strings.NewReader(raw)).ReadAll()
		if err != nil || len(rows) != 2 || len(rows[1]) != 2 {
			return fmt.Errorf("invalid range replication response")
		}
		total, err := strconv.Atoi(rows[1][0])
		if err != nil || total == 0 || rows[1][0] != rows[1][1] {
			return fmt.Errorf("three-voter replication has not converged")
		}
		return nil
	}
	wait(replicated)
	t.Log("Stopping only the independent node; retaining the other two servers")
	if err := command("vm", "stop", "--name", joining, "--state-dir", state, "--allow-outage"); err != nil {
		t.Fatal(err)
	}
	// Require repeated success throughout a minute, rather than one lucky probe.
	until := time.Now().Add(time.Minute)
	for time.Now().Before(until) {
		if err := health(survivors); err != nil {
			t.Fatal("surviving cluster failed during outage:", err)
		}
		if _, err := sql(survivors[0], "UPSERT INTO lifecycle.public.probe VALUES (3, 'during-stop')"); err != nil {
			t.Fatal(err)
		}
		if _, err := kubectl("--server=https://"+survivors[0]+"."+domain+":6443", "patch", "configmap", "lifecycle-probe", "--type=merge", "-p", `{"data":{"value":"during-stop"}}`); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Second):
		}
	}
	t.Log("Starting the same saved VM and checking recovery")
	if err := command("vm", "start", "--name", joining, "--state-dir", state, "--wait", "15m"); err != nil {
		t.Fatal(err)
	}
	wait(func() error { return health(all) })
	wait(replicated)
	after, err := identity()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("Cockroach node/store identities changed across restart")
	}
	raw, err = sql(probe, "SELECT value FROM lifecycle.public.probe WHERE id=3")
	if err != nil || !strings.Contains(raw, "during-stop") {
		t.Fatal("SQL writes during outage not visible after restart")
	}
	raw, err = kubectl("--server=https://"+probe+"."+domain+":6443", "get", "configmap", "lifecycle-probe", "-o", "jsonpath={.data.value}")
	if err != nil || raw != "during-stop" {
		t.Fatal("Kine write during outage not visible through restarted API")
	}
	raw, err = run("tailscale", "ssh", "core@"+probe+"."+domain, "tailscale", "status", "--json", "--peers=false")
	var tailAfter map[string]json.RawMessage
	if err != nil || json.Unmarshal([]byte(raw), &tailAfter) != nil {
		t.Fatal("cannot capture restarted Tailscale identity")
	}
	// Self includes online/traffic fields; compare the stable ID separately.
	var oldSelf, newSelf struct {
		ID      string
		DNSName string
	}
	if json.Unmarshal(selfBefore, &oldSelf) != nil || json.Unmarshal(tailAfter["Self"], &newSelf) != nil || oldSelf.ID == "" || oldSelf != newSelf {
		t.Fatal("Tailscale identity changed across restart")
	}
	t.Log("Verified create, single-node stop, surviving SQL/Kine writes, restart and persistent identities; test VMs will shut down and disks/state remain retained")
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
