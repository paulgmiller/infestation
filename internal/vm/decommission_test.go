package vm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/fsutil"
	"github.com/paulgmiller/infestation/internal/retirement"
)

const testRemovalClusterID = "a9568bbd-95aa-4e57-baff-43303851e6b4"
const testRemovalFingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func retirementTestIdentity(index int) retirement.Identity {
	return retirement.Identity{ContractVersion: retirement.ContractVersion, Name: fmt.Sprintf("pool-%d", index), Address: fmt.Sprintf("pool-%d.example.ts.net:26257", index), ClusterID: testRemovalClusterID, CAFingerprint: testRemovalFingerprint, NodeID: fmt.Sprint(index + 1), StoreIDs: []string{fmt.Sprint(index + 11)}}
}

type retirementRunner struct {
	calls          []string
	request        *retirement.Request
	identities     map[string]retirement.Identity
	identityError  bool
	identityOutput string
	mutateReceipt  func(*retirement.Receipt)
	failRequest    bool
}

func (r *retirementRunner) Run(_ context.Context, name string, args ...string) executil.Result {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if name != "tailscale" || len(args) != 7 || args[0] != "ssh" || args[2] != "sudo" || args[3] != "-n" || args[4] != "/opt/infestation/infestation-node" || args[5] != "retire" {
		return executil.Result{Code: 99}
	}

	operation := args[len(args)-1]
	if operation == "--identify" {
		if r.identityError {
			return executil.Result{Code: 1, Stderr: "AUTH-URL-SENSITIVE-TEST"}
		}
		if r.identityOutput != "" {
			return executil.Result{Stdout: r.identityOutput}
		}
		identity, ok := r.identities[args[1]]
		if !ok {
			return executil.Result{Code: 98}
		}
		raw, _ := json.Marshal(identity)
		return executil.Result{Stdout: string(raw)}
	}
	if strings.HasPrefix(operation, "--request=") {
		if r.failRequest {
			return executil.Result{Code: 1, Stderr: "PRIVATE-FAILURE-DETAILS"}
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(operation, "--request="))
		if err != nil {
			return executil.Result{Code: 97}
		}
		var request retirement.Request
		if json.Unmarshal(raw, &request) != nil {
			return executil.Result{Code: 96}
		}
		r.request = &request
		receipt := retirement.Receipt{ContractVersion: request.ContractVersion, RequestID: request.RequestID, ClusterID: request.ClusterID, CAFingerprint: request.CAFingerprint, Targets: request.Targets, Coordinator: request.Targets[0], Completed: true, RemainingHealthy: true, ZeroReplicas: true}
		if r.mutateReceipt != nil {
			r.mutateReceipt(&receipt)
		}
		raw, _ = json.Marshal(receipt)
		return executil.Result{Stdout: string(raw)}
	}
	return executil.Result{Code: 95}
}
func newRetirementRunner() *retirementRunner {
	return &retirementRunner{identities: map[string]retirement.Identity{"core@pool-0.example.ts.net": retirementTestIdentity(0), "core@pool-1.example.ts.net": retirementTestIdentity(1)}}
}

