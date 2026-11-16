package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

func TestConcurrentCreatesFinishBeforeFailureCleanup(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"kubectl", "tailscale"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("unused"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", root)
	t.Setenv("TAILSCALE_AUTH_KEY", "tskey-auth-TEST")
	o := Options{Name: "check", StateDir: filepath.Join(root, "run"), Domain: "example.ts.net", Accel: "tcg", Timeout: time.Minute, SSH: "compute"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	creates, stops := 0, 0
	entered := make(chan struct{}, 2)
	var both sync.WaitGroup
	both.Add(2)
	allDone := make(chan struct{})
	go func() { both.Wait(); close(allDone) }()
	invoke := func(ctx context.Context, args []string, out io.Writer) error {
		if args[1] == "create" {
			mu.Lock()
			creates++
			mu.Unlock()
			var name, size string
			hasInit, hasSSH := false, false
			for i, arg := range args {
				if arg == "--init" {
					hasInit = true
				}
				if arg == "--ssh" {
					hasSSH = args[i+1] == "compute"
				}
				if arg == "--name" {
					name = args[i+1]
				}
				if arg == "--size" {
					size = args[i+1]
				}
			}
			if hasInit != (name == "checks") || hasSSH != (name == "checkj") {
				return fmt.Errorf("wrong initializer or remote target")
			}
			if (name == "checks" && size != "2") || (name == "checkj" && size != "1") {
				return fmt.Errorf("wrong group capacity")
			}
			dir := filepath.Join(o.StateDir, name)
			if err := os.Mkdir(dir, 0700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(dir, "nodes.json"), []byte("{}"), 0600); err != nil {
				return err
			}
			entered <- struct{}{}
			// Neither create may finish until the other has started. This fails a
			// sequential implementation, and concurrent writes exercise progress safety.
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				return fmt.Errorf("other create did not start")
			}
			fmt.Fprintln(out, "Creating", name)
			both.Done()
			select {
			case <-allDone:
			case <-time.After(3 * time.Second):
				return fmt.Errorf("creates did not overlap")
			}
			cancel()
			return context.Canceled
		}
		if args[1] != "stop" {
			return fmt.Errorf("unexpected lifecycle action")
		}
		select {
		case <-allDone:
		default:
			return fmt.Errorf("cleanup raced unfinished creates")
		}
		mu.Lock()
		stops++
		mu.Unlock()
		if ctx.Err() != nil {
			return fmt.Errorf("cleanup inherited canceled context")
		}
		if args[len(args)-1] != "--allow-outage" {
			return fmt.Errorf("cleanup lost shutdown option")
		}
		return io.ErrUnexpectedEOF
	}

	var out bytes.Buffer
	err := runLifecycle(ctx, o, &out, invoke)
	if err == nil || creates != 2 || stops != 2 || !strings.Contains(err.Error(), "shutdown") || !errors.Is(err, context.Canceled) {
		t.Fatal("incomplete creation was not cleaned up", err, creates, stops)
	}
	if strings.Contains(out.String(), "PASS:") {
		t.Fatal("cleanup failure reported success")
	}
	if _, err := os.Stat(filepath.Join(o.StateDir, "checks", "nodes.json")); err != nil {
		t.Fatal("cleanup removed ownership state")
	}
}
