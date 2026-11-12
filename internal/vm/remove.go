package vm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/fsutil"
)

// RemoveGroup removes only compute owned by this saved provider group. Local
// credentials and inventory survive both successful removal and failed retries.
func RemoveGroup(ctx context.Context, o Options, provider string, deleteDisks bool, out io.Writer) error {
	if provider == "azure" {
		return removeAzureGroup(ctx, o, out, executil.OSRunner{})
	}
	if provider != "qemu" {
		return fmt.Errorf("unknown compute provider")
	}
	if runtime.GOOS != "linux" {
		return fmt.Errorf("QEMU removal requires the original Linux host")
	}
	dir, c, err := loadCluster(o)
	if err != nil {
		return err
	}
	actual, err := groupProvider(dir)
	if err != nil {
		return err
	}
	if actual != provider {
		return fmt.Errorf("provider ownership mismatch; use %s remove", actual)
	}
	// Check all recorded identities before changing any machine.
	for _, n := range c.Nodes {
		node := Options{Name: n.Name, StateDir: dir}
		nodeDir, err := directory(node)
		if err != nil {
			return err
		}
		info, err := os.Lstat(nodeDir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing non-directory VM state: %s", n.Name)
		}
		if _, _, err := load(node); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			// Creation may fail after reserving the directory or disk but
			// before saving machine.json. No process may have started then.
			for _, file := range []string{"qemu.pid", "qmp.sock"} {
				if _, err := os.Lstat(filepath.Join(nodeDir, file)); !os.IsNotExist(err) {
					return fmt.Errorf("incomplete VM state has control files; preserving %s", n.Name)
				}
			}
		}
	}
	for _, n := range c.Nodes {
		if err := removeMachine(ctx, Options{Name: n.Name, StateDir: dir}, deleteDisks); err != nil {
			return fmt.Errorf("%s: %w", n.Name, err)
		}
		fmt.Fprintln(out, n.Name, "QEMU resources removed")
	}
	fmt.Fprintln(out, "Private state retained:", dir)
	if !deleteDisks {
		fmt.Fprintln(out, "VM disks retained; use --delete-disks to permanently delete overlays.")
	}
	return nil
}

func removeAzureGroup(ctx context.Context, o Options, out io.Writer, runner executil.Runner) error {
	dir, _, err := loadCluster(o)
	if err != nil {
		return err
	}
	// Require explicit provider metadata for destructive operations. Legacy mixed
	// inventories are intentionally read-only rather than guessed from filenames.
	var provider string
	if err := fsutil.ReadJSON(filepath.Join(dir, "provider.json"), &provider); err != nil {
		return err
	}
	if provider != "azure" {
		return fmt.Errorf("Azure provider ownership mismatch")
	}
	var set azureScaleSet
	if err := fsutil.ReadJSON(filepath.Join(dir, "azure-scaleset.json"), &set); err != nil {
		return err
	}
	if set.Name != o.Name || set.Group != o.Name+"-rg" || set.Ownership != o.Name || set.Subscription == "" || set.Capacity < 1 {
		return fmt.Errorf("Azure scale-set ownership mismatch")
	}
	exists := runner.Run(ctx, "az", "group", "exists", "--subscription", set.Subscription, "--name", set.Group, "-o", "tsv")
	if err := exists.Err("check Azure resource group"); err != nil {
		return err
	}
	switch strings.TrimSpace(exists.Stdout) {
	case "false":
		fmt.Fprintln(out, set.Group, "already removed; local state retained")
		return nil
	case "true":
	default:
		return fmt.Errorf("unexpected Azure resource-group existence response")
	}
	owner := runner.Run(ctx, "az", "group", "show", "--subscription", set.Subscription, "--name", set.Group, "--query", "tags.\"infestation-mixed-id\"", "-o", "tsv")
	if err := owner.Err("verify Azure resource-group ownership"); err != nil {
		return err
	}
	if strings.TrimSpace(owner.Stdout) != set.Ownership {
		return fmt.Errorf("Azure resource-group ownership mismatch")
	}
	result := runner.Run(ctx, "az", "group", "delete", "--subscription", set.Subscription, "--name", set.Group, "--yes")
	if err := result.Err("remove Azure resource group"); err != nil {
		return err
	}
	fmt.Fprintln(out, set.Group, "removed, including compute, disks and networking; local state retained")
	return nil
}

// processPresent verifies the saved PID's actual QEMU arguments; it never signals
// a process by PID. Unknown live processes prevent cleanup of control files.
func processPresent(dir string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "qemu.pid"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return false, fmt.Errorf("invalid QEMU PID file")
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(cmdline) == 0 {
		return false, nil
	} // exited/zombie process
	args := strings.Split(string(cmdline), "\x00")
	has := func(value string) bool {
		for _, arg := range args {
			if arg == value {
				return true
			}
		}
		return false
	}
	if !strings.HasPrefix(filepath.Base(args[0]), "qemu-system-") || !has("unix:"+filepath.Join(dir, "qmp.sock")+",server=on,wait=off") || !has("file="+filepath.Join(dir, "disk.qcow2")+",format=qcow2,if=virtio") {
		return false, fmt.Errorf("saved PID belongs to an unverified process; preserving VM resources")
	}
	return true, nil
}

func removeMachine(ctx context.Context, o Options, deleteDisks bool) error {
	dir, err := directory(o)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	lockPath := filepath.Join(dir, "start.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("another start/removal may be in progress: %w", err)
	}
	lock.Close()
	defer os.Remove(lockPath)
	present, err := processPresent(dir)
	if err != nil {
		return err
	}
	raw, monitorErr := qmp(ctx, dir, "query-name")
	if monitorErr == nil {
		var identity struct{ Name string }
		if json.Unmarshal(raw, &identity) != nil || identity.Name != o.Name {
			return fmt.Errorf("QEMU monitor identity mismatch")
		}
		if !present {
			return fmt.Errorf("QEMU is running without a verified PID; preserving resources")
		}
		if _, err := qmp(ctx, dir, "quit"); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	} else if present {
		return fmt.Errorf("QEMU process exists but monitor is unavailable: %w", monitorErr)
	} else if !errors.Is(monitorErr, os.ErrNotExist) && !errors.Is(monitorErr, syscall.ECONNREFUSED) {
		return fmt.Errorf("cannot verify stopped QEMU: %w", monitorErr)
	}
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		present, err := processPresent(dir)
		if err != nil {
			return err
		}
		if !present {
			break
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("waiting for QEMU exit; resources retained: %w", deadline.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
	// Do not unlink a monitor still accepting connections, even with a stale PID.
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "qmp.sock"))
	if err == nil {
		conn.Close()
		return fmt.Errorf("QEMU monitor still active; resources retained")
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ECONNREFUSED) {
		return err
	}
	files := []string{"qmp.sock", "qemu.pid"}
	if deleteDisks {
		files = append(files, "disk.qcow2")
	}
	for _, name := range files {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
