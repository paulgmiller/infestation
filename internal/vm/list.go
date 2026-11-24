package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/paulgmiller/infestation/internal/fsutil"
)

// NodeState is a compute power observation, not guest or cluster readiness.
type NodeState struct{ Name, State string }

// List reads only saved ownership records and provider power state.
func List(ctx context.Context, root string, out io.Writer) error {
	return listNodes(ctx, root, out, func(ctx context.Context, o Options, provider string) ([]NodeState, error) {
		p, err := ProviderFor(provider)
		if err != nil {
			return nil, err
		}
		return p.NodeStates(ctx, o)
	})
}

func listNodes(ctx context.Context, root string, out io.Writer, observe func(context.Context, Options, string) ([]NodeState, error)) error {
	if root == "" {
		root = DefaultStateDir()
	}
	entries, err := os.ReadDir(root)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATE")
	var failures []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		} // Never follow a symlink to another state root.
		dir := filepath.Join(root, entry.Name())
		tracked := false
		for _, file := range []string{"nodes.json", "cluster.json", "machine.json", "provider.json", "ssh.json"} {
			if _, e := os.Lstat(filepath.Join(dir, file)); !os.IsNotExist(e) {
				tracked = true
				break
			}
		}
		if !tracked {
			continue
		}
		o := Options{Name: entry.Name(), StateDir: root}
		provider, e := groupProvider(dir)
		var rows []NodeState
		if e == nil {
			// Bound unavailable providers so one group cannot stall the whole inventory.
			deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
			rows, e = observe(deadline, o, provider)
			cancel()
		}
		if len(rows) == 0 {
			rows, _ = unknownNodes(o)
			if len(rows) == 0 {
				rows = []NodeState{{Name: o.Name, State: "unknown"}}
			}
			if e == nil {
				e = fmt.Errorf("no node power states returned")
			}
		}
		for _, row := range rows {
			fmt.Fprintf(w, "%s\t%s\n", row.Name, row.State)
		}
		if e != nil {
			failures = append(failures, fmt.Errorf("%s: %w", o.Name, e))
		}
	}
	return errors.Join(append(failures, w.Flush())...)
}

func unknownNodes(o Options) ([]NodeState, error) {
	_, c, err := loadCluster(o)
	if err != nil {
		return nil, fmt.Errorf("cannot read owned node inventory")
	}
	rows := make([]NodeState, len(c.Nodes))
	for i, n := range c.Nodes {
		rows[i] = NodeState{Name: n.Name, State: "unknown"}
	}
	return rows, nil
}

func (qemuCompute) NodeStates(ctx context.Context, o Options) ([]NodeState, error) {
	dir, err := directory(o)
	if err != nil {
		return nil, err
	}
	provider, err := groupProvider(dir)
	if err != nil || provider != "qemu" {
		return nil, fmt.Errorf("QEMU provider ownership mismatch")
	}
	rows, err := unknownNodes(o)
	if err != nil {
		// Standalone local machines have no group inventory.
		if _, e := os.Lstat(inventoryPath(dir)); !os.IsNotExist(e) {
			return nil, err
		}
		rows = []NodeState{{Name: o.Name, State: "unknown"}}
	} else {
		var link sshCompute
		if e := fsutil.ReadJSON(filepath.Join(dir, "ssh.json"), &link); e == nil {
			return remoteNodeStates(ctx, o, link, rows)
		} else if !os.IsNotExist(e) {
			return rows, fmt.Errorf("cannot read SSH ownership")
		}
	}
	var failures []error
	for i := range rows {
		node := Options{Name: rows[i].Name, StateDir: dir}
		if rows[i].Name == o.Name {
			node = o
		}
		state, e := localNodeState(ctx, node)
		rows[i].State = state
		if e != nil {
			failures = append(failures, fmt.Errorf("%s: %w", rows[i].Name, e))
		}
	}
	return rows, errors.Join(failures...)
}

