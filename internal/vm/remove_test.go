package vm

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/fsutil"
)

func removalState(t *testing.T, provider string) (Options, string) {
	t.Helper()
	root, err := os.MkdirTemp("", "rmvm-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	o := Options{Name: "pool", StateDir: root}
	dir := filepath.Join(root, "pool")
	c := clusterConfig(ClusterOptions{Options: o, Size: 2, Domain: "example.ts.net"})
	for path, value := range map[string]any{"nodes.json": c, "provider.json": provider, "azure-scaleset.json": azureScaleSet{Subscription: "sub", Group: "pool-rg", Name: "pool", Ownership: "pool", Capacity: 2}} {
		if err := fsutil.WriteJSON(filepath.Join(dir, path), value); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range c.Nodes {
		if err := fsutil.WriteJSON(filepath.Join(dir, n.Name, "machine.json"), machine{Name: n.Name}); err != nil {
			t.Fatal(err)
		}
		for _, file := range []string{"disk.qcow2", "config.ign", "console.log", "qemu.pid"} {
			content := []byte("private-data")
			if file == "qemu.pid" {
				content = []byte("2147483647")
			}
			if err := os.WriteFile(filepath.Join(dir, n.Name, file), content, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return o, dir
}
func TestQEMURemovalRetainsPrivateStateAndOptionalDisks(t *testing.T) {
	for _, deleteDisks := range []bool{false, true} {
		o, dir := removalState(t, "qemu")
		// A refused Unix socket is a stale monitor, not a running VM.
		socket, err := net.Listen("unix", filepath.Join(dir, "pool-0", "qmp.sock"))
		if err != nil {
			t.Fatal(err)
		}
		socket.(*net.UnixListener).SetUnlinkOnClose(false)
		socket.Close()
		for range 2 {
			if err := RemoveGroup(context.Background(), o, "qemu", deleteDisks, io.Discard); err != nil {
				t.Fatal(err)
			}
		}
		for _, node := range []string{"pool-0", "pool-1"} {
			for _, file := range []string{"qemu.pid", "qmp.sock"} {
				if _, err := os.Lstat(filepath.Join(dir, node, file)); !os.IsNotExist(err) {
					t.Fatal("control file remains", file, err)
				}
			}
			_, err := os.Stat(filepath.Join(dir, node, "disk.qcow2"))
			if deleteDisks != os.IsNotExist(err) {
				t.Fatal("incorrect disk retention", err)
			}
			for _, file := range []string{"machine.json", "config.ign", "console.log"} {
				if _, err := os.Stat(filepath.Join(dir, node, file)); err != nil {
					t.Fatal(err)
				}
			}
		}
		if _, err := os.Stat(filepath.Join(dir, "nodes.json")); err != nil {
			t.Fatal(err)
		}
	}
}
func TestQEMURemovalRejectsForeignStateBeforeMutation(t *testing.T) {
	for _, scenario := range []string{"provider", "identity", "symlink", "lock", "pid"} {
		t.Run(scenario, func(t *testing.T) {
			o, dir := removalState(t, "qemu")
			node := filepath.Join(dir, "pool-0")
			switch scenario {
			case "provider":
				fsutil.WriteJSON(filepath.Join(dir, "provider.json"), "azure")
			case "identity":
				fsutil.WriteJSON(filepath.Join(dir, "pool-1", "machine.json"), machine{Name: "foreign"})
			case "symlink":
				target := filepath.Join(dir, "pool-1")
				os.Rename(target, target+"-saved")
				os.Symlink(target+"-saved", target)
			case "lock":
				os.WriteFile(filepath.Join(node, "start.lock"), nil, 0600)
			case "pid":
				os.WriteFile(filepath.Join(node, "qemu.pid"), []byte(strconv.Itoa(os.Getpid())), 0600)
			}
			if err := RemoveGroup(context.Background(), o, "qemu", true, io.Discard); err == nil {
				t.Fatal("unsafe removal accepted")
			}
			if _, err := os.Stat(filepath.Join(node, "disk.qcow2")); err != nil {
				t.Fatal("disk removed on failed guard", err)
			}
		})
	}
}

type removalRunner struct {
	azureRunner
	failDelete bool
}

func (r *removalRunner) Run(ctx context.Context, name string, args ...string) executil.Result {
	result := r.azureRunner.Run(ctx, name, args...)
	if len(args) > 1 && args[0] == "group" && args[1] == "delete" && r.failDelete {
		return executil.Result{Code: 1}
	}
	return result
}
func TestAzureRemovalOwnershipRetryAndFailure(t *testing.T) {
	for _, scenario := range []string{"success", "absent", "foreign", "failure", "provider", "invalid-record"} {
		t.Run(scenario, func(t *testing.T) {
			o, dir := removalState(t, "azure")
			r := &removalRunner{azureRunner: azureRunner{exists: true, owner: "pool"}}
			switch scenario {
			case "absent":
				r.exists = false
			case "foreign":
				r.owner = "other"
			case "failure":
				r.failDelete = true
			case "provider":
				fsutil.WriteJSON(filepath.Join(dir, "provider.json"), "qemu")
			case "invalid-record":
				fsutil.WriteJSON(filepath.Join(dir, "azure-scaleset.json"), azureScaleSet{Name: "other"})
			}
			err := removeAzureGroup(context.Background(), o, io.Discard, r)
			wantError := scenario != "success" && scenario != "absent"
			if (err != nil) != wantError {
				t.Fatal("wrong result", err)
			}
			calls := strings.Join(r.calls, "\n")
			deleted := strings.Contains(calls, "az group delete --subscription sub --name pool-rg --yes")
			if deleted != (scenario == "success" || scenario == "failure") {
				t.Fatal("wrong deletion scope", calls)
			}
			if scenario == "absent" && len(r.calls) != 1 {
				t.Fatal("absent group not idempotent", calls)
			}
			if _, err := os.Stat(filepath.Join(dir, "azure-scaleset.json")); err != nil {
				t.Fatal("private state removed", err)
			}
		})
	}
}

func TestRemovalProcessHelper(t *testing.T) {
	if os.Getenv("INFESTATION_REMOVAL_HELPER") != "1" {
		return
	}
	for {
		time.Sleep(time.Second)
	}
}
func TestQEMURemovalQuitsVerifiedProcess(t *testing.T) {
	o, dir := removalState(t, "qemu")
	node := filepath.Join(dir, "pool-0")
	cmd := exec.Command(os.Args[0], "-test.run=^TestRemovalProcessHelper$", "--", "unix:"+filepath.Join(node, "qmp.sock")+",server=on,wait=off", "file="+filepath.Join(node, "disk.qcow2")+",format=qcow2,if=virtio")
	cmd.Args[0] = "qemu-system-test"
	cmd.Env = append(os.Environ(), "INFESTATION_REMOVAL_HELPER=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	exited := make(chan struct{})
	go func() { cmd.Wait(); close(exited) }()
	if err := os.WriteFile(filepath.Join(node, "qemu.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(node, "qmp.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	commands := make(chan string, 4)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
			enc.Encode(map[string]any{"QMP": map[string]any{}})
			for {
				var command struct{ Execute string }
				if dec.Decode(&command) != nil {
					break
				}
				result := map[string]any{}
				if command.Execute == "query-name" {
					result["name"] = "pool-0"
				}
				enc.Encode(map[string]any{"return": result})
				if command.Execute != "qmp_capabilities" {
					commands <- command.Execute
				}
				if command.Execute == "quit" {
					listener.Close()
					cmd.Process.Kill()
					<-exited
					break
				}
			}
			conn.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := RemoveGroup(ctx, o, "qemu", true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if <-commands != "query-name" || <-commands != "quit" {
		t.Fatal("wrong QMP sequence")
	}
	if _, err := os.Stat(filepath.Join(node, "disk.qcow2")); !os.IsNotExist(err) {
		t.Fatal("disk retained", err)
	}
}

func TestQEMURemovalAfterPartialCreate(t *testing.T) {
	o, dir := removalState(t, "qemu")
	node := filepath.Join(dir, "pool-0")
	for _, file := range []string{"machine.json", "qemu.pid"} {
		if err := os.Remove(filepath.Join(node, file)); err != nil {
			t.Fatal(err)
		}
	}
	// The second VM was never reserved.
	if err := os.RemoveAll(filepath.Join(dir, "pool-1")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveGroup(context.Background(), o, "qemu", true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(node, "disk.qcow2")); !os.IsNotExist(err) {
		t.Fatal("partial-create disk remains", err)
	}
}