func TestRemovalDelegatesEntireVerifiedBatchToOneGuest(t *testing.T) {
	o, dir := removalState(t, "qemu")
	_, config, err := loadCluster(o)
	if err != nil {
		t.Fatal(err)
	}
	runner := newRetirementRunner()
	if err := prepareRemoval(context.Background(), dir, config, RemovalOptions{}, io.Discard, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 3 || runner.request == nil || len(runner.request.Targets) != 2 {
		t.Fatal("batch was not delegated once after two identifications", runner.calls)
	}
	if !strings.Contains(runner.calls[2], "core@pool-0.example.ts.net sudo -n /opt/infestation/infestation-node retire --request=") {
		t.Fatal("coordinator not verified owned guest", runner.calls)
	}
	var checkpoint removalCheckpoint
	if err := fsutil.ReadJSON(filepath.Join(dir, "decommission.json"), &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.Stage != "remaining-cluster-healthy" || checkpoint.RequestID != runner.request.RequestID || checkpoint.ClusterID != testRemovalClusterID {
		t.Fatal("wrong checkpoint", checkpoint)
	}
	info, err := os.Stat(filepath.Join(dir, "decommission.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("checkpoint not private", err)
	}
}

func TestRetirementIdentityFailurePreventsRequest(t *testing.T) {
	for _, scenario := range []string{"old-helper", "wrong-name", "wrong-address", "wrong-cluster", "wrong-ca", "duplicate-node", "duplicate-store", "version", "malformed-json", "extra-output", "unknown-field", "oversized", "negative-timeout", "checkpoint"} {
		t.Run(scenario, func(t *testing.T) {
			o, dir := removalState(t, "qemu")
			_, config, err := loadCluster(o)
			if err != nil {
				t.Fatal(err)
			}
			runner := newRetirementRunner()
			identity := runner.identities["core@pool-1.example.ts.net"]
			options := RemovalOptions{}
			switch scenario {
			case "old-helper":
				runner.identityError = true
			case "wrong-name":
				identity.Name = "foreign"
			case "wrong-address":
				identity.Address = "foreign.example.ts.net:26257"
			case "wrong-cluster":
				identity.ClusterID = "37c63506-9f58-48e4-a4b2-77724c15a2f3"
			case "wrong-ca":
				identity.CAFingerprint = strings.Repeat("f", 64)
			case "duplicate-node":
				identity.NodeID = "1"
			case "duplicate-store":
				identity.StoreIDs = []string{"11"}
			case "version":
				identity.ContractVersion = "999"
			case "malformed-json":
				runner.identityOutput = "not-json-SECRET"
			case "extra-output":
				raw, _ := json.Marshal(retirementTestIdentity(0))
				runner.identityOutput = string(raw) + "\nSECRET"
			case "unknown-field":
				runner.identityOutput = `{"private_key":"SECRET"}`
			case "oversized":
				runner.identityOutput = strings.Repeat("x", retirementResponseLimit+1)
			case "negative-timeout":
				options.Timeout = -time.Second
			case "checkpoint":
				fsutil.WriteJSON(filepath.Join(dir, "decommission.json"), removalCheckpoint{ClusterID: "wrong"})
			}
			runner.identities["core@pool-1.example.ts.net"] = identity
			err = prepareRemoval(context.Background(), dir, config, options, io.Discard, runner)
			if err == nil {
				t.Fatal("unsafe identities accepted")
			}
			if strings.Contains(err.Error(), "SENSITIVE") || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("raw helper response leaked", err)
			}
			if runner.request != nil {
				t.Fatal("retirement submitted before identity guards")
			}
		})
	}
}

func TestRetirementReceiptMismatchRetainsCompute(t *testing.T) {
	for _, scenario := range []string{"request-id", "cluster", "ca", "target", "coordinator", "incomplete", "replicas", "unhealthy", "version", "failed-command"} {
		t.Run(scenario, func(t *testing.T) {
			o, dir := removalState(t, "qemu")
			_, config, err := loadCluster(o)
			if err != nil {
				t.Fatal(err)
			}
			runner := newRetirementRunner()
			runner.mutateReceipt = func(receipt *retirement.Receipt) {
				switch scenario {
				case "request-id":
					receipt.RequestID = "old-receipt"
				case "cluster":
					receipt.ClusterID = "wrong"
				case "ca":
					receipt.CAFingerprint = "wrong"
				case "target":
					receipt.Targets = receipt.Targets[:1]
				case "coordinator":
					receipt.Coordinator = receipt.Targets[1]
				case "incomplete":
					receipt.Completed = false
				case "replicas":
					receipt.ZeroReplicas = false
				case "unhealthy":
					receipt.RemainingHealthy = false
				case "version":
					receipt.ContractVersion = "999"
				}
			}
			runner.failRequest = scenario == "failed-command"
			err = prepareRemoval(context.Background(), dir, config, RemovalOptions{}, io.Discard, runner)
			if err == nil {
				t.Fatal("unsafe receipt accepted")
			}
			var checkpoint removalCheckpoint
			if err := fsutil.ReadJSON(filepath.Join(dir, "decommission.json"), &checkpoint); err != nil {
				t.Fatal(err)
			}
			if checkpoint.Stage != "identities-verified" {
				t.Fatal("bad receipt advanced checkpoint")
			}
			if _, err := os.Stat(filepath.Join(dir, "pool-0", "disk.qcow2")); err != nil {
				t.Fatal("compute disk changed", err)
			}
		})
	}
}

func TestExplicitAbandonDoesNotContactGuests(t *testing.T) {
	o, dir := removalState(t, "qemu")
	_, config, err := loadCluster(o)
	if err != nil {
		t.Fatal(err)
	}
	runner := newRetirementRunner()
	if err := prepareRemoval(context.Background(), dir, config, RemovalOptions{AbandonCluster: true}, io.Discard, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 0 {
		t.Fatal("abandon contacted guests")
	}
}

type inventoryRemovalClient struct {
	azureFake
	instances     []azureInstance
	instanceError error
}

func (r *inventoryRemovalClient) Instances(_ context.Context, set azureScaleSet) ([]azureInstance, error) {
	r.call("instances", set)
	return r.instances, r.instanceError
}
func TestAzureNormalRemovalVerifiesCompleteInstanceBatch(t *testing.T) {
	for _, ids := range [][]string{{"0", "1", "2"}, {"0", "9"}, nil} {
		t.Run(strings.Join(ids, ","), func(t *testing.T) {
			o, _ := removalState(t, "azure")
			client := &inventoryRemovalClient{azureFake: azureFake{exists: true, owner: "pool"}}
			for _, id := range ids {
				client.instances = append(client.instances, azureInstance{InstanceID: id})
			}
			err := removeAzureGroup(context.Background(), o, RemovalOptions{}, io.Discard, client)
			if err == nil || !strings.Contains(err.Error(), "inventory") {
				t.Fatal("unowned instance batch not detected", err)
			}
			if strings.Contains(strings.Join(client.calls, "\n"), "delete ") {
				t.Fatal("compute deleted before evacuation")
			}
		})
	}
}
