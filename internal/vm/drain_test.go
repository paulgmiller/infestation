package vm

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/paulgmiller/infestation/internal/executil"
)

type drainTestRunner func(context.Context, string, ...string) executil.Result

func (r drainTestRunner) Run(ctx context.Context, name string, args ...string) executil.Result {
	return r(ctx, name, args...)
}

func TestStopBatchParallelDrainsAndPartialPowerOff(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow-outage=%t", override), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var entered, completed atomic.Int32
			bothEntered := make(chan struct{})
			runner := drainTestRunner(func(ctx context.Context, name string, args ...string) executil.Result {
				want := []string{"ssh", args[1], "sudo", "-n", "/usr/bin/docker", "exec", "--user", "0",
					"cockroachdb", "/cockroach/cockroach", "node", "drain", "--self",
					"--host=localhost:26257", "--certs-dir=/cockroach/certs", "--user=node", "--drain-wait=90s"}
				if name != "tailscale" || !reflect.DeepEqual(args, want) {
					t.Errorf("unexpected drain command: %s %v", name, args)
				}
				if entered.Add(1) == 2 {
					close(bothEntered)
				}
				select {
				case <-bothEntered:
				case <-ctx.Done():
					t.Error("drains did not overlap")
					return executil.Result{Code: 1}
				}
				completed.Add(1)
				if args[1] == "core@bad.example.ts.net" {
					return executil.Result{Code: 1, Stderr: "private output"}
				}
				return executil.Result{Stdout: "drain ok\n"}
			})
			powered := make([]bool, 3)
			var targets []stopTarget
			for i, name := range []string{"good", "bad", "off"} {
				targets = append(targets, stopTarget{
					name: name, databaseHost: name + ".example.ts.net",
					prepare: func(context.Context) (bool, error) { return name == "off", nil },
					power: func(context.Context) error {
						if completed.Load() != 2 {
							t.Error("power-off started before both drains completed")
						}
						powered[i] = true
						return nil
					},
				})
			}
			var out bytes.Buffer
			err := runStopBatch(ctx, targets, override, &out, runner)
			if !reflect.DeepEqual(powered, []bool{true, override, false}) || (err == nil) != override {
				t.Fatalf("powered = %v, err = %v", powered, err)
			}
			if !strings.Contains(out.String(), "off already stopped") || !strings.Contains(out.String(), "good stopped") {
				t.Fatalf("missing results: %s", &out)
			}
			if override && !strings.Contains(out.String(), "database drain unconfirmed") {
				t.Fatalf("missing overridden drain result: %s", &out)
			}
			if strings.Contains(fmt.Sprint(err, &out), "private output") {
				t.Fatal("exposed raw drain output")
			}
		})
	}
}

func TestStopBatchPowerOffOverlapsAndReportsFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var started atomic.Int32
	bothStarted := make(chan struct{})
	var targets []stopTarget
	for _, name := range []string{"first", "second"} {
		targets = append(targets, stopTarget{
			name: name, prepare: func(context.Context) (bool, error) { return false, nil },
			power: func(ctx context.Context) error {
				if started.Add(1) == 2 {
					close(bothStarted)
				}
				select {
				case <-bothStarted:
				case <-ctx.Done():
					t.Error("power-offs did not overlap")
					return ctx.Err()
				}
				if name == "first" {
					return fmt.Errorf("provider rejected request")
				}
				return nil
			},
		})
	}
	var out bytes.Buffer
	err := runStopBatch(ctx, targets, false, &out, nil)
	if err == nil || !strings.Contains(err.Error(), "first: power-off unconfirmed") ||
		!strings.Contains(out.String(), "second stopped") {
		t.Fatalf("output = %q, err = %v", &out, err)
	}
}

type azureStopFake struct {
	azureFake
	instances []azureInstance
	powered   chan string
}

func (f *azureStopFake) Instances(context.Context, azureScaleSet) ([]azureInstance, error) {
	return f.instances, nil
}

func (f *azureStopFake) DeallocateInstance(_ context.Context, _ azureScaleSet, id string) error {
	f.powered <- id
	return nil
}

func TestAzureStopUsesOwnedInstancesAndSkipsDeallocated(t *testing.T) {
	o, _ := removalState(t, "azure")
	_, set, err := loadAzureSet(o)
	if err != nil {
		t.Fatal(err)
	}
	client := &azureStopFake{
		instances: []azureInstance{
			{InstanceID: "1", Statuses: []azureStatus{{Code: "PowerState/running"}}},
			{InstanceID: "0", Statuses: []azureStatus{{Code: "PowerState/deallocated"}}},
		},
		powered: make(chan string, 2),
	}
	var drains atomic.Int32
	runner := drainTestRunner(func(_ context.Context, _ string, args ...string) executil.Result {
		drains.Add(1)
		if !strings.HasPrefix(args[1], "core@pool-1.") {
			t.Errorf("wrong drain target: %v", args)
		}
		return executil.Result{Stdout: "drain ok\n"}
	})
	if err := stopAzureGroup(context.Background(), o, set, io.Discard, client, runner); err != nil {
		t.Fatal(err)
	}
	if drains.Load() != 1 || len(client.powered) != 1 || <-client.powered != "1" {
		t.Fatal("stop operated on an unexpected instance")
	}
	client.instances[1].InstanceID = "7"
	o.AllowOutage = true
	if err := stopAzureGroup(context.Background(), o, set, io.Discard, client, runner); err == nil ||
		drains.Load() != 1 || len(client.powered) != 0 {
		t.Fatal("mismatched inventory allowed guest or provider actions", err)
	}
}
