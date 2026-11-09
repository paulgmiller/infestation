package vm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	config "github.com/coreos/ignition/v2/config/v3_3"
	"infestation/internal/fsutil"
	"infestation/internal/tailnet"
)

func TestProfileKeepsSecretsPrivateAndNetworkingOutboundOnly(t *testing.T) {
	key := "tskey-auth-PRIVATE-TEST"
	raw, err := Profile("friend-node", key)
	if err != nil {
		t.Fatal(err)
	}
	doc, report, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err, report)
	}
	found := false
	for _, f := range doc.Storage.Files {
		if f.Contents.Source == nil {
			continue
		}
		src, ok := strings.CutPrefix(*f.Contents.Source, "data:;base64,")
		if !ok {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(src)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), key) {
			found = true
			if f.Path != tailnet.AuthPath || f.Mode == nil || *f.Mode != 0600 {
				t.Fatal("credential escaped root-only enrollment file")
			}
		}
		if f.Path == "/opt/infestation/enroll" {
			for _, s := range []string{"--auth-key=file:", "--hostname=friend-node", "tailscale ip -4", "rm -f /etc/infestation/tailscale-authkey", "--peers=false"} {
				if !strings.Contains(string(data), s) {
					t.Fatal("missing enrollment lifecycle guard", s)
				}
			}
		}
	}
	if !found {
		t.Fatal("key missing")
	}
	args := strings.Join(arguments("/private/node", machine{Name: "friend-node", Accel: "tcg"}), " ")
	if !strings.Contains(args, "user,id=egress") || strings.Contains(args, "hostfwd") || strings.Contains(args, key) || strings.Contains(args, "tap") {
		t.Fatal("network or secret in QEMU args", args)
	}
	if strings.Contains(string(raw), "k3s") || strings.Contains(string(raw), "cockroach") {
		t.Fatal("enrollment-only VM unexpectedly starts cluster components")
	}
}

func TestKeySourcesAndSanitizedErrors(t *testing.T) {
	t.Setenv("TAILSCALE_AUTH_KEY", "tskey-auth-ENV-SECRET")
	key, err := authKey(Options{})
	if err != nil || key != "tskey-auth-ENV-SECRET" {
		t.Fatal("env fallback failed")
	}
	key, err = authKey(Options{AuthKey: "tskey-auth-ARG-SECRET"})
	if err != nil || key != "tskey-auth-ARG-SECRET" {
		t.Fatal("explicit arg did not override env")
	}
	path := filepath.Join(t.TempDir(), "auth")
	if err = os.WriteFile(path, []byte("tskey-auth-FILE-SECRET\n"), 0600); err != nil {
		t.Fatal(err)
	}
	key, err = authKey(Options{AuthKeyFile: path})
	if err != nil || key != "tskey-auth-FILE-SECRET" {
		t.Fatal("file did not override env")
	}
	if _, err = authKey(Options{AuthKey: "tskey-auth-SECRET", AuthKeyFile: path}); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("ambiguous sources accepted or exposed")
	}
	t.Setenv("TAILSCALE_AUTH_KEY", "invalid-SECRET")
	if _, err = authKey(Options{}); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("invalid key accepted or exposed")
	}
}

func TestExistingVMAndUnsafePathsAreNeverOverwritten(t *testing.T) {
	for _, name := range []string{"../escape", "bad,name", "BadName", ""} {
		if _, err := directory(Options{Name: name, StateDir: t.TempDir()}); err == nil {
			t.Fatal("unsafe name", name)
		}
	}
	o := Options{Name: "friend-node", StateDir: t.TempDir(), Accel: "tcg", AuthKey: "tskey-auth-TEST"}
	dir, _ := directory(o)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(dir, "disk.qcow2")
	if err := os.WriteFile(sentinel, []byte("existing state"), 0600); err != nil {
		t.Fatal(err)
	}
	// Host preflight may fail when QEMU isn't installed; neither outcome may
	// mutate or truncate an existing VM disk or identity.
	if err := Create(context.Background(), o, io.Discard); err == nil {
		t.Fatal("existing VM accepted")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "existing state" {
		t.Fatal("existing state changed")
	}
}

func TestEnrollmentRequiresCompleteRunningTaggedIdentity(t *testing.T) {
	dir := t.TempDir()
	good := tailnet.Status{BackendState: "Running", TailscaleIPs: []string{"100.100.1.2"}}
	good.Self.HostName = "friend-node"
	good.Self.DNSName = "friend-node.example.ts.net."
	good.Self.Tags = []string{"tag:lab"}
	for _, change := range []func(*tailnet.Status){
		func(s *tailnet.Status) { s.BackendState = "NeedsLogin" },
		func(s *tailnet.Status) { s.Self.Tags = nil },
		func(s *tailnet.Status) { s.Self.HostName = "another-node" },
		func(s *tailnet.Status) { s.TailscaleIPs = nil },
	} {
		s := good
		change(&s)
		if err := fsutil.WriteJSON(filepath.Join(dir, "enrollment.json"), s); err != nil {
			t.Fatal(err)
		}
		if _, err := enrollment(dir, "friend-node"); err == nil {
			t.Fatal("invalid enrollment report accepted")
		}
	}
	if err := fsutil.WriteJSON(filepath.Join(dir, "enrollment.json"), good); err != nil {
		t.Fatal(err)
	}
	if _, err := enrollment(dir, "friend-node"); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(good)
	if err := os.WriteFile(filepath.Join(dir, "enrollment.json"), raw[:len(raw)/2], 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := enrollment(dir, "friend-node"); err == nil {
		t.Fatal("partial guest report accepted")
	}
}
