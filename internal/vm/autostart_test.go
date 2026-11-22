package vm

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulgmiller/infestation/internal/fsutil"
)

func TestAutostartPolicyDoesNotStartStoppedMachines(t *testing.T) {
	o, dir := removalState(t, "qemu")
	if err := ConfigureAutostart(context.Background(), o, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pool-0", "pool-1"} {
		state, err := readAutostart(filepath.Join(dir, name))
		if err != nil || !state.Enabled || state.Resume {
			t.Fatal("enabling policy armed an intentionally stopped VM", state, err)
		}
		info, err := os.Stat(filepath.Join(dir, name, "autostart.json"))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("policy must be private", err)
		}
	}
	if err := setResume(filepath.Join(dir, "pool-0"), true); err != nil {
		t.Fatal(err)
	}
	// Reapplying policy after a host outage must preserve the saved boot intent.
	if err := ConfigureAutostart(context.Background(), o, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	state, err := readAutostart(filepath.Join(dir, "pool-0"))
	if err != nil || !state.Resume {
		t.Fatal("reapplying policy lost saved boot intent", state, err)
	}
	o.Instance = "0"
	if err := ConfigureAutostart(context.Background(), o, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	state, err = readAutostart(filepath.Join(dir, "pool-0"))
	if err != nil || state.Enabled || state.Resume {
		t.Fatal("disable did not clear intent", state, err)
	}
}

func TestResumeRefusesMissingOrAliasedDiskAndInvalidPolicy(t *testing.T) {
	for _, scenario := range []string{"missing-disk", "disk-link", "profile-link", "policy"} {
		t.Run(scenario, func(t *testing.T) {
			_, dir := removalState(t, "qemu")
			node := Options{Name: "pool-0", StateDir: dir, resumeOnly: true}
			nodeDir := filepath.Join(dir, node.Name)
			fsutil.WriteJSON(filepath.Join(nodeDir, "autostart.json"), autostartState{Enabled: true, Resume: true})
			switch scenario {
			case "missing-disk":
				os.Remove(filepath.Join(nodeDir, "disk.qcow2"))
			case "disk-link", "profile-link":
				name := "disk.qcow2"
				if scenario == "profile-link" {
					name = "config.ign"
				}
				path := filepath.Join(nodeDir, name)
				os.Rename(path, path+"-saved")
				os.Symlink(path+"-saved", path)
			case "policy":
				os.WriteFile(filepath.Join(nodeDir, "autostart.json"), []byte("invalid"), 0600)
			}
			if err := Start(context.Background(), node, io.Discard); err == nil {
				t.Fatal("boot accepted missing/aliased state")
			}
			if scenario == "missing-disk" {
				if _, err := os.Lstat(filepath.Join(nodeDir, "disk.qcow2")); !os.IsNotExist(err) {
					t.Fatal("boot recreated missing disk")
				}
			}
		})
	}
}

func TestAutostartRejectsCloudRemoteAndUnverifiedPID(t *testing.T) {
	for _, scenario := range []string{"cloud", "ssh", "pid", "identity", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			o, dir := removalState(t, "qemu")
			switch scenario {
			case "cloud":
				fsutil.WriteJSON(filepath.Join(dir, "provider.json"), "azure")
			case "ssh":
				fsutil.WriteJSON(filepath.Join(dir, "ssh.json"), sshCompute{Target: "never-contact"})
			case "pid":
				os.WriteFile(filepath.Join(dir, "pool-0", "qemu.pid"), []byte("1"), 0600)
			case "identity":
				fsutil.WriteJSON(filepath.Join(dir, "pool-0", "machine.json"), machine{Name: "foreign"})
			case "symlink":
				os.Rename(filepath.Join(dir, "pool-0"), filepath.Join(dir, "saved"))
				os.Symlink(filepath.Join(dir, "saved"), filepath.Join(dir, "pool-0"))
			}
			if err := ConfigureAutostart(context.Background(), o, true, io.Discard); err == nil {
				t.Fatal("unsafe host policy accepted")
			}
		})
	}
}

