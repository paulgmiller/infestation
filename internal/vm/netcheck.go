package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/coreos/ignition/v2/config/v3_3/types"
	"github.com/paulgmiller/infestation/internal/ignition"
)

func validateNetwork(network string) error {
	if network != "" && network != "slirp" && network != "passt" {
		return fmt.Errorf("--network must be slirp or passt")
	}
	if network == "passt" {
		if _, err := exec.LookPath("passt"); err != nil {
			return fmt.Errorf("--network passt requires the passt executable on PATH")
		}
	}
	return nil
}

func addNetcheck(doc *types.Config) {
	yes := true
	script := `#!/bin/sh
set -eu
umask 077
/usr/bin/tailscale netcheck --format=json >/run/infestation-netcheck.json 2>/run/infestation-netcheck.log
cat /run/infestation-netcheck.json >/dev/virtio-ports/org.infestation.netcheck
`
	doc.Storage.Files = append(doc.Storage.Files, ignition.File("/opt/infestation/netcheck", []byte(script), 0700))
	unit := `[Unit]
Description=Report guest Tailscale network diagnostics privately to the VM host
Requires=tailscaled.service
Wants=network-online.target
After=network-online.target tailscaled.service infestation-enroll.service
[Service]
Type=oneshot
ExecStart=/opt/infestation/netcheck
TimeoutStartSec=60s
`
	timer := `[Unit]
Description=Refresh guest Tailscale network diagnostics
[Timer]
OnBootSec=30s
OnUnitInactiveSec=5min
Unit=infestation-netcheck.service
[Install]
WantedBy=timers.target
`
	doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "infestation-netcheck.service", Contents: &unit}, types.Unit{Name: "infestation-netcheck.timer", Enabled: &yes, Contents: &timer})
}

// latestObject tolerates a partial final write on the guest's private virtio
// channel, but never uses a report from before the current VM start.
func latestObject(path string) (map[string]json.RawMessage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 4<<20))
	var last map[string]json.RawMessage
	for {
		var next map[string]json.RawMessage
		if err = dec.Decode(&next); err != nil {
			break
		}
		if next != nil {
			last = next
		}
	}
	if last == nil {
		return nil, fmt.Errorf("guest netcheck report is not available yet; wait for the boot diagnostics timer")
	}
	return last, nil
}

// Netcheck reads the real guest CLI's latest boot/timer report, not the host's
// network diagnostics. The Now field identifies when the guest ran the command.
func Netcheck(ctx context.Context, o Options, out io.Writer) error {
	dir, _, err := load(o)
	if err != nil {
		return err
	}
	if _, err = qmp(ctx, dir, "query-status"); err != nil {
		return fmt.Errorf("VM is not running or QMP is unavailable: %w", err)
	}
	report, err := latestObject(filepath.Join(dir, "netcheck.json"))
	if err != nil {
		return err
	}
	// Reject unrelated JSON before displaying a stale/incomplete success result.
	if report["Now"] == nil || report["UDP"] == nil {
		return fmt.Errorf("guest reported invalid netcheck JSON")
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}
