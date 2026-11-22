package vm

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/paulgmiller/infestation/internal/executil"
)

type autostartRunner struct {
	calls  []string
	linger string
	fail   string
}

func (r *autostartRunner) Run(_ context.Context, name string, args ...string) executil.Result {
	call := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, call)
	if r.fail != "" && strings.Contains(call, r.fail) {
		return executil.Result{Code: 1, Stderr: "PRIVATE-RAW-OUTPUT"}
	}
	if name == "loginctl" {
		return executil.Result{Stdout: r.linger}
	}
	return executil.Result{}
}

func testAutostartHost(t *testing.T) autostartHost {
	t.Helper()
	return autostartHost{ConfigDir: t.TempDir(), Executable: "/home/operator/go/bin/infestation", SearchPath: "/usr/local/bin:/usr/bin:/bin", UID: "1000"}
}

func TestUserAutostartInstallsWithoutStartingVMsOrChangingLingering(t *testing.T) {
	for _, linger := range []string{"yes\n", "no\n", "unknown", "query-failure"} {
		t.Run(strings.TrimSpace(linger), func(t *testing.T) {
			host := testAutostartHost(t)
			root := filepath.Join(t.TempDir(), "vms")
			runner := &autostartRunner{linger: linger}
			if linger == "query-failure" {
				runner.fail = "loginctl"
			}
			var out bytes.Buffer
			if err := installUserAutostart(context.Background(), root, host, &out, runner); err != nil {
				t.Fatal(err)
			}
			name, _, expected, err := autostartService(root, host)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(host.ConfigDir, "systemd", "user", name)
			raw, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(raw, expected) {
				t.Fatal("wrong saved user unit", err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("unit not private", err)
			}
			want := []string{"loginctl show-user 1000 --property=Linger --value", "systemctl --user daemon-reload", "systemctl --user enable " + name}
			if !reflect.DeepEqual(runner.calls, want) {
				t.Fatal("setup started compute, escalated privileges or changed lingering", runner.calls)
			}
			warning := strings.Contains(out.String(), "Warning:")
			if warning != (linger != "yes\n") || strings.Contains(out.String(), "PRIVATE") {
				t.Fatal("incorrect or unsafe lingering warning", out.String())
			}
			if warning && !strings.Contains(out.String(), "sudo loginctl enable-linger 1000") {
				t.Fatal("warning omitted the host-side remedy", out.String())
			}
			// Repeating setup updates only our managed unit, without restarting it.
			if err := installUserAutostart(context.Background(), root, host, io.Discard, runner); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUserAutostartRefusesForeignAndSymlinkedUnits(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		host := testAutostartHost(t)
		root := t.TempDir()
		name, _, _, _ := autostartService(root, host)
		path := filepath.Join(host.ConfigDir, "systemd", "user", name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		target := path
		if symlink {
			target += ".original"
			if err := os.Symlink(target, path); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(target, []byte("operator-owned unit"), 0600); err != nil {
			t.Fatal(err)
		}
		runner := &autostartRunner{}
		if err := installUserAutostart(context.Background(), root, host, io.Discard, runner); err == nil || len(runner.calls) != 0 {
			t.Fatal("changed or enabled an unowned unit", err)
		}
		raw, err := os.ReadFile(target)
		if err != nil || string(raw) != "operator-owned unit" {
			t.Fatal("foreign unit was overwritten", err)
		}
	}
}

func TestUserAutostartServiceFailuresAreClearAndRedacted(t *testing.T) {
	for _, stage := range []string{"daemon-reload", "enable"} {
		host := testAutostartHost(t)
		runner := &autostartRunner{linger: "yes", fail: stage}
		err := installUserAutostart(context.Background(), t.TempDir(), host, io.Discard, runner)
		if err == nil || !strings.Contains(err.Error(), "--autostart=false") || !strings.Contains(err.Error(), "no VMs created") || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("service failure unclear or exposed raw output", err)
		}
	}
	host := testAutostartHost(t)
	for _, path := range []string{"relative", "/tmp/quote\"exe", "/tmp/back\\slash", "/tmp/tab\texe"} {
		host.Executable = path
		if _, _, _, err := autostartService("/tmp/vms", host); err == nil {
			t.Fatal("unsupported executable path accepted", path)
		}
	}
}

func TestUserAutostartServiceQuotesPathsAndSeparatesStateRoots(t *testing.T) {
	host := testAutostartHost(t)
	host.Executable = `/home/operator/a $b%/infestation`
	root := `/home/operator/vms $literal% "quoted"`
	name, _, raw, err := autostartService(root, host)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `ExecStart="/home/operator/a $b%%/infestation" vm autostart-run --state-dir "/home/operator/vms $$literal%% \"quoted\""`) {
		t.Fatal("unit expands paths or splits arguments", string(raw))
	}
	other, _, _, _ := autostartService(root+"-other", host)
	if name == other || strings.Contains(string(raw), "User=") || !strings.Contains(string(raw), "WantedBy=default.target") {
		t.Fatal("wrong user-service identity or collision")
	}
	if _, _, _, err := autostartService(root+"\nExecStart=bad", host); err == nil {
		t.Fatal("unit injection accepted")
	}
}

// Executables below only record calls or write test sentinels: no QEMU, disk
// formatting, systemd manager or guest provisioning is involved.
func fakeAutostartComputeHost(t *testing.T, failSetup bool) string {
	t.Helper()
	tools := t.TempDir()
	log := filepath.Join(tools, "calls")
	t.Setenv("AUTOSTART_HOST_TEST_LOG", log)
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("PATH", tools)
	systemctl := "#!/bin/sh\nprintf 'systemctl %s %s\\n' \"$1\" \"$2\" >>\"$AUTOSTART_HOST_TEST_LOG\"\n"
	if failSetup {
		systemctl += "exit 23\n"
	}
	for name, script := range map[string]string{
		"systemctl":          systemctl,
		"loginctl":           "#!/bin/sh\necho no\n",
		"qemu-img":           "#!/bin/sh\nprintf 'qemu-img %s\\n' \"$1\" >>\"$AUTOSTART_HOST_TEST_LOG\"\nif [ \"$1\" = info ]; then\n  echo '{\"format\":\"qcow2\"}'\nelse\n  printf 'test disk' >\"$8\"\nfi\n",
		"qemu-system-x86_64": "#!/bin/sh\necho qemu-start >>\"$AUTOSTART_HOST_TEST_LOG\"\n",
	} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return log
}

func TestQEMUCreateAutostartPolicyAndFailBeforeDiskCreation(t *testing.T) {
	for _, scenario := range []string{"enabled", "opt-out", "setup-failure"} {
		t.Run(scenario, func(t *testing.T) {
			r := preparedProviderFixture(t, "qemu")
			r.Autostart = scenario != "opt-out"
			log := fakeAutostartComputeHost(t, scenario == "setup-failure")
			var out bytes.Buffer
			err := (qemuCompute{}).Create(context.Background(), r, &out)
			calls, _ := os.ReadFile(log)
			if scenario == "setup-failure" {
				if err == nil || !strings.Contains(err.Error(), "no VMs created") || strings.Contains(string(calls), "qemu") {
					t.Fatal("setup failure reached disk creation or launch", err, string(calls))
				}
				for _, n := range r.Nodes {
					if _, err := os.Lstat(filepath.Join(r.StateDir, n.Name)); !os.IsNotExist(err) {
						t.Fatal("setup failure reserved a machine", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.Autostart {
				if !strings.HasPrefix(string(calls), "systemctl --user daemon-reload\nsystemctl --user enable\nqemu-img info\n") || !strings.Contains(out.String(), "Warning: lingering is disabled") {
					t.Fatal("autostart was not set up before first disk", string(calls), out.String())
				}
			} else if strings.Contains(string(calls), "systemctl") || strings.Contains(out.String(), "lingering") {
				t.Fatal("opt-out still configured user services")
			}
			for _, n := range r.Nodes {
				state, err := readAutostart(filepath.Join(r.StateDir, n.Name))
				if err != nil || state.Enabled != r.Autostart || state.Resume != r.Autostart {
					t.Fatal("initial start lost policy or resume intent", state, err)
				}
			}
		})
	}
}
