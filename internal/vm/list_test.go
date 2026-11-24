package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/paulgmiller/infestation/internal/fsutil"
)

func TestListLocalPowerAndIdentity(t *testing.T) {
	for _, identity := range []string{"pool-0", "other"} {
		t.Run(identity, func(t *testing.T) {
			o, dir := removalState(t, "qemu")
			listener, err := net.Listen("unix", filepath.Join(dir, "pool-0", "qmp.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				conn, e := listener.Accept()
				if e != nil {
					return
				}
				defer conn.Close()
				enc, dec := json.NewEncoder(conn), json.NewDecoder(conn)
				_ = enc.Encode(map[string]any{"QMP": map[string]any{}})
				for {
					var command struct{ Execute string }
					if dec.Decode(&command) != nil {
						return
					}
					result := map[string]any{}
					if command.Execute == "query-name" {
						result["name"] = identity
					}
					_ = enc.Encode(map[string]any{"return": result})
				}
			}()
			var out bytes.Buffer
			err = List(context.Background(), o.StateDir, &out)
			want := "running"
			if identity != "pool-0" {
				want = "unknown"
			}
			if (err != nil) != (want == "unknown") {
				t.Fatal(err)
			}
			if fields := strings.Fields(out.String()); !reflect.DeepEqual(fields, []string{"NAME", "STATE", "pool-0", want, "pool-1", "stopped"}) {
				t.Fatal(fields)
			}
			// Listing must not remove disks, logs or stale PID records.
			for _, file := range []string{"disk.qcow2", "console.log", "qemu.pid"} {
				if _, e := os.Stat(filepath.Join(dir, "pool-1", file)); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}

func TestLocalUnknownProcessAndStaleMonitor(t *testing.T) {
	o, dir := removalState(t, "qemu")
	node := Options{Name: "pool-0", StateDir: dir}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "pool-0", "qmp.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	listener.Close()
	if state, err := localNodeState(context.Background(), node); err != nil || state != "stopped" {
		t.Fatal(state, err)
	}
	// A reused PID must not establish stopped ownership.
	if err := os.WriteFile(filepath.Join(dir, "pool-0", "qemu.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	if state, err := localNodeState(context.Background(), node); err == nil || state != "unknown" {
		t.Fatal(state, err)
	}
	// Continue listing the remaining known node despite the first failure.
	var out bytes.Buffer
	if err := List(context.Background(), o.StateDir, &out); err == nil || !strings.Contains(out.String(), "pool-1") {
		t.Fatal(out.String(), err)
	}
}

func TestListMalformedOwnershipKeepsKnownNodeNames(t *testing.T) {
	o, dir := removalState(t, "qemu")
	if err := fsutil.WriteJSON(filepath.Join(dir, "provider.json"), "unrecognized"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := List(context.Background(), o.StateDir, &out); err == nil {
		t.Fatal("invalid ownership succeeded")
	}
	if fields := strings.Join(strings.Fields(out.String()), " "); fields != "NAME STATE pool-0 unknown pool-1 unknown" {
		t.Fatal(fields)
	}
	// Corrupted inventory must not expose private data or use unvalidated names.
	if err := os.WriteFile(filepath.Join(dir, "nodes.json"), []byte(`{"private":"SECRET"`), 0600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	err := List(context.Background(), o.StateDir, &out)
	if err == nil || strings.Contains(out.String()+err.Error(), "SECRET") || strings.Join(strings.Fields(out.String()), " ") != "NAME STATE pool unknown" {
		t.Fatal(out.String(), err)
	}
}

func TestListDiscoveryAndPartialFailures(t *testing.T) {
	root := t.TempDir()
	for name, provider := range map[string]string{"local": "qemu", "remote": "qemu", "cloud": "azure"} {
		if err := fsutil.WriteJSON(filepath.Join(root, name, "provider.json"), provider); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "images"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "local"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	var calls []string
	var out bytes.Buffer
	err := listNodes(context.Background(), root, &out, func(_ context.Context, o Options, p string) ([]NodeState, error) {
		calls = append(calls, o.Name+":"+p)
		if o.Name == "remote" {
			return []NodeState{{"remote-0", "unknown"}}, fmt.Errorf("unreachable")
		}
		return []NodeState{{o.Name + "-0", "stopped"}}, nil
	})
	if err == nil || !reflect.DeepEqual(calls, []string{"cloud:azure", "local:qemu", "remote:qemu"}) {
		t.Fatal(calls, err)
	}
	if fields := strings.Fields(out.String()); !reflect.DeepEqual(fields, []string{"NAME", "STATE", "cloud-0", "stopped", "local-0", "stopped", "remote-0", "unknown"}) {
		t.Fatal(fields)
	}
	for _, path := range []string{t.TempDir(), filepath.Join(root, "absent")} {
		out.Reset()
		if err := List(context.Background(), path, &out); err != nil || strings.Join(strings.Fields(out.String()), " ") != "NAME STATE" {
			t.Fatal(out.String(), err)
		}
	}
}

type listAzureFake struct {
	azureFake
	instances []azureInstance
}

func (f *listAzureFake) Instances(context.Context, azureScaleSet) ([]azureInstance, error) {
	return f.instances, nil
}

func TestAzureNodePowerStates(t *testing.T) {
	o, _ := removalState(t, "azure")
	for _, tc := range []struct {
		name, owner       string
		ids, powers, want []string
		fail              bool
	}{
		{"power", "pool", []string{"1", "0"}, []string{"PowerState/deallocated", "PowerState/running"}, []string{"running", "stopped"}, false},
		{"allocated-stop", "pool", []string{"0", "1"}, []string{"PowerState/stopped", "PowerState/stopped"}, []string{"stopped", "stopped"}, false},
		{"missing", "pool", []string{"0"}, []string{"PowerState/running"}, []string{"running", "unknown"}, true},
		{"transitional", "pool", []string{"0", "1"}, []string{"PowerState/starting", "ProvisioningState/succeeded"}, []string{"unknown", "unknown"}, true},
		{"ownership", "other", []string{"0"}, []string{"PowerState/running"}, []string{"unknown", "unknown"}, true},
		{"unexpected", "pool", []string{"2"}, []string{"PowerState/running"}, []string{"unknown", "unknown"}, true},
		{"duplicate", "pool", []string{"0", "0"}, []string{"PowerState/running", "PowerState/stopped"}, []string{"unknown", "unknown"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &listAzureFake{azureFake: azureFake{owner: tc.owner, exists: true}}
			for i, id := range tc.ids {
				fake.instances = append(fake.instances, azureInstance{InstanceID: id, Statuses: []azureStatus{{Code: tc.powers[i]}}})
			}
			rows, err := unknownNodes(o)
			if err != nil {
				t.Fatal(err)
			}
			rows, err = azureNodeStates(context.Background(), o, rows, fake)
			var states []string
			for _, row := range rows {
				states = append(states, row.State)
			}
			if (err != nil) != tc.fail || !reflect.DeepEqual(states, tc.want) {
				t.Fatal(rows, err)
			}
			for _, call := range fake.calls {
				if !strings.HasPrefix(call, "get ") {
					t.Fatal("mutating API called", call)
				}
			}
		})
	}
}

func TestSSHListOwnershipAndPartialStates(t *testing.T) {
	o, dir := removalState(t, "qemu")
	owner := uuid.NewString()
	if err := fsutil.WriteJSON(filepath.Join(dir, "ssh-owner.json"), owner); err != nil {
		t.Fatal(err)
	}
	for _, mismatch := range []bool{false, true} {
		request := sshRequest{Operation: "list", Owner: owner, Options: o}
		if mismatch {
			request.Owner = uuid.NewString()
		}
		raw, _ := json.Marshal(request)
		var out bytes.Buffer
		err := ServeSSH(context.Background(), bytes.NewReader(raw), &out)
		if mismatch {
			if err == nil || out.Len() != 0 {
				t.Fatal("ownership bypass", err)
			}
			continue
		}
		var response sshNodeStates
		if err != nil || json.Unmarshal(out.Bytes(), &response) != nil || response.Failed || len(response.Nodes) != 2 || response.Nodes[0].State != "stopped" {
			t.Fatal(out.String(), err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "pool-0", "machine.json"), []byte(`{"Name":"other"}`), 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(sshRequest{Operation: "list", Owner: owner, Options: o})
	var partial bytes.Buffer
	var response sshNodeStates
	if err := ServeSSH(context.Background(), bytes.NewReader(raw), &partial); err != nil {
		t.Fatal(err)
	}
	if json.Unmarshal(partial.Bytes(), &response) != nil || !response.Failed || response.Nodes[0].State != "unknown" || response.Nodes[1].State != "stopped" {
		t.Fatal(partial.String())
	}
	// Failed transports cannot expose stderr credentials or classify nodes as stopped.
	fakeSSH(t, true)
	rows, _ := unknownNodes(o)
	rows, err := remoteNodeStates(context.Background(), o, sshCompute{Target: "host", Owner: owner, StateDir: o.StateDir}, rows)
	if err == nil || strings.Contains(err.Error(), "SECRET") || rows[0].State != "unknown" {
		t.Fatal(rows, err)
	}
}

func TestSSHListValidatesResponseAndRouting(t *testing.T) {
	o, _ := removalState(t, "qemu")
	_, requestPath := fakeSSH(t, false)
	path := filepath.Join(os.Getenv("PATH"), "ssh")
	script, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	script = append(script, []byte("printf '%s' \"$SSH_LIST_RESPONSE\"\n")...)
	if err := os.WriteFile(path, script, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"pool-0", "unowned-0"} {
		response := sshNodeStates{Nodes: []NodeState{{name, "running"}, {"pool-1", "unknown"}}, Failed: true}
		raw, _ := json.Marshal(response)
		t.Setenv("SSH_LIST_RESPONSE", string(raw))
		rows, _ := unknownNodes(o)
		rows, err := remoteNodeStates(context.Background(), o, sshCompute{Target: "host", Owner: "owner", StateDir: "/remote/vms"}, rows)
		if err == nil {
			t.Fatal("partial/invalid response succeeded")
		}
		want := "running"
		if name != "pool-0" {
			want = "unknown"
		}
		if rows[0].State != want {
			t.Fatal(rows)
		}
	}
	raw, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	var request sshRequest
	if json.Unmarshal(raw, &request) != nil || request.Operation != "list" || request.Owner != "owner" || request.Options.StateDir != "/remote/vms" || len(request.Profiles) != 0 {
		t.Fatal("incorrect list routing")
	}
}
