package vm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/fsutil"
	"github.com/paulgmiller/infestation/internal/retirement"
)

// RemovalOptions separates intentional abandonment from normal guest-side
// retirement. No workstation database credential or SQL client is needed.
type RemovalOptions struct {
	DeleteDisks    bool
	AbandonCluster bool
	Timeout        time.Duration
}

type removalCheckpoint struct {
	ContractVersion string                `json:"contract_version"`
	RequestID       string                `json:"request_id"`
	ClusterID       string                `json:"cluster_id"`
	CAFingerprint   string                `json:"ca_fingerprint"`
	Targets         []retirement.Identity `json:"targets"`
	Stage           string                `json:"stage"`
	UpdatedAt       time.Time             `json:"updated_at"`
}

const retirementResponseLimit = 128 << 10

// retirementCall accepts only the two narrowly scoped helper operations. Raw
// output is never included in errors: Tailscale failures can contain auth URLs.
func retirementCall(ctx context.Context, runner executil.Runner, address, operation string, response any) error {
	result := runner.Run(ctx, "tailscale", "ssh", "core@"+address, "sudo", "-n", "/opt/infestation/infestation-node", "retire", operation)
	if result.Code != 0 {
		return fmt.Errorf("guest retirement helper failed on %s (exit %d); verify Tailscale SSH access and upgrade an older helper explicitly; compute and disks retained", address, result.Code)
	}
	if len(result.Stdout) > retirementResponseLimit {
		return fmt.Errorf("oversized guest retirement response; compute and disks retained")
	}
	decoder := json.NewDecoder(strings.NewReader(result.Stdout))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(response); err != nil {
		return fmt.Errorf("invalid guest retirement JSON; verify helper contract version; compute and disks retained")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return fmt.Errorf("guest retirement response contains extra output; compute and disks retained")
	}
	return nil
}

func prepareRemoval(ctx context.Context, dir string, config cluster.Config, options RemovalOptions, out io.Writer, runner executil.Runner) error {
	if options.AbandonCluster {
		fmt.Fprintln(out, "Abandoning database membership: replica evacuation and cluster health checks skipped by explicit request.")
		return nil
	}
	if len(config.Databases()) == 0 {
		return nil
	}
	if options.Timeout == 0 {
		options.Timeout = 30 * time.Minute
	}
	if options.Timeout < 0 {
		return fmt.Errorf("decommission timeout must be positive")
	}
	ctx, cancel := context.WithTimeout(ctx, options.Timeout)
	defer cancel()
	checkpoint := removalCheckpoint{ContractVersion: retirement.ContractVersion, RequestID: uuid.NewString()}
	for _, node := range config.Databases() {
		address := config.NodeAddress(node)
		// Ownership inventory, never a guest response, selects the SSH destination.
		identifyCtx, identifyCancel := context.WithTimeout(ctx, 30*time.Second)
		var identity retirement.Identity
		err := retirementCall(identifyCtx, runner, address, "--identify", &identity)
		identifyCancel()
		if err != nil {
			return fmt.Errorf("verify %s: %w", node.Name, err)
		}
		if err := retirement.ValidateIdentity(identity); err != nil {
			return fmt.Errorf("invalid retirement identity for %s; compute and disks retained", node.Name)
		}
		if identity.Name != node.Name || identity.Address != net.JoinHostPort(address, "26257") {
			return fmt.Errorf("retirement identity does not match owned node %s; compute and disks retained", node.Name)
		}
		if len(checkpoint.Targets) == 0 {
			checkpoint.ClusterID, checkpoint.CAFingerprint = identity.ClusterID, identity.CAFingerprint
		} else if identity.ClusterID != checkpoint.ClusterID || identity.CAFingerprint != checkpoint.CAFingerprint {
			return fmt.Errorf("owned database nodes disagree on cluster identity or CA; compute and disks retained")
		}
		checkpoint.Targets = append(checkpoint.Targets, identity)
	}
	request := retirement.Request{ContractVersion: retirement.ContractVersion, RequestID: checkpoint.RequestID, ClusterID: checkpoint.ClusterID, CAFingerprint: checkpoint.CAFingerprint, Targets: checkpoint.Targets}
	if err := retirement.ValidateRequest(request); err != nil {
		return fmt.Errorf("invalid retirement batch: %w", err)
	}
	path := filepath.Join(dir, "decommission.json")
	var previous removalCheckpoint
	if err := fsutil.ReadJSON(path, &previous); err == nil {
		if previous.ContractVersion != retirement.ContractVersion || previous.ClusterID != checkpoint.ClusterID || previous.CAFingerprint != checkpoint.CAFingerprint || !retirement.SameTargets(previous.Targets, checkpoint.Targets) {
			return fmt.Errorf("database cluster/node/store identities differ from retirement checkpoint; compute and disks retained")
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	save := func(stage string) error {
		checkpoint.Stage, checkpoint.UpdatedAt = stage, time.Now().UTC()
		return fsutil.WriteJSON(path, checkpoint)
	}
	if err := save("identities-verified"); err != nil {
		return err
	}
	for _, target := range checkpoint.Targets {
		fmt.Fprintf(out, "Decommission %s: n%s, stores %s\n", target.Name, target.NodeID, strings.Join(target.StoreIDs, ","))
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(raw) > retirement.MaxRequestBytes {
		return fmt.Errorf("retirement request exceeds helper limit; compute and disks retained")
	}
	coordinator := checkpoint.Targets[0]
	// The helper owns all database commands and its node credential. Exactly one
	// verified guest receives the entire batch, including its own retirement.
	operation := "--request=" + base64.StdEncoding.EncodeToString(raw)
	var receipt retirement.Receipt
	if err := retirementCall(ctx, runner, config.NodeAddress(config.Databases()[0]), operation, &receipt); err != nil {
		return err
	}
	if err := verifyRetirementReceipt(request, coordinator, receipt); err != nil {
		return err
	}
	if err := save("remaining-cluster-healthy"); err != nil {
		return err
	}
	fmt.Fprintln(out, "Guest database retirement complete; remaining cluster healthy. Removing owned compute.")
	return nil
}

func verifyRetirementReceipt(request retirement.Request, coordinator retirement.Identity, receipt retirement.Receipt) error {
	if err := retirement.ValidateReceipt(receipt, request, coordinator); err != nil {
		return fmt.Errorf("guest retirement receipt does not prove this verified batch completed decommissioning with zero replicas and healthy survivors; compute and disks retained")
	}
	return nil
}
