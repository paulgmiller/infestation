package vm

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestInstanceLifecycleDispatchPreservesInventoryAndDisks(t *testing.T) {
	o, dir := removalState(t, "qemu")
	original, err := os.ReadFile(filepath.Join(dir, "nodes.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		instance string
		want     []string
	}{{"", []string{"pool-0", "pool-1"}}, {"0", []string{"pool-0"}}, {"1", []string{"pool-1"}}} {
		for _, action := range []string{"start", "stop"} {
			var calls []string
			fake := func(_ context.Context, node Options, _ io.Writer) error {
				if node.StateDir != dir {
					t.Fatal("instance lost group ownership path")
				}
				calls = append(calls, node.Name)
				return nil
			}
			o.Instance = test.instance
			if err := clusterAction(context.Background(), o, action, io.Discard, fake, fake); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(calls, test.want) {
				t.Fatal("operated unintended instances", action, test.instance, calls)
			}
		}
	}
	for _, instance := range []string{"-1", "2", "unknown", "../pool-0"} {
		o.Instance = instance
		fake := func(context.Context, Options, io.Writer) error {
			t.Fatal("invalid selector reached compute")
			return nil
		}
		if err := clusterAction(context.Background(), o, "stop", io.Discard, fake, fake); err == nil {
			t.Fatal("invalid instance accepted")
		}
	}
	after, err := os.ReadFile(filepath.Join(dir, "nodes.json"))
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("instance selection rewrote ownership inventory")
	}
	for _, node := range []string{"pool-0", "pool-1"} {
		raw, err := os.ReadFile(filepath.Join(dir, node, "disk.qcow2"))
		if err != nil || string(raw) != "private-data" {
			t.Fatal("instance dispatch changed disk")
		}
	}
	o.Instance = "0"
	var output bytes.Buffer
	if err := (qemuCompute{}).Stop(context.Background(), o, &output); err != nil || output.String() != "pool-0 already stopped; disk retained\n" {
		t.Fatalf("stop did not reconcile the selected stopped VM: %v, %s", err, &output)
	}
	if err := RemoveGroup(context.Background(), o, "qemu", RemovalOptions{AbandonCluster: true}, io.Discard); err == nil {
		t.Fatal("instance selector silently removed whole group")
	}
	o.Name = ""
	if err := StopAllLocal(context.Background(), o, io.Discard); err == nil {
		t.Fatal("instance selector accepted for host-wide stop")
	}
}
