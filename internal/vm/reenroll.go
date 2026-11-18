package vm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/paulgmiller/infestation/internal/tailnet"
)

// Reenroll restores tailnet access to running QEMU guests on their compute host.
func Reenroll(ctx context.Context, o Options, out io.Writer) error {
	dir, c, err := loadCluster(o)
	if err != nil {
		return err
	}
	provider, err := groupProvider(dir)
	if err != nil {
		return err
	}
	if provider != "qemu" {
		return fmt.Errorf("re-enrollment requires a QEMU group")
	}
	if _, err := os.Lstat(filepath.Join(dir, "ssh.json")); !os.IsNotExist(err) {
		if err != nil {
			return err
		}
		return fmt.Errorf("use the original QEMU host and its saved group state for re-enrollment")
	}
	nodes, err := selectedQEMUNodes(c, o.Instance)
	if err != nil {
		return err
	}
	key, err := authKey(o)
	if err != nil {
		return err
	}
	var result error
	for _, n := range nodes {
		if ctx.Err() != nil {
			return errors.Join(result, ctx.Err())
		}
		expected := n.Name + "." + strings.TrimSuffix(c.Tailscale.Domain, ".")
		if err := reenrollGuest(ctx, filepath.Join(dir, n.Name), n.Name, expected, key, c.Tailscale.SSH); err != nil {
			result = errors.Join(result, fmt.Errorf("%s: %w", n.Name, err))
		} else {
			fmt.Fprintln(out, n.Name, "connected to the expected tailnet")
		}
	}
	return result
}

func reenrollGuest(ctx context.Context, dir, name, expected, key string, ssh bool) (err error) {
	lockPath := filepath.Join(dir, "start.lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("acquire VM lifecycle lock: %w", err)
	}
	lock.Close()
	defer os.Remove(lockPath)
	present, err := processPresent(dir)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("QEMU is stopped")
	}
	raw, err := qmp(ctx, dir, "query-name")
	if err != nil {
		return err
	}
	var identity struct{ Name string }
	if json.Unmarshal(raw, &identity) != nil || identity.Name != name {
		return fmt.Errorf("QEMU monitor identity mismatch")
	}

	const chardev, port = "infest-reenroll", "infest-reenroll-port"
	// QEMU creates the socket inside an owner-only directory, independently of umask.
	socketDir, err := os.MkdirTemp("/tmp", "infestation-ga-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(socketDir)
	path := filepath.Join(socketDir, "sock")
	_, err = qmp(ctx, dir, "chardev-add", map[string]any{
		"id": chardev,
		"backend": map[string]any{"type": "socket", "data": map[string]any{
			"addr":   map[string]any{"type": "unix", "data": map[string]any{"path": path}},
			"server": true, "wait": false,
		}},
	})
	if err != nil {
		return err
	}
	attached := false
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
		defer cancel()
		if attached {
			if _, e := qmp(cleanup, dir, "device_del", map[string]any{"id": port}); e != nil {
				err = errors.Join(err, e)
			}
		}
		for {
			_, e := qmp(cleanup, dir, "chardev-remove", map[string]any{"id": chardev})
			if e == nil {
				return
			}
			if waitGuest(cleanup, 200*time.Millisecond) != nil {
				err = errors.Join(err, fmt.Errorf("remove temporary guest-agent channel: %w", e))
				return
			}
		}
	}()
	_, err = qmp(ctx, dir, "device_add", map[string]any{
		"driver": "virtserialport", "id": port, "chardev": chardev, "name": "org.qemu.guest_agent.0",
	})
	if err != nil {
		return err
	}
	attached = true
	probe, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		if _, err := agentRPC(probe, path, "guest-ping", nil); err == nil {
			break
		}
		if err := waitGuest(probe, 500*time.Millisecond); err != nil {
			return fmt.Errorf("guest agent did not respond: %w", err)
		}
	}
	verify := func() (tailnet.Status, error) {
		raw, err := guestTailscale(ctx, path, "", "status", "--json", "--peers=false")
		var status tailnet.Status
		if err != nil {
			return status, err
		}
		if json.Unmarshal(raw, &status) != nil {
			return status, fmt.Errorf("invalid guest Tailscale status")
		}
		return status, nil
	}
	check := func(s tailnet.Status) error {
		if s.BackendState != "Running" || strings.TrimSuffix(s.Self.DNSName, ".") != expected || len(s.Self.Tags) == 0 || len(s.TailscaleIPs) == 0 {
			return fmt.Errorf("guest did not report the expected running tagged identity %s", expected)
		}
		return nil
	}
	status, err := verify()
	if err != nil {
		return err
	}
	if status.BackendState == "Running" {
		return check(status)
	}
	if _, err := guestTailscale(ctx, path, key, "up", "--auth-key=file:/dev/stdin", "--hostname="+name,
		"--accept-routes", "--snat-subnet-routes=false", "--ssh="+strconv.FormatBool(ssh), "--timeout=30s"); err != nil {
		return err
	}
	status, err = verify()
	if err != nil {
		return err
	}
	return check(status)
}

func guestTailscale(ctx context.Context, path, key string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	request := map[string]any{"path": "/usr/bin/tailscale", "arg": args, "capture-output": key == ""}
	if key != "" {
		request["input-data"] = base64.StdEncoding.EncodeToString([]byte(key))
	}
	raw, err := agentRPC(ctx, path, "guest-exec", request)
	if err != nil {
		return nil, err
	}
	var process struct{ PID int }
	if json.Unmarshal(raw, &process) != nil || process.PID <= 0 {
		return nil, fmt.Errorf("invalid guest process ID")
	}
	for {
		raw, err := agentRPC(ctx, path, "guest-exec-status", map[string]any{"pid": process.PID})
		if err != nil {
			return nil, err
		}
		var result struct {
			Exited   bool
			ExitCode *int
			Output   []byte `json:"out-data"`
		}
		if json.Unmarshal(raw, &result) != nil {
			return nil, fmt.Errorf("invalid guest process status")
		}
		if result.Exited {
			if result.ExitCode == nil || *result.ExitCode != 0 {
				return nil, fmt.Errorf("guest tailscale %s failed", args[0])
			}
			return result.Output, nil
		}
		if err := waitGuest(ctx, 250*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

func waitGuest(ctx context.Context, delay time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

func agentRPC(ctx context.Context, path, command string, args map[string]any) (json.RawMessage, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	return qemuRPC(json.NewEncoder(conn), json.NewDecoder(conn), command, args)
}

func qemuRPC(enc *json.Encoder, dec *json.Decoder, command string, args map[string]any) (json.RawMessage, error) {
	request := map[string]any{"execute": command}
	if args != nil {
		request["arguments"] = args
	}
	if err := enc.Encode(request); err != nil {
		return nil, err
	}
	for {
		var reply map[string]json.RawMessage
		if err := dec.Decode(&reply); err != nil {
			return nil, err
		}
		if reply["error"] != nil {
			return nil, fmt.Errorf("QEMU command %s failed", command)
		}
		if ret, ok := reply["return"]; ok {
			return ret, nil
		}
	}
}
