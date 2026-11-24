package ignition

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	config "github.com/coreos/ignition/v2/config/v3_3"
	"github.com/coreos/ignition/v2/config/v3_3/types"
	"github.com/paulgmiller/infestation/internal/nodebootstrap"
	"github.com/paulgmiller/infestation/internal/tailnet"
)

// ReprovisionKeepPaths returns Flatcar retention arguments: state directories
// and the SSH host-key regular expression for the configuration-only reset.
// Workload-specific hostPath/local-volume paths require a separate inventory.
func ReprovisionKeepPaths() []string {
	return []string{"/var/lib/cockroach", "/var/lib/tailscale", "/var/lib/rancher/k3s", "/var/lib/kubelet", "/etc/rancher/node", "/var/lib/infestation", "/etc/cockroach/certs", "/etc/ssh/ssh_host_.*"}
}

func reprovisionRetains(path string) bool {
	if strings.HasPrefix(path, "/etc/ssh/ssh_host_") {
		return true
	}
	for _, retained := range ReprovisionKeepPaths() {
		if path == retained || strings.HasPrefix(path, retained+"/") {
			return true
		}
	}
	return false
}

// ReprovisionMemory updates a joining node's original profile without changing
// software pins, identity or helper revision. It never supplies fresh enrollment
// or initialization authority. The caller must retain state, keep the machine ID,
// verify workload storage and drain/snapshot the VM before applying the profile.
// Founding-node and version upgrades require their own recovery workflow.
func ReprovisionMemory(previous []byte, goMemory, sqlMemory string) ([]byte, error) {
	return reprovision(previous, goMemory, sqlMemory, nil)
}

// Release pins the software delivered by a configuration reset. Flatcar's OS
// payload is staged separately; database format upgrades remain unsupported.
type Release struct {
	K3s, Tailscale, Cockroach, HelperCommit string
}

// ReprovisionRelease prepares a retained-state reset for one joining server.
func ReprovisionRelease(previous []byte, release Release) ([]byte, error) {
	return reprovision(previous, "768MiB", "256MiB", &release)
}

