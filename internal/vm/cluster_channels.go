package vm

import (
	"encoding/json"
	"fmt"
	config "github.com/coreos/ignition/v2/config/v3_3"
	"github.com/coreos/ignition/v2/config/v3_3/types"
	"infestation/internal/ignition"
	"strings"
)

func addClusterChannels(raw []byte) ([]byte, error) {
	doc, report, err := config.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("cluster Ignition invalid: %v: %s", err, report.String())
	}
	addNetcheck(&doc)
	// Enrollment-only units use this dependency; on cluster nodes the host
	// preparation unit owns enrollment instead.
	for i := range doc.Systemd.Units {
		if doc.Systemd.Units[i].Name == "infestation-netcheck.service" {
			s := *doc.Systemd.Units[i].Contents
			s = strings.ReplaceAll(s, "infestation-enroll.service", "infestation-tailnet.service")
			doc.Systemd.Units[i].Contents = &s
		}
	}
	script := `#!/bin/sh
set -eu
umask 077
/usr/bin/tailscale status --json --peers=false >/run/infestation-tailnet-status.json
cat /run/infestation-tailnet-status.json >/dev/virtio-ports/org.infestation.enrollment
`
	doc.Storage.Files = append(doc.Storage.Files, ignition.File("/opt/infestation/report-enrollment", []byte(script), 0700))
	unit := `[Unit]
Description=Publish cluster node diagnostic report privately
Wants=network-online.target
After=network-online.target tailscaled.service
[Service]
Type=oneshot
ExecStart=/opt/infestation/infestation-node diagnose
ExecStartPost=-/opt/infestation/report-enrollment
TimeoutStartSec=90s
`
	timer := `[Unit]
Description=Refresh cluster diagnostics and enrollment status
[Timer]
OnBootSec=30s
OnUnitInactiveSec=30s
Unit=infestation-diagnostics.service
[Install]
WantedBy=timers.target
`
	yes := true
	doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "infestation-diagnostics.service", Contents: &unit}, types.Unit{Name: "infestation-diagnostics.timer", Enabled: &yes, Contents: &timer})
	raw, err = json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if _, report, err = config.Parse(raw); err != nil {
		return nil, fmt.Errorf("cluster diagnostic Ignition invalid: %v: %s", err, report.String())
	}
	return raw, nil
}
