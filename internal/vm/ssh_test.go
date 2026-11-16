package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/paulgmiller/infestation/internal/fsutil"
)

// A fake ssh executable captures stdin/argv only; no network or compute runs.
func fakeSSH(t *testing.T, fail bool) (string, string) {
	t.Helper()
	root := t.TempDir()
	args, request := filepath.Join(root, "args"), filepath.Join(root, "request")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SSH_TEST_ARGS\"\n/bin/cat > \"$SSH_TEST_REQUEST\"\n"
	if fail {
		script += "echo tskey-auth-SECRET-FAILURE >&2\nexit 1\n"
	}
	if err := os.WriteFile(filepath.Join(root, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root)
	t.Setenv("SSH_TEST_ARGS", args)
	t.Setenv("SSH_TEST_REQUEST", request)
	return args, request
}
func TestSSHCreateTransfersOnlyProfilesAndPersistsRouting(t *testing.T) {
	r := preparedProviderFixture(t, "qemu")
	r.SSHTarget = "vm-host"
	r.SSHStateDir = "/remote/vms"
	// Secret workstation artifacts must never be serialized into the RPC.
	if err := fsutil.WriteFile(filepath.Join(r.StateDir, "pki", "ca.key"), []byte("CA-PRIVATE-MARKER"), 0600); err != nil {
		t.Fatal(err)
	}
	argsPath, requestPath := fakeSSH(t, false)
	if err := (qemuCompute{}).Create(context.Background(), r, io.Discard); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(argsPath)
	if strings.Contains(string(args), "CA-PRIVATE-MARKER") || !strings.Contains(string(args), "vm-host\nif command -v infestation") {
		t.Fatal("unexpected SSH arguments")
	}
	raw, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	var request sshRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "CA-PRIVATE-MARKER") || len(request.Profiles) != 2 || request.Operation != "create" {
		t.Fatal("unexpected private SSH request")
	}
	var link sshCompute
	if err := fsutil.ReadJSON(filepath.Join(r.StateDir, "ssh.json"), &link); err != nil {
		t.Fatal(err)
	}
	if link.Target != r.SSHTarget || link.Owner != request.Owner || link.StateDir != r.SSHStateDir {
		t.Fatal("routing/ownership not persisted")
	}
	// Routing is automatic on later lifecycle operations.
	o := Options{Name: r.Name, StateDir: filepath.Dir(r.StateDir), AllowOutage: true}
	for _, action := range []string{"start", "status", "stop", "netcheck", "remove"} {
		handled, err := remoteAction(context.Background(), o, action, RemovalOptions{}, io.Discard)
		if !handled || err != nil {
			t.Fatal(action, handled, err)
		}
		raw, _ := os.ReadFile(requestPath)
		_ = json.Unmarshal(raw, &request)
		if request.Operation != action || request.Owner != link.Owner || request.Options.StateDir != link.StateDir || !request.Options.AllowOutage {
			t.Fatal("lifecycle lost routing or ownership")
		}
	}
}
func TestSSHFailuresRetainOwnershipAndDoNotExposeSecrets(t *testing.T) {
	r := preparedProviderFixture(t, "qemu")
	r.SSHTarget = "vm-host"
	fakeSSH(t, true)
	err := createRemote(context.Background(), r, io.Discard)
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("SSH error leaked private output", err)
	}
	if _, err := os.Stat(filepath.Join(r.StateDir, "ssh.json")); err != nil {
		t.Fatal("failed create lost ownership")
	}
	if err := createRemote(context.Background(), r, io.Discard); err == nil {
		t.Fatal("retry overwrote ownership")
	}
}
func TestSSHRejectsDestinationsAndOwnershipBeforeMutation(t *testing.T) {
	for _, target := range []string{"-oProxyCommand=bad", "host; command", "$(command)", "host\ncommand"} {
		if err := sshCall(context.Background(), target, sshRequest{}, io.Discard); err == nil {
			t.Fatal("unsafe SSH destination accepted")
		}
	}
	o, dir := removalState(t, "qemu")
	if err := fsutil.WriteJSON(filepath.Join(dir, "ssh-owner.json"), uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"start", "stop", "remove", "status"} {
		raw, _ := json.Marshal(sshRequest{Operation: operation, Owner: uuid.NewString(), Options: o})
		if err := ServeSSH(context.Background(), bytes.NewReader(raw), io.Discard); err == nil || !strings.Contains(err.Error(), "ownership mismatch") {
			t.Fatal("unowned remote action accepted", operation, err)
		}
	}
	for _, raw := range []string{`{"Operation":"start","unknown":"private"}`, `{} {}`, `{"Operation":"preflight","Options":{"StateDir":"relative"}}`} {
		if err := ServeSSH(context.Background(), strings.NewReader(raw), io.Discard); err == nil {
			t.Fatal("invalid RPC accepted")
		}
	}
}
