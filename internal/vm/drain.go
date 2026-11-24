package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/paulgmiller/infestation/internal/executil"
)

const stopParallelism = 4

type stopTarget struct {
	name, databaseHost string
	prepare            func(context.Context) (bool, error) // true when already off
	power              func(context.Context) error
	close              func()
}

func drainDatabase(ctx context.Context, runner executil.Runner, host string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	result := runner.Run(ctx, "tailscale", "ssh", "core@"+host, "sudo", "-n",
		"/usr/bin/docker", "exec", "--user", "0", "cockroachdb", "/cockroach/cockroach",
		"node", "drain", "--self", "--host=localhost:26257", "--certs-dir=/cockroach/certs",
		"--user=node", "--drain-wait=90s")
	if err := ctx.Err(); err != nil {
		return err
	}
	if result.Code != 0 {
		return fmt.Errorf("CockroachDB drain failed (SSH/command exit %d)", result.Code)
	}
	if !slices.Contains(strings.Split(strings.ReplaceAll(result.Stdout, "\r\n", "\n"), "\n"), "drain ok") {
		return fmt.Errorf("CockroachDB did not confirm drain completion")
	}
	return nil
}

func runStopBatch(ctx context.Context, targets []stopTarget, allowOutage bool, out io.Writer, runner executil.Runner) error {
	type outcome struct {
		off                            bool
		inspectErr, drainErr, powerErr error
	}
	results := make([]outcome, len(targets))
	defer func() {
		for _, target := range targets {
			if target.close != nil {
				target.close()
			}
		}
	}()
	if allowOutage {
		fmt.Fprintln(out, "Outage permitted: power-off may proceed after an unconfirmed database drain.")
	}
	parallelStopPhase(len(targets), func(i int) {
		target, result := targets[i], &results[i]
		if err := ctx.Err(); err != nil {
			result.inspectErr = err
			return
		}
		result.off, result.inspectErr = target.prepare(ctx)
		if result.inspectErr == nil && !result.off && target.databaseHost != "" {
			result.drainErr = drainDatabase(ctx, runner, target.databaseHost)
		}
	})
	parallelStopPhase(len(targets), func(i int) {
		target, result := targets[i], &results[i]
		if result.off || result.inspectErr != nil || (result.drainErr != nil && !allowOutage) {
			return
		}
		result.powerErr = ctx.Err()
		if result.powerErr == nil {
			result.powerErr = target.power(ctx)
		}
	})

	var failures []error
	for i, target := range targets {
		result := results[i]
		switch {
		case result.inspectErr != nil:
			failures = append(failures, fmt.Errorf("%s: power-off skipped: %w", target.name, result.inspectErr))
		case result.off:
			fmt.Fprintf(out, "%s already stopped; disk retained\n", target.name)
		case result.drainErr != nil && !allowOutage:
			failures = append(failures, fmt.Errorf("%s: power-off skipped; database may be partially drained: %w", target.name, result.drainErr))
		default:
			drainState := "database drain confirmed"
			if target.databaseHost == "" {
				drainState = "no managed database"
			} else if result.drainErr != nil {
				drainState = fmt.Sprintf("database drain unconfirmed (%v)", result.drainErr)
			}
			if result.powerErr != nil {
				failures = append(failures, fmt.Errorf("%s: power-off unconfirmed; %s: %w", target.name, drainState, result.powerErr))
			} else {
				fmt.Fprintf(out, "%s stopped; disk retained; %s\n", target.name, drainState)
			}
		}
	}
	return errors.Join(failures...)
}

func parallelStopPhase(count int, action func(int)) {
	var workers sync.WaitGroup
	limit := make(chan struct{}, stopParallelism)
	for i := range count {
		limit <- struct{}{}
		workers.Go(func() {
			defer func() { <-limit }()
			action(i)
		})
	}
	workers.Wait()
}

func qemuStopTarget(o Options, host string) stopTarget {
	var dir string
	var lock *os.File
	return stopTarget{
		name: o.Name, databaseHost: host,
		prepare: func(ctx context.Context) (bool, error) {
			var err error
			dir, _, err = load(o)
			if err != nil {
				return false, err
			}
			lock, err = lockMachine(dir)
			if err != nil {
				return false, err
			}
			present, err := inspectQEMUStop(ctx, dir, o.Name)
			if err != nil {
				return false, err
			}
			if !present {
				return true, setResume(dir, false)
			}
			return false, nil
		},
		power: func(ctx context.Context) error { return stopQEMULocked(ctx, dir, o.Name, io.Discard) },
		close: func() {
			if lock != nil {
				lock.Close()
			}
		},
	}
}

func stopQEMUGroup(ctx context.Context, o Options, out io.Writer) error {
	dir, config, err := loadCluster(o)
	if err != nil {
		return err
	}
	provider, err := groupProvider(dir)
	if err != nil {
		return err
	}
	if provider != "qemu" {
		return fmt.Errorf("use azure stop for Azure-owned nodes")
	}
	nodes, err := selectedQEMUNodes(config, o.Instance)
	if err != nil {
		return err
	}
	targets := make([]stopTarget, 0, len(nodes))
	for _, node := range nodes {
		var host string
		if node.Database() {
			host = config.NodeAddress(node)
		}
		targets = append(targets, qemuStopTarget(Options{Name: node.Name, StateDir: dir}, host))
	}
	return runStopBatch(ctx, targets, o.AllowOutage, out, executil.OSRunner{})
}