func localNodeState(ctx context.Context, o Options) (string, error) {
	if err := validateLocalMachine(o); err != nil {
		return "unknown", fmt.Errorf("cannot validate saved VM identity")
	}
	dir, _ := directory(o)
	if err := ctx.Err(); err != nil {
		return "unknown", err
	}
	raw, monitorErr := qmp(ctx, dir, "query-name")
	if monitorErr == nil {
		var identity struct{ Name string }
		if json.Unmarshal(raw, &identity) != nil || identity.Name != o.Name {
			return "unknown", fmt.Errorf("QEMU monitor identity mismatch")
		}
		// This is compute power state: a responsive owned emulator is powered
		// on, even when its guest CPUs are paused or bootstrap has failed.
		return "running", nil
	}
	present, err := processPresent(dir)
	if err != nil {
		return "unknown", fmt.Errorf("cannot verify QEMU process identity")
	}
	if !present && (errors.Is(monitorErr, os.ErrNotExist) || errors.Is(monitorErr, syscall.ECONNREFUSED)) {
		return "stopped", nil
	}
	return "unknown", fmt.Errorf("QEMU power state unavailable")
}

type sshNodeStates struct {
	Nodes  []NodeState
	Failed bool
}

func remoteNodeStates(ctx context.Context, o Options, link sshCompute, rows []NodeState) ([]NodeState, error) {
	var out bytes.Buffer
	err := sshCall(ctx, link.Target, sshRequest{Operation: "list", Owner: link.Owner, Options: Options{Name: o.Name, StateDir: link.StateDir}}, &out)
	if err != nil {
		return rows, err
	}
	var response sshNodeStates
	if json.Unmarshal(out.Bytes(), &response) != nil || len(response.Nodes) != len(rows) {
		return rows, fmt.Errorf("invalid SSH power-state response")
	}
	for i, row := range response.Nodes {
		if row.Name != rows[i].Name || (row.State != "running" && row.State != "stopped" && row.State != "unknown") {
			return rows, fmt.Errorf("SSH node identity or power-state mismatch")
		}
	}
	for _, row := range response.Nodes {
		if row.State == "unknown" {
			response.Failed = true
		}
	}
	if response.Failed {
		return response.Nodes, fmt.Errorf("SSH host could not determine all node states")
	}
	return response.Nodes, nil
}

func (azureCompute) NodeStates(ctx context.Context, o Options) ([]NodeState, error) {
	rows, err := unknownNodes(o)
	if err != nil {
		return nil, err
	}
	client, err := newAzureClient()
	if err != nil {
		return rows, err
	}
	return azureNodeStates(ctx, o, rows, client)
}

func azureNodeStates(ctx context.Context, o Options, rows []NodeState, client azureAPI) ([]NodeState, error) {
	_, set, err := loadAzureSet(o)
	if err != nil {
		return rows, fmt.Errorf("cannot validate saved Azure ownership")
	}
	owner, exists, err := client.Group(ctx, set)
	if err != nil {
		return rows, err
	}
	if !exists || owner != set.Ownership {
		return rows, fmt.Errorf("Azure group missing or ownership mismatch")
	}
	instances, err := client.Instances(ctx, set)
	if err != nil {
		return rows, err
	}
	seen := map[int]bool{}
	for _, instance := range instances {
		i, e := strconv.Atoi(instance.InstanceID)
		if e != nil || i < 0 || i >= len(rows) || seen[i] {
			return rows, fmt.Errorf("Azure instance inventory differs from saved ownership")
		}
		seen[i] = true
	}
	for _, instance := range instances {
		i, _ := strconv.Atoi(instance.InstanceID)
		for _, status := range instance.Statuses {
			switch status.Code {
			case "PowerState/running":
				rows[i].State = "running"
			case "PowerState/stopped", "PowerState/deallocated":
				rows[i].State = "stopped"
			}
		}
	}
	for _, row := range rows {
		if row.State == "unknown" {
			return rows, fmt.Errorf("Azure power state missing or transitional")
		}
	}
	return rows, nil
}
