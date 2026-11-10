package vm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// Explicit opt-in only: this boots a real VM, contacts the Tailscale control
// plane and creates a tailnet device. Never run as part of normal unit tests.
func TestLiveEnrollmentAndRestart(t *testing.T) {
	if os.Getenv("INFESTATION_VM_TEST") != "1" {
		t.Skip("set INFESTATION_VM_TEST=1 and TAILSCALE_AUTH_KEY to run real enrollment")
	}
	if os.Getenv("TAILSCALE_AUTH_KEY") == "" {
		t.Fatal("TAILSCALE_AUTH_KEY is required")
	}
	state, err := os.MkdirTemp("", "inf-vm-")
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately retain private artifacts for diagnosis and recovery. Never
	// auto-remove a potentially live VM's disk, even when the test fails.
	t.Log("Private VM state retained at", state)
	accel := os.Getenv("INFESTATION_VM_ACCEL")
	if accel == "" {
		accel = "kvm"
	}
	o := Options{Name: fmt.Sprintf("inf-enroll-%d", time.Now().Unix()), StateDir: state, Accel: accel, Wait: 10 * time.Minute, BaseImage: os.Getenv("INFESTATION_VM_BASE_IMAGE")}
	ctx, cancel := context.WithTimeout(context.Background(), 24*time.Minute)
	defer cancel()
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		if _, _, err := load(o); err == nil {
			_ = Stop(cleanup, o, io.Discard)
		}
	}()
	if err = Create(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	dir, _, err := load(o)
	if err != nil {
		t.Fatal(err)
	}
	before, err := enrollment(dir, o.Name)
	if err != nil {
		t.Fatal(err)
	}
	if before.Self.ID == "" {
		t.Fatal("guest reported no stable node ID")
	}
	t.Log("Initial running, tagged enrollment confirmed; restarting the VM")
	disk, err := os.Stat(filepath.Join(dir, "disk.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	if err = Stop(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	until := time.NewTimer(2 * time.Minute)
	defer until.Stop()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
shutdown:
	for {
		if _, err := os.Lstat(filepath.Join(dir, "qmp.sock")); os.IsNotExist(err) {
			break shutdown
		}
		select {
		case <-until.C:
			t.Fatal("VM did not shut down")
		case <-tick.C:
		}
	}
	if err = Start(ctx, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	after, err := enrollment(dir, o.Name)
	if err != nil {
		t.Fatal(err)
	}
	nextDisk, err := os.Stat(filepath.Join(dir, "disk.qcow2"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(disk, nextDisk) {
		t.Fatal("restart replaced VM disk")
	}
	if before.Self.ID != after.Self.ID || before.Self.DNSName != after.Self.DNSName || !reflect.DeepEqual(before.TailscaleIPs, after.TailscaleIPs) {
		t.Fatal("restart did not preserve enrolled identity")
	}
	t.Log("Verified real tagged enrollment and persistent identity across graceful restart")
}
