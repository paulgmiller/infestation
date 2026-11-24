package vm

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/paulgmiller/infestation/internal/fsutil"
)

func TestStopAllDiscoversOnlyLocalOwnedMachines(t *testing.T) {
	o, dir := removalState(t, "qemu")
	// Add a standalone VM alongside the named group.
	if err := fsutil.WriteJSON(filepath.Join(o.StateDir, "standalone", "machine.json"), machine{Name: "standalone"}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"remote", "cloud"} {
		if err := fsutil.WriteJSON(filepath.Join(o.StateDir, name, "provider.json"), "azure"); err != nil {
			t.Fatal(err)
		}
	}
	if err := fsutil.WriteJSON(filepath.Join(o.StateDir, "remote", "provider.json"), "qemu"); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteJSON(filepath.Join(o.StateDir, "remote", "ssh.json"), sshCompute{Target: "never-contact"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(o.StateDir, "alias")); err != nil {
		t.Fatal(err)
	}
	var names []string
	collect := func(_ context.Context, machines []localStopMachine, _ io.Writer) error {
		for _, machine := range machines {
			names = append(names, machine.Name)
		}
		return nil
	}
	o.Name = ""
	o.AllowOutage = true
	err := stopAllLocal(context.Background(), o, io.Discard, collect)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, []string{"pool-0", "pool-1", "standalone"}) {
		t.Fatal("wrong local targets", names)
	}
	// An invalid later machine prevents any earlier VM from being stopped.
	if err := fsutil.WriteJSON(filepath.Join(o.StateDir, "standalone", "machine.json"), machine{Name: "someone-else"}); err != nil {
		t.Fatal(err)
	}
	names = nil
	if err := stopAllLocal(context.Background(), o, io.Discard, collect); err == nil || len(names) != 0 {
		t.Fatal("invalid inventory caused partial shutdown", err, names)
	}
}

func TestStopAllRequiresExplicitOutageAndAttemptsEveryMachine(t *testing.T) {
	o, _ := removalState(t, "qemu")
	o.Name = ""
	calls := 0
	stop := func(_ context.Context, machines []localStopMachine, _ io.Writer) error {
		calls += len(machines)
		return errors.New("shutdown failed")
	}
	if err := stopAllLocal(context.Background(), o, io.Discard, stop); err == nil || calls != 0 {
		t.Fatal("outage guard bypassed")
	}
	o.AllowOutage = true
	if err := stopAllLocal(context.Background(), o, io.Discard, stop); err == nil || calls != 2 {
		t.Fatal("did not attempt every VM", err, calls)
	}
}
