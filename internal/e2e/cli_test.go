package e2e

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCLIRequiresExplicitRunAndValidInputs(t *testing.T) {
	t.Setenv("TAILSCALE_AUTH_KEY", "tskey-auth-TEST")
	for _, args := range [][]string{nil, {"--help"}, {"run", "--help"}} {
		called := false
		cmd := newCommand(func(context.Context, Options, io.Writer) error { called = true; return nil })
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		if err := cmd.ExecuteContext(context.Background()); err != nil || called {
			t.Fatal("help invoked provisioning", err)
		}
	}
	for _, args := range [][]string{{"run"}, {"run", "--tailnet-domain", "bad"}, {"run", "--tailnet-domain", "example.ts.net", "--timeout=-1m"}, {"run", "--tailnet-domain", "example.ts.net", "--ssh-state-dir", "/remote"}} {
		called := false
		cmd := newCommand(func(context.Context, Options, io.Writer) error { called = true; return nil })
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		if err := cmd.ExecuteContext(context.Background()); err == nil || called {
			t.Fatal("invalid invocation reached provisioning", args, err)
		}
	}
	var got Options
	cmd := newCommand(func(_ context.Context, o Options, _ io.Writer) error { got = o; return nil })
	cmd.SetArgs([]string{"run", "--tailnet-domain", "example.ts.net", "--ssh", "compute", "--timeout", "10m"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got.SSH != "compute" || got.Timeout != 10*time.Minute || got.Accel != "kvm" {
		t.Fatal("flags not passed to runner")
	}
}

func TestFailureCleanupUsesFreshContextAndReportsFailures(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"kubectl", "tailscale"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("unused"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", root)
	t.Setenv("TAILSCALE_AUTH_KEY", "tskey-auth-TEST")
	o := Options{Name: "check", StateDir: filepath.Join(root, "run"), Domain: "example.ts.net", Accel: "tcg", Timeout: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	creates, stops := 0, 0
	invoke := func(ctx context.Context, args []string, _ io.Writer) error {
		if args[1] == "create" {
			creates++
			var name string
			for i, arg := range args[:len(args)-1] {
				if arg == "--name" {
					name = args[i+1]
				}
			}
			dir := filepath.Join(o.StateDir, name)
			if err := os.Mkdir(dir, 0700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, "nodes.json"), []byte("{}"), 0600); err != nil {
				return err
			}
			if creates == 2 {
				cancel()
				return context.Canceled
			}
			return nil
		}
		if args[1] != "stop" {
			t.Fatal("unexpected lifecycle action", args[1])
		}
		stops++
		if ctx.Err() != nil {
			t.Fatal("cleanup inherited canceled run context")
		}
		if args[len(args)-1] != "--allow-outage" {
			t.Fatal("cleanup lost explicit shutdown option")
		}
		return io.ErrUnexpectedEOF
	}
	var out bytes.Buffer
	err := runLifecycle(ctx, o, &out, invoke)
	if err == nil || creates != 2 || stops != 2 || !strings.Contains(err.Error(), "shutdown") {
		t.Fatal("incomplete creation was not cleaned up", err, creates, stops)
	}
	if strings.Contains(out.String(), "PASS:") {
		t.Fatal("cleanup failure reported success")
	}
	if _, err := os.Stat(filepath.Join(o.StateDir, "checks", "nodes.json")); err != nil {
		t.Fatal("cleanup removed ownership state")
	}
}
