package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/paulgmiller/infestation/internal/fsutil"
)

// Policy and desired power state are separate: enabling a stopped machine must
// not undo an intentional stop. Only an explicit start arms it again.
type autostartState struct {
	Enabled bool `json:"enabled"`
	Resume  bool `json:"resume"`
}

func readAutostart(dir string) (autostartState, error) {
	var state autostartState
	err := fsutil.ReadJSON(filepath.Join(dir, "autostart.json"), &state)
	if os.IsNotExist(err) {
		err = nil
	}
	return state, err
}

func setResume(dir string, resume bool) error {
	state, err := readAutostart(dir)
	if err != nil || !state.Enabled {
		return err
	}
	state.Resume = resume
	return writeAutostart(dir, state)
}

func writeAutostart(dir string, state autostartState) error {
	if err := fsutil.WriteJSON(filepath.Join(dir, "autostart.json"), state); err != nil {
		return err
	}
	// The atomic rename must reach the directory before issuing a power action,
	// including when the host loses power immediately after an intentional stop.
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// A kernel lock is released on process exit or host reboot. Keep its inode:
// unlinking a lock file lets another process lock a different inode concurrently.
func lockMachine(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, "start.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("another lifecycle operation may be in progress: %w", err)
	}
	return f, nil
}

// ConfigureAutostart changes saved host policy only; it never starts a VM or
// installs a host service. Run it on the actual compute host for SSH groups.
func ConfigureAutostart(ctx context.Context, o Options, enabled bool, out io.Writer) error {
	dir, c, err := loadCluster(o)
	if err != nil {
		return err
	}
	provider, err := groupProvider(dir)
	if err != nil {
		return err
	}
	if provider != "qemu" {
		return fmt.Errorf("autostart requires a local QEMU group")
	}
	if _, err := os.Lstat(filepath.Join(dir, "ssh.json")); !os.IsNotExist(err) {
		return fmt.Errorf("configure autostart on the SSH compute host using its state directory")
	}
	nodes, err := selectedQEMUNodes(c, o.Instance)
	if err != nil {
		return err
	}
	for _, node := range nodes {
		if err := validateLocalMachine(Options{Name: node.Name, StateDir: dir}); err != nil {
			return err
		}
	}
	for _, node := range nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		machine := Options{Name: node.Name, StateDir: dir}
		if err := validateLocalMachine(machine); err != nil {
			return err
		}
		if err := configureMachineAutostart(machine, enabled); err != nil {
			return fmt.Errorf("%s: %w", node.Name, err)
		}
		fmt.Fprintf(out, "%s autostart enabled=%t; host boot service setup: docs/autostart.md\n", node.Name, enabled)
	}
	return nil
}

func configureMachineAutostart(o Options, enabled bool) error {
	dir, _, err := load(o)
	if err != nil {
		return err
	}
	lock, err := lockMachine(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	present, err := processPresent(dir)
	if err != nil {
		return err
	}
	previous, err := readAutostart(dir)
	if err != nil {
		return err
	}
	return writeAutostart(dir, autostartState{Enabled: enabled, Resume: enabled && (present || previous.Enabled && previous.Resume)})
}

// ResumeLocal is the host boot entry point. Discovery excludes Azure and SSH
// routing records. Start rechecks the intent under the same lock as stop/remove.
func ResumeLocal(ctx context.Context, o Options, out io.Writer) error {
	return resumeLocal(ctx, o, out, Start)
}

func resumeLocal(ctx context.Context, o Options, out io.Writer, start func(context.Context, Options, io.Writer) error) error {
	machines, err := localMachines(o.StateDir)
	if err != nil {
		return err
	}
	var failures []error
	for _, machine := range machines {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		dir, _, err := load(machine.Options)
		if err == nil {
			var state autostartState
			state, err = readAutostart(dir)
			if err == nil && (!state.Enabled || !state.Resume) {
				continue
			}
		}
		if err == nil {
			machine.resumeOnly = true
			err = start(ctx, machine.Options, out)
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", machine.Name, err))
		}
	}
	return errors.Join(failures...)
}
