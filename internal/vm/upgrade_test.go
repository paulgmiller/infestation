package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	ignitionconfig "github.com/coreos/ignition/v2/config/v3_3"
	"github.com/paulgmiller/infestation/internal/fsutil"
	"github.com/paulgmiller/infestation/internal/ignition"
)

func TestUpgradeTransitionRejectsDowngradesAndSkippedMinors(t *testing.T) {
	for _, version := range []string{"v1.35.8+k3s1", K3sVersion} {
		if err := upgradeTransition(version); err != nil {
			t.Fatal(err)
		}
	}
	for _, version := range []string{"v1.34.12+k3s1", "v1.37.1+k3s1", "v1.36.6+k3s1", "v1.35.bad+k3s1", "v1.35.8+k3s1; reboot"} {
		if err := upgradeTransition(version); err == nil {
			t.Fatalf("unsafe transition accepted: %s", version)
		}
	}
}

func TestUpgradeRetentionMatchesDataAndSSHKeys(t *testing.T) {
	re, err := regexp.Compile("^(?:" + upgradeRetentionRegex(upgradeKeepPaths()) + ")$")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/var/lib/cockroach", "/var/lib/cockroach/000001.sst", "/var/lib/tailscale/tailscaled.state", "/etc/ssh/ssh_host_ed25519_key", "/etc/ssh/ssh_host_rsa_key.pub", "/etc/extensions/k3s.raw", "/etc/extensions/tailscale.raw", "/var/lib/infestation/join.json", "/opt/infestation/helper-build.identity"} {
		if !re.MatchString(p) {
			t.Fatalf("reset would delete %s", p)
		}
	}
	for _, p := range []string{"/etc/old-unit.conf", "/var/lib/cockroach-other", "/etc/extensions/other.raw"} {
		if re.MatchString(p) {
			t.Fatalf("unintended retention: %s", p)
		}
	}
	for _, p := range []string{"/var/lib/rancher/k3s/storage/app", "/var/lib/kubelet/pods/a", "/sys/fs/cgroup"} {
		if !upgradeRetains(p) {
			t.Fatal("retained workload rejected", p)
		}
	}
	for _, p := range []string{"/srv/database", "/var/lib/cockroach-other", "/var/lib/cockroach/../../lost"} {
		if upgradeRetains(p) {
			t.Fatal("unretained workload accepted", p)
		}
	}
}

