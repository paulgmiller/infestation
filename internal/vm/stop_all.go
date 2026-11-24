package vm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/paulgmiller/infestation/internal/executil"
)

// StopAllLocal discovers owned machines from one state root, not host process
// names. It never follows SSH routing or operates cloud resources.
func StopAllLocal(ctx context.Context, o Options, out io.Writer) error {
	return stopAllLocal(ctx, o, out, func(ctx context.Context, machines []localStopMachine, out io.Writer) error {
		targets := make([]stopTarget, 0, len(machines))
		for _, machine := range machines {
			targets = append(targets, qemuStopTarget(machine.Options, machine.databaseHost))
		}
		return runStopBatch(ctx, targets, true, out, executil.OSRunner{})
	})
}

type localStopMachine struct {
	Options
	databaseHost string
}

func localMachines(root string) ([]localStopMachine, error) {
	if root == "" {
		root = DefaultStateDir()
	}
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var machines []localStopMachine
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		} // Includes symlinks: never follow another state root.
		dir := filepath.Join(root, entry.Name())
		exists := func(file string) (bool, error) {
			_, err := os.Lstat(filepath.Join(dir, file))
			if os.IsNotExist(err) {
				return false, nil
			}
			return err == nil, err
		}
		remote, err := exists("ssh.json")
		if err != nil {
			return nil, err
		}
		if remote {
			continue
		}
		provider, err := groupProvider(dir)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		if provider != "qemu" {
			continue
		}
		inventory := inventoryPath(dir)
		if _, err := os.Lstat(inventory); err == nil {
			group, c, err := loadCluster(Options{Name: entry.Name(), StateDir: root})
			if err != nil {
				return nil, err
			}
			for _, node := range c.Nodes {
				o := Options{Name: node.Name, StateDir: group}
				if err := validateLocalMachine(o); err != nil {
					return nil, err
				}
				var host string
				if node.Database() {
					host = c.NodeAddress(node)
				}
				machines = append(machines, localStopMachine{Options: o, databaseHost: host})
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		} else if present, err := exists("machine.json"); err != nil {
			return nil, err
		} else if present {
			o := Options{Name: entry.Name(), StateDir: root}
			if err := validateLocalMachine(o); err != nil {
				return nil, err
			}
			machines = append(machines, localStopMachine{Options: o})
		}
	}
	return machines, nil
}

func validateLocalMachine(o Options) error {
	dir, err := directory(o)
	if err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing non-directory VM state: %s", o.Name)
	}
	_, _, err = load(o)
	return err
}

func stopAllLocal(ctx context.Context, o Options, out io.Writer, stop func(context.Context, []localStopMachine, io.Writer) error) error {
	if o.Instance != "" {
		return fmt.Errorf("--all cannot be combined with --instance")
	}
	if o.Name != "" {
		return fmt.Errorf("--all cannot be combined with --name")
	}
	if !o.AllowOutage {
		return fmt.Errorf("stopping all local VMs requires --allow-outage; this can take the cluster offline")
	}
	// Validate the complete saved inventory before changing any machine.
	machines, err := localMachines(o.StateDir)
	if err != nil {
		return err
	}
	if len(machines) == 0 {
		fmt.Fprintln(out, "No owned local VMs found under the selected state directory.")
		return nil
	}
	fmt.Fprintf(out, "Stopping %d local VMs; disks and private state will be retained.\n", len(machines))
	return stop(ctx, machines, out)
}