func TestResumeSelectsOnlyArmedLocalVMsAndContinuesAfterFailure(t *testing.T) {
	o, dir := removalState(t, "qemu")
	for _, name := range []string{"pool-0", "pool-1"} {
		if err := fsutil.WriteJSON(filepath.Join(dir, name, "autostart.json"), autostartState{Enabled: true, Resume: true}); err != nil {
			t.Fatal(err)
		}
	}
	for _, provider := range []string{"azure", "qemu"} {
		remote := filepath.Join(o.StateDir, provider)
		fsutil.WriteJSON(filepath.Join(remote, "provider.json"), provider)
		if provider == "qemu" {
			fsutil.WriteJSON(filepath.Join(remote, "ssh.json"), sshCompute{Target: "never-contact"})
		}
	}
	calls := 0
	err := resumeLocal(context.Background(), o, io.Discard, func(_ context.Context, machine Options, _ io.Writer) error {
		calls++
		if !machine.resumeOnly || machine.StateDir != dir {
			t.Fatal("resume lost host intent/ownership guard")
		}
		return errors.New("test launch failure")
	})
	if err == nil || calls != 2 {
		t.Fatal("boot must attempt every eligible local machine", calls, err)
	}
	setResume(filepath.Join(dir, "pool-0"), false)
	fsutil.WriteJSON(filepath.Join(dir, "pool-1", "autostart.json"), autostartState{Enabled: false, Resume: true})
	if err := resumeLocal(context.Background(), o, io.Discard, func(context.Context, Options, io.Writer) error {
		t.Fatal("boot ignored stop or opt-in")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResumeUsesSavedDiskAndRechecksStopIntent(t *testing.T) {
	o, dir := removalState(t, "qemu")
	node := Options{Name: "pool-0", StateDir: dir, resumeOnly: true}
	nodeDir := filepath.Join(dir, node.Name)
	fsutil.WriteJSON(filepath.Join(nodeDir, "machine.json"), machine{Name: node.Name, Accel: "tcg", Network: "slirp"})
	fsutil.WriteJSON(filepath.Join(nodeDir, "autostart.json"), autostartState{Enabled: true, Resume: true})
	tools := t.TempDir()
	args := filepath.Join(tools, "arguments")
	t.Setenv("AUTOSTART_TEST_ARGS", args)
	for name, script := range map[string]string{
		"qemu-system-x86_64": "#!/bin/sh\nprintf '%s\\n' \"$@\" >\"$AUTOSTART_TEST_ARGS\"\n",
		"qemu-img":           "#!/bin/sh\nexit 99\n",
	} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", tools)
	// A lock file left by a dead launcher is no longer a permanent boot blocker.
	os.WriteFile(filepath.Join(nodeDir, "start.lock"), nil, 0600)
	if err := Start(context.Background(), node, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(args)
	if err != nil || !strings.Contains(string(raw), "file="+filepath.Join(nodeDir, "disk.qcow2")) {
		t.Fatal("did not reopen saved disk", err)
	}
	disk, err := os.ReadFile(filepath.Join(nodeDir, "disk.qcow2"))
	if err != nil || string(disk) != "private-data" {
		t.Fatal("boot changed saved disk", err)
	}
	// Stop wins even if discovery already selected this machine for boot.
	if err := Stop(context.Background(), node, io.Discard); err != nil {
		t.Fatal(err)
	}
	os.Remove(args)
	if err := Start(context.Background(), node, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(args); !os.IsNotExist(err) {
		t.Fatal("boot raced past an intentional stop")
	}
	// Explicit start rearms the policy, unlike boot's conditional start.
	node.resumeOnly = false
	if err := Start(context.Background(), node, io.Discard); err != nil {
		t.Fatal(err)
	}
	state, err := readAutostart(nodeDir)
	if err != nil || !state.Resume {
		t.Fatal("explicit start did not rearm autostart", state, err)
	}
	if err := RemoveGroup(context.Background(), o, "qemu", RemovalOptions{AbandonCluster: true}, io.Discard); err != nil {
		t.Fatal(err)
	}
	state, err = readAutostart(nodeDir)
	if err != nil || state.Resume {
		t.Fatal("removal left retained disk armed for boot", state, err)
	}
}

func TestMachineLockSurvivesStaleFileButExcludesLiveOwner(t *testing.T) {
	dir := t.TempDir()
	first, err := lockMachine(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := lockMachine(dir); err == nil {
		second.Close()
		t.Fatal("concurrent lifecycle lock accepted")
	}
	first.Close()
	next, err := lockMachine(dir)
	if err != nil {
		t.Fatal("stale file blocked lock after owner exit", err)
	}
	next.Close()
}