func upgradeFixture(t *testing.T) (Options, string, []byte) {
	t.Helper()
	o, dir := removalState(t, "qemu")
	o.Instance = "0"
	_, c, err := loadCluster(o)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ignition.Render(c, c.Nodes[0], "", "", ignition.Bootstrap{HelperSourceCommit: strings.Repeat("a", 40), TailscaleAuthKey: "tskey-auth-TEST"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteFile(upgradeProfilePath(dir, "pool-0"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	c.K3sExtensionVersion = "v1.35.8+k3s1"
	other, err := ignition.Render(c, c.Nodes[1], "", "", ignition.Bootstrap{HelperSourceCommit: strings.Repeat("a", 40), TailscaleAuthKey: "tskey-auth-TEST"})
	if err != nil {
		t.Fatal(err)
	}
	otherRaw, err := json.Marshal(other)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteFile(upgradeProfilePath(dir, "pool-1"), otherRaw, 0600); err != nil {
		t.Fatal(err)
	}
	return o, dir, raw
}

func TestUpgradeProfilePreservesStateAndUsesStagedImages(t *testing.T) {
	_, _, raw := upgradeFixture(t)
	release := ignition.Release{K3s: K3sVersion, Tailscale: TailscaleVersion, Cockroach: CockroachImage, HelperCommit: strings.Repeat("b", 40)}
	raw, err := ignition.ReprovisionRelease(raw, release)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = retainedExtensionProfile(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := profileNode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Node != "pool-0" || cfg.Cluster.K3sExtensionVersion != K3sVersion || cfg.Cluster.CockroachImage != CockroachImage {
		t.Fatal("profile identity/pins changed unexpectedly")
	}
	doc, report, err := ignitionconfig.Parse(raw)
	if err != nil || report.IsFatal() {
		t.Fatal("invalid generated profile")
	}
	guard := false
	for _, f := range doc.Storage.Files {
		if f.Path == "/etc/extensions/k3s.raw" || f.Path == "/etc/extensions/tailscale.raw" {
			t.Fatal("reset would redownload staged artifact")
		}
		if strings.HasPrefix(f.Path, "/etc/cockroach/certs/") || f.Path == "/var/lib/infestation/join.json" {
			t.Fatal("profile would overwrite retained credentials/state")
		}
		if f.Path == "/opt/infestation/check-retained-state" {
			guard = true
		}
	}
	if !guard {
		t.Fatal("retained state is not guarded")
	}
}

func TestUpgradeRefusesUnselectedOrLockedInstance(t *testing.T) {
	o, dir, _ := upgradeFixture(t)
	o.Instance = ""
	if err := Upgrade(context.Background(), o, io.Discard); err == nil {
		t.Fatal("whole-group upgrade accepted")
	}
	o.Instance = "2"
	// A missing instance must fail before any guest operation.
	if err := Upgrade(context.Background(), o, io.Discard); err == nil {
		t.Fatal("invalid instance profile accepted")
	}
	o.Instance = "0"
	lock, err := lockMachine(filepath.Join(dir, "pool-0"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := Upgrade(context.Background(), o, io.Discard); err == nil || !strings.Contains(err.Error(), "lifecycle") {
		t.Fatal("upgrade did not respect VM lifecycle lock", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "pool-0", "disk.qcow2"))
	if err != nil || string(raw) != "private-data" {
		t.Fatal("refused upgrade changed disk")
	}
}

// Mock QEMU and guest commands exercise the installed/start checkpoint crash
// window. This never starts a VM, resets a host or contacts a real guest.
func TestUpgradeResumeDoesNotStartRunningInstanceOrCopyLiveDisk(t *testing.T) {
	for _, stage := range []string{"installed", "stopped"} {
		t.Run(stage, func(t *testing.T) {
			o, dir, profile := upgradeFixture(t)
			node := filepath.Join(dir, "pool-0")
			cmd := exec.Command(os.Args[0], "-test.run=^TestRemovalProcessHelper$", "--", "unix:"+filepath.Join(node, "qmp.sock")+",server=on,wait=off", "file="+filepath.Join(node, "disk.qcow2")+",format=qcow2,if=virtio")
			cmd.Args[0] = "qemu-system-test"
			cmd.Env = append(os.Environ(), "INFESTATION_REMOVAL_HELPER=1")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			if err := os.WriteFile(filepath.Join(node, "qemu.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(node, "qmp.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					go func() {
						defer conn.Close()
						enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
						_ = enc.Encode(map[string]any{"QMP": map[string]any{}})
						for {
							var request struct{ Execute string }
							if dec.Decode(&request) != nil {
								return
							}
							response := map[string]any{}
							if request.Execute == "query-name" {
								response["name"] = "pool-0"
							}
							_ = enc.Encode(map[string]any{"return": response})
						}
					}()
				}
			}()
			helper := strings.Repeat("b", 40)
			backup := filepath.Join(node, "upgrade-"+strings.ReplaceAll(K3sVersion, "+", "-"))
			r := upgradeRecord{Node: "pool-0", Stage: stage, HelperCommit: helper, Profile: profile, Identity: fmt.Sprintf("%x", sha256.Sum256([]byte("identity\n"))), TailID: "same", DatabaseIdentity: "header\nid"}
			if err := fsutil.WriteJSON(filepath.Join(backup, "progress.json"), r); err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			script := `#!/bin/sh
case "$*" in
 *sha256sum*) printf 'identity\n';;
 *status*--json*) printf '{"Self":{"ID":"same"}}';;
 *SELECT*) printf 'header\nid\n';;
 *VERSION_ID*) cat <<'OUT'
k3s version ` + K3sVersion + ` (mock)
` + strings.TrimPrefix(TailscaleVersion, "v") + `
` + ignition.HelperBuildIdentity(helper, "") + `
ok{
 "status":{"nodeInfo":{"kubeletVersion":"` + K3sVersion + `"},"conditions":[{"type":"Ready","status":"True"}]}}
OUT
 ;;
 *uncordon*) exit 0;;
 *) exit 99;;
esac
`
			if err := os.WriteFile(filepath.Join(bin, "tailscale"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var output bytes.Buffer
			err = Upgrade(ctx, o, &output)
			if stage == "installed" {
				if err != nil {
					t.Fatal("running instance could not resume verification", err)
				}
				if err := fsutil.ReadJSON(filepath.Join(backup, "progress.json"), &r); err != nil || r.Stage != "complete" {
					t.Fatal("resume did not complete", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "remain stopped") {
				t.Fatal("attempted live disk copy", err)
			}
			raw, err := os.ReadFile(filepath.Join(node, "disk.qcow2"))
			if err != nil || string(raw) != "private-data" {
				t.Fatal("resume modified retained disk")
			}
		})
	}
}

func TestUpgradeGroupPinsWaitForEveryInstance(t *testing.T) {
	o, dir, current := upgradeFixture(t)
	_, config, err := loadCluster(o)
	if err != nil {
		t.Fatal(err)
	}
	config.K3sExtensionVersion = "v1.35.8+k3s1"
	if err := fsutil.WriteJSON(inventoryPath(dir), config); err != nil {
		t.Fatal(err)
	}
	if err := updateUpgradeInventory(dir); err != nil {
		t.Fatal(err)
	}
	_, config, err = loadCluster(o)
	if err != nil || config.K3sExtensionVersion != "v1.35.8+k3s1" {
		t.Fatal("partial rollout changed group pins", err)
	}
	cfg, err := profileNode(current)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node = "pool-1"
	cfg.Cluster.Nodes[0].Name = "pool-1"
	// Generate an actual second-node profile with the target release.
	doc, err := ignition.Render(cfg.Cluster, cfg.Cluster.Nodes[0], "", "", ignition.Bootstrap{HelperSourceCommit: strings.Repeat("a", 40), TailscaleAuthKey: "tskey-auth-TEST"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteFile(upgradeProfilePath(dir, "pool-1"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := updateUpgradeInventory(dir); err != nil {
		t.Fatal(err)
	}
	_, config, err = loadCluster(o)
	if err != nil || config.K3sExtensionVersion != K3sVersion {
		t.Fatal("complete rollout did not advance group pins", err)
	}
	for _, name := range []string{"pool-0", "pool-1"} {
		data, err := os.ReadFile(filepath.Join(dir, name, "disk.qcow2"))
		if err != nil || string(data) != "private-data" {
			t.Fatal("bookkeeping changed an instance disk")
		}
	}
}

// Simulate update-engine state across attempts. No Flatcar update or node SSH
// runs here. In particular, a retry must reuse a matching pending payload.
func TestStageFlatcarHandlesAlreadyStagedAndBusyUpdates(t *testing.T) {
	for _, tc := range []struct {
		name, current, op, pending string
		wantUpdate, wantError      bool
	}{
		{"matching-pending", "4757.2.2", "UPDATE_STATUS_UPDATED_NEED_REBOOT", FlatcarVersion, false, false},
		{"different-pending", "4757.2.2", "UPDATE_STATUS_UPDATED_NEED_REBOOT", "4757.2.3", false, true},
		{"already-current", FlatcarVersion, "UPDATE_STATUS_IDLE", "", false, false},
		{"current-but-other-pending", FlatcarVersion, "UPDATE_STATUS_UPDATED_NEED_REBOOT", "9999.0.0", false, true},
		{"busy", "4757.2.2", "UPDATE_STATUS_DOWNLOADING", FlatcarVersion, false, true},
		{"stage-new", "4757.2.2", "UPDATE_STATUS_IDLE", "", true, false},
		{"downgrade", "9999.0.0", "UPDATE_STATUS_IDLE", "", false, true},
		{"missing-status", "4757.2.2", "", "", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			log := filepath.Join(bin, "calls")
			staged := filepath.Join(bin, "staged")
			script := `#!/bin/sh
printf '%s\n' "$*" >> "$UPGRADE_TEST_CALLS"
case "$*" in
 *update_engine_client*)
  if test -e "$UPGRADE_TEST_STAGED"; then
   printf 'CURRENT_OP=UPDATE_STATUS_UPDATED_NEED_REBOOT\nNEW_VERSION=%s\n' "$UPGRADE_TEST_TARGET"
  else
   printf 'CURRENT_OP=%s\nNEW_VERSION=%s\n' "$UPGRADE_TEST_OP" "$UPGRADE_TEST_PENDING"
  fi;;
 *flatcar-update*) touch "$UPGRADE_TEST_STAGED";;
 *VERSION_ID*) printf '%s' "$UPGRADE_TEST_CURRENT";;
 *) exit 90;;
esac
`
			if err := os.WriteFile(filepath.Join(bin, "tailscale"), []byte(script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			for k, v := range map[string]string{"UPGRADE_TEST_CALLS": log, "UPGRADE_TEST_STAGED": staged, "UPGRADE_TEST_TARGET": FlatcarVersion, "UPGRADE_TEST_CURRENT": tc.current, "UPGRADE_TEST_OP": tc.op, "UPGRADE_TEST_PENDING": tc.pending} {
				t.Setenv(k, v)
			}
			err := stageFlatcar(context.Background(), "mock.example.ts.net")
			if (err != nil) != tc.wantError {
				t.Fatalf("stage result: %v", err)
			}
			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}
			called := bytes.Contains(calls, []byte("/usr/bin/flatcar-update --to-version"))
			if called != tc.wantUpdate {
				t.Fatal("unexpected update attempt", string(calls))
			}
			if bytes.Contains(calls, []byte("reset_status")) {
				t.Fatal("discarded staged update")
			}
		})
	}
}
