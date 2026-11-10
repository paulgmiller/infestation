package ignition

import (
	"fmt"
	"github.com/coreos/ignition/v2/config/v3_3/types"
	"infestation/internal/cluster"
)

// accessProfile leaves K3s's managed kubeconfig untouched. Publication does not
// depend on Service approval or reachability, and retries source renewal.
func accessProfile(doc *types.Config, c cluster.Config, n cluster.Node) {
	if c.Tailscale == nil || n.Worker() {
		return
	}
	endpoint := "https://" + c.NodeAddress(n) + ":6443"
	if c.APITailnetHostname != "" {
		endpoint = "https://" + c.APITailnetHostname + ":443"
	}
	script := fmt.Sprintf(`#!/bin/sh
set -eu
umask 077
source=/etc/rancher/k3s/k3s.yaml
test -s "$source"
install -d -m 0700 /etc/infestation
tmp=$(mktemp /etc/infestation/.kubeconfig-XXXXXX)
trap 'rm -f "$tmp"' EXIT
cp "$source" "$tmp"
/usr/local/bin/k3s kubectl --kubeconfig "$tmp" config set-cluster default --server=%s --tls-server-name=kubernetes.default.svc.cluster.local >/dev/null
chmod 0600 "$tmp"
if ! cmp -s "$tmp" /etc/infestation/kubeconfig; then mv "$tmp" /etc/infestation/kubeconfig; fi
`, endpoint)
	doc.Storage.Files = append(doc.Storage.Files, File("/opt/infestation/publish-kubeconfig", []byte(script), 0700))
	unit := `[Unit]
Description=Publish portable private kubeconfig without cloud storage
After=k3s.service
[Service]
Type=oneshot
ExecStart=/opt/infestation/publish-kubeconfig
TimeoutStartSec=30s
`
	timer := `[Unit]
Description=Refresh portable kubeconfig after K3s credential renewal
[Timer]
OnBootSec=30s
OnUnitInactiveSec=30s
Unit=infestation-access.service
[Install]
WantedBy=timers.target
`
	doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "infestation-access.service", Contents: &unit}, types.Unit{Name: "infestation-access.timer", Enabled: ptr(true), Contents: &timer})
}