func reprovision(previous []byte, goMemory, sqlMemory string, release *Release) ([]byte, error) {
	doc, report, err := config.Parse(previous)
	if err != nil || report.IsFatal() {
		return nil, fmt.Errorf("invalid source Ignition profile")
	}
	if len(doc.Storage.Disks)+len(doc.Storage.Raid)+len(doc.Storage.Filesystems)+len(doc.Storage.Luks) != 0 {
		return nil, fmt.Errorf("reprovisioning must not partition or format disks")
	}
	var cfg nodebootstrap.Config
	configIndex := -1
	for i, file := range doc.Storage.Files {
		if file.Path == "/etc/infestation/bootstrap.json" {
			if configIndex >= 0 || file.Contents.Source == nil {
				return nil, fmt.Errorf("expected one embedded node configuration")
			}
			const prefix = "data:;base64,"
			source := *file.Contents.Source
			if !strings.HasPrefix(source, prefix) {
				return nil, fmt.Errorf("node configuration must be embedded")
			}
			data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(source, prefix))
			if err != nil || json.Unmarshal(data, &cfg) != nil {
				return nil, fmt.Errorf("invalid embedded node configuration")
			}
			configIndex = i
		}
	}
	if configIndex < 0 || !cfg.Cluster.DiscoverPeers || cfg.Cluster.Tailscale == nil || cfg.ScaleSet != nil {
		return nil, fmt.Errorf("memory reprovisioning requires an independently enrolled QEMU node")
	}
	if release != nil {
		cfg.Cluster.K3sExtensionVersion = release.K3s
		cfg.Cluster.Tailscale.Version = release.Tailscale
		cfg.Cluster.CockroachImage = release.Cockroach
	}
	cfg.Cluster.CockroachGoMemory = goMemory
	cfg.Cluster.CockroachSQLMemory = sqlMemory
	if err := cfg.Cluster.Validate(); err != nil {
		return nil, err
	}
	if len(cfg.Cluster.Nodes) != 1 || cfg.Cluster.Nodes[0].Name != cfg.Node {
		return nil, fmt.Errorf("source profile must describe exactly its local node")
	}
	node := cfg.Cluster.Nodes[0]
	if node.Init || node.Worker() || !node.Database() || cfg.Cluster.CockroachStorage != "persistent" {
		return nil, fmt.Errorf("memory reprovisioning currently supports joining database servers with persistent storage")
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	doc.Storage.Files[configIndex] = File("/etc/infestation/bootstrap.json", data, 0600)
	var files []types.File
	for _, file := range doc.Storage.Files {
		// Retained state must never be overwritten by the replacement profile.
		omit := file.Path == tailnet.AuthPath || file.Path == "/opt/infestation/check-retained-state" || strings.HasPrefix(file.Path, nodebootstrap.BootstrapCredentialDir+"/") || reprovisionRetains(file.Path)
		if !omit {
			files = append(files, file)
		}
	}
	doc.Storage.Files = files
	if release != nil {
		kept := doc.Storage.Files[:0]
		for _, file := range doc.Storage.Files {
			if file.Path == "/opt/infestation/build-helper" {
				continue
			}
			if file.Path == "/etc/extensions/k3s.raw" {
				file.Contents.Source = ptr(fmt.Sprintf("https://extensions.flatcar.org/extensions/k3s-%s-%s.raw", release.K3s, cfg.Cluster.Architecture))
			}
			if file.Path == "/etc/extensions/tailscale.raw" {
				file.Contents.Source = ptr(fmt.Sprintf("https://extensions.flatcar.org/extensions/tailscale-%s-%s.raw", release.Tailscale, cfg.Cluster.Architecture))
			}
			kept = append(kept, file)
		}
		doc.Storage.Files = kept
	}
	for _, link := range doc.Storage.Links {
		if reprovisionRetains(link.Path) {
			return nil, fmt.Errorf("profile link would replace retained state")
		}
	}
	var units []types.Unit
	for _, unit := range doc.Systemd.Units {
		if unit.Name == "infestation-retained-state.service" || (release != nil && unit.Name == "infestation-helper-build.service") {
			continue
		}
		var dropins []types.Dropin
		for _, dropin := range unit.Dropins {
			if dropin.Name != "05-retained-state.conf" && !(release != nil && dropin.Name == "10-helper-build.conf") {
				dropins = append(dropins, dropin)
			}
		}
		unit.Dropins = dropins
		units = append(units, unit)
	}
	doc.Systemd.Units = units
	found, foundTailscale := false, false
	for i := range doc.Systemd.Units {
		unit := &doc.Systemd.Units[i]
		foundTailscale = foundTailscale || unit.Name == "tailscaled.service"
		if unit.Name == "cockroachdb.service" {
			if found {
				return nil, fmt.Errorf("duplicate database unit")
			}
			contents := CockroachUnit(cfg.Cluster, node)
			unit.Contents = &contents
			found = true
		}
		if unit.Contents != nil && strings.Contains(*unit.Contents, " --init") {
			return nil, fmt.Errorf("reprovisioning must not grant initialization authority")
		}
		if unit.Name == "cockroachdb.service" || unit.Name == "k3s.service" || unit.Name == "tailscaled.service" || strings.HasPrefix(unit.Name, "infestation-") || unit.Name == "tailscale-api.service" {
			dependency := "[Unit]\nRequires=infestation-retained-state.service\nAfter=infestation-retained-state.service\n"
			unit.Dropins = append(unit.Dropins, types.Dropin{Name: "05-retained-state.conf", Contents: &dependency})
		}
	}
	if !found {
		return nil, fmt.Errorf("source profile has no database service")
	}
	guard := `#!/bin/sh
set -eu
for path in /var/lib/cockroach/STORAGE_MIN_VERSION /var/lib/tailscale/tailscaled.state /var/lib/rancher/k3s/server/token /etc/rancher/node/password /var/lib/infestation/join.json /etc/cockroach/certs/ca.crt /etc/cockroach/certs/node.key /etc/cockroach/certs/client.kine.key; do
  if ! test -s "$path"; then
    echo "missing retained node state: $path; refusing enrollment or database startup" >&2
    exit 1
  fi
done
`
	doc.Storage.Files = append(doc.Storage.Files, File("/opt/infestation/check-retained-state", []byte(guard), 0755))
	unit := "[Unit]\nDescription=Verify state retained across immutable reprovisioning\nBefore=infestation-tailnet.service cockroachdb.service k3s.service\n[Service]\nType=oneshot\nExecStart=/opt/infestation/check-retained-state\nRemainAfterExit=yes\n[Install]\nWantedBy=multi-user.target\n"
	doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "infestation-retained-state.service", Enabled: ptr(true), Contents: &unit})
	dependency := "[Unit]\nRequires=infestation-retained-state.service\nAfter=infestation-retained-state.service\n"
	if !foundTailscale {
		doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "tailscaled.service", Dropins: []types.Dropin{{Name: "05-retained-state.conf", Contents: &dependency}}})
	}
	if release != nil {
		if err := helperBuildProfile(&doc, Bootstrap{HelperSourceCommit: release.HelperCommit}); err != nil {
			return nil, err
		}
		for i := range doc.Systemd.Units {
			if doc.Systemd.Units[i].Name == "infestation-helper-build.service" {
				doc.Systemd.Units[i].Dropins = append(doc.Systemd.Units[i].Dropins, types.Dropin{Name: "05-retained-state.conf", Contents: &dependency})
			}
		}
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	if _, rpt, err := config.Parse(raw); err != nil || rpt.IsFatal() {
		return nil, fmt.Errorf("invalid reprovisioning Ignition profile")
	}
	return append(raw, '\n'), nil
}
