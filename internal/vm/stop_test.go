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
)

// This uses a test subprocess impersonating QEMU and a mock QMP socket, never a VM.
func TestStopWaitsForExitAndPreservesDisk(t *testing.T) {
	for _, scenario := range []string{"shutdown", "wrong-identity", "cancel"} {
		t.Run(scenario, func(t *testing.T) {
			_, dir := removalState(t, "qemu")
			node := filepath.Join(dir, "pool-0")
			cmd := exec.Command(os.Args[0], "-test.run=^TestRemovalProcessHelper$", "--", "unix:"+filepath.Join(node, "qmp.sock")+",server=on,wait=off", "file="+filepath.Join(node, "disk.qcow2")+",format=qcow2,if=virtio")
			cmd.Args[0] = "qemu-system-test"
			cmd.Env = append(os.Environ(), "INFESTATION_REMOVAL_HELPER=1")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			exited := make(chan struct{})
			go func() { _ = cmd.Wait(); close(exited) }()
			if err := os.WriteFile(filepath.Join(node, "qemu.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(node, "qmp.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			powered := make(chan struct{}, 1)
			go func() {
				for {
					conn, err := listener.Accept()
					if err != nil {
						return
					}
					enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
					_ = enc.Encode(map[string]any{"QMP": map[string]any{}})
					for {
						var command struct{ Execute string }
						if dec.Decode(&command) != nil {
							break
						}
						response := map[string]any{}
						if command.Execute == "query-name" {
							response["name"] = "pool-0"
							if scenario == "wrong-identity" {
								response["name"] = "someone-else"
							}
						}
						_ = enc.Encode(map[string]any{"return": response})
						if command.Execute == "system_powerdown" {
							powered <- struct{}{}
							if scenario == "shutdown" {
								_ = listener.Close()
								_ = cmd.Process.Kill()
								<-exited
							} else {
								cancel()
							}
						}
					}
					_ = conn.Close()
				}
			}()
			if err := configureMachineAutostart(Options{Name: "pool-0", StateDir: dir}, true); err != nil {
				t.Fatal(err)
			}
			state, err := readAutostart(node)
			if err != nil || !state.Resume {
				t.Fatal("enabling a verified running machine did not arm boot", state, err)
			}
			err = Stop(ctx, Options{Name: "pool-0", StateDir: dir}, io.Discard)
			switch scenario {
			case "shutdown":
				if err != nil {
					t.Fatal(err)
				}
			case "wrong-identity":
				if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
					t.Fatal("wrong monitor accepted", err)
				}
				select {
				case <-powered:
					t.Fatal("unowned VM powered down")
				default:
				}
			case "cancel":
				if err == nil {
					t.Fatal("canceled shutdown reported success")
				}
				present, e := processPresent(node)
				if e != nil || !present {
					t.Fatal("cancellation forced process off", e)
				}
			}
			state, stateErr := readAutostart(node)
			if stateErr != nil || state.Resume != (scenario == "wrong-identity") {
				t.Fatal("stop intent changed before identity verification or survived powerdown", state, stateErr)
			}
			raw, err := os.ReadFile(filepath.Join(node, "disk.qcow2"))
			if err != nil || string(raw) != "private-data" {
				t.Fatal("stop changed disk")
			}
			for _, file := range []string{"machine.json", "config.ign"} {
				if _, err := os.Stat(filepath.Join(node, file)); err != nil {
					t.Fatal("private state removed", err)
				}
			}
		})
	}
}
