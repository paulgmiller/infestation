package ignition

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/coreos/ignition/v2/config/v3_3/types"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/tailnet"
)

func tailnetProfile(doc *types.Config, c cluster.Config, n cluster.Node, options []Bootstrap) error {
	if len(options) == 0 || (options[0].LocalHelper == "" && options[0].HelperSourceCommit == "") {
		return fmt.Errorf("Tailscale currently requires a static VM profile and a node helper; Azure support is pending")
	}
	keyText := options[0].TailscaleAuthKey
	if keyText != "" && c.Tailscale.AuthKeyFile != "" {
		return fmt.Errorf("provide either a Tailscale auth key argument or auth_key_file, not both")
	}
	if keyText == "" {
		if c.Tailscale.AuthKeyFile == "" {
			return fmt.Errorf("Tailscale enrollment requires --tailscale-auth-key or auth_key_file")
		}
		key, err := os.ReadFile(c.Tailscale.AuthKeyFile)
		if err != nil {
			return fmt.Errorf("read private Tailscale auth_key_file: %w", err)
		}
		keyText = string(key)
	}
	keyText = strings.TrimSpace(keyText)
	if !strings.HasPrefix(keyText, "tskey-auth-") || strings.ContainsAny(keyText, " \t\r\n") {
		return fmt.Errorf("invalid Tailscale auth key")
	}
	if options[0].HelperSourceCommit == "" {
		helper, err := os.ReadFile(options[0].LocalHelper)
		if err != nil {
			return err
		}
		doc.Storage.Files = append(doc.Storage.Files, File("/opt/infestation/infestation-node", helper, 0755))
	}
	doc.Storage.Files = append(doc.Storage.Files,
		File(tailnet.AuthPath, []byte(keyText+"\n"), 0600),
		File("/etc/rancher/k3s/vpn-auth", []byte("name=tailscale,joinKey=host-managed-no-enrollment\n"), 0600),
		File("/etc/sysctl.d/70-infestation-tailnet.conf", []byte("net.ipv4.ip_forward=1\nnet.bridge.bridge-nf-call-iptables=1\n"), 0644),
		File("/etc/modules-load.d/70-infestation-tailnet.conf", []byte("br_netfilter\n"), 0644),
		File("/etc/default/tailscaled", []byte("PORT=41641\nFLAGS=\n"), 0644))
	settings, _ := json.Marshal(tailnet.Config{Name: n.Name, DNSName: c.NodeAddress(n), Server: !n.Worker()})
	doc.Storage.Files = append(doc.Storage.Files, File(tailnet.ConfigPath, settings, 0600))
	url := fmt.Sprintf("https://extensions.flatcar.org/extensions/tailscale-%s-%s.raw", c.Tailscale.Version, c.Architecture)
	doc.Storage.Files = append(doc.Storage.Files, types.File{Node: types.Node{Path: "/etc/extensions/tailscale.raw", Overwrite: ptr(true)}, FileEmbedded1: types.FileEmbedded1{Mode: ptr(0644), Contents: types.Resource{Source: ptr(url)}}})
	// tailscaled is supplied by the immutable sysext. No Kubernetes dependency.
	doc.Storage.Links = append(doc.Storage.Links, types.Link{Node: types.Node{Path: "/etc/systemd/system/multi-user.target.wants/tailscaled.service", Overwrite: ptr(true)}, LinkEmbedded1: types.LinkEmbedded1{Target: ptr("/usr/lib/systemd/system/tailscaled.service")}})
	unit := `[Unit]
Description=Enroll and prepare host Tailscale before database and Kubernetes
Wants=network-online.target
After=network-online.target tailscaled.service
Requires=tailscaled.service
Before=cockroachdb.service k3s.service k3s-agent.service
StartLimitIntervalSec=0

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/opt/infestation/infestation-node tailnet
TimeoutStartSec=120s
Restart=on-failure
RestartSec=15s

[Install]
WantedBy=multi-user.target
`
	if c.Tailscale.SSH {
		unit = strings.Replace(unit, "ExecStart=/opt/infestation/infestation-node tailnet\n", "ExecStart=/opt/infestation/infestation-node tailnet\nExecStartPost=/usr/bin/tailscale set --ssh=true\n", 1)
	}
	doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "infestation-tailnet.service", Enabled: ptr(true), Contents: &unit})
	return nil
}
