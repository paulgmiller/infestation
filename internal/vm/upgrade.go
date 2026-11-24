package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	ignitionconfig "github.com/coreos/ignition/v2/config/v3_3"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/fsutil"
	"github.com/paulgmiller/infestation/internal/ignition"
	"github.com/paulgmiller/infestation/internal/nodebootstrap"
)

const K3sVersion = "v1.36.5+k3s1"
const CockroachImage = "cockroachdb/cockroach:v26.2.7"

type upgradeRecord struct {
	Node, Stage, HelperCommit, Identity, TailID, DatabaseIdentity string
	Profile                                                       []byte
}

type upgradeExport struct {
	Profile []byte
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// Guest output may contain credentials or authentication URLs. Keep it private,
// and report only the operation and exit status on failure.
func upgradeSSH(ctx context.Context, host, script string, input []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "tailscale", "ssh", "core@"+host, "sudo", "-n", "/bin/sh", "-ec", shellQuote(script))
	cmd.Stdin = bytes.NewReader(input)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("guest upgrade operation failed; node state retained (check Tailscale SSH and guest services)")
	}
	return stdout.Bytes(), nil
}

func uploadUpgrade(ctx context.Context, host, path string, data []byte, mode string) error {
	_, err := upgradeSSH(ctx, host, "umask 077; install -d -m 0700 /var/lib/infestation/upgrade; cat > "+shellQuote(path+".tmp")+"; chmod "+mode+" "+shellQuote(path+".tmp")+"; mv "+shellQuote(path+".tmp")+" "+shellQuote(path), data)
	return err
}

func profileNode(raw []byte) (nodebootstrap.Config, error) {
	var c nodebootstrap.Config
	doc, report, err := ignitionconfig.Parse(raw)
	if err != nil || report.IsFatal() {
		return c, fmt.Errorf("invalid owned Ignition profile")
	}
	found := false
	for _, f := range doc.Storage.Files {
		if f.Path != "/etc/infestation/bootstrap.json" {
			continue
		}
		if found || f.Contents.Source == nil {
			return c, fmt.Errorf("ambiguous profile configuration")
		}
		u, err := url.Parse(*f.Contents.Source)
		if err != nil || u.Scheme != "data" || !strings.HasPrefix(u.Opaque, ";base64,") {
			return c, fmt.Errorf("profile configuration must be embedded")
		}
		b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(u.Opaque, ";base64,"))
		if err != nil || json.Unmarshal(b, &c) != nil {
			return c, fmt.Errorf("invalid profile configuration")
		}
		found = true
	}
	if !found {
		return c, fmt.Errorf("profile has no node configuration")
	}
	return c, nil
}

func upgradeTransition(current string) error {
	if current == K3sVersion {
		return nil
	}
	if !regexp.MustCompile(`^v1\.35\.[0-9]+\+k3s1$`).MatchString(current) {
		return fmt.Errorf("this binary upgrades K3s 1.35 to %s only; refusing unsupported transition", K3sVersion)
	}
	return nil
}

func upgradeProfilePath(dir, name string) string { return filepath.Join(dir, name, "config.ign") }

// Upgrade applies this binary's pins to exactly one owned QEMU server. It is a
// temporary retained-store reset, not decommissioning or a cluster orchestrator.
// A successful CockroachDB drain is required; it is not a quorum certificate.
func Upgrade(ctx context.Context, o Options, out io.Writer) error {
	if o.Instance == "" {
		return fmt.Errorf("upgrade requires --instance; exactly one node is upgraded")
	}
	if handled, err := remoteAction(ctx, o, "upgrade", RemovalOptions{}, out); handled {
		if err != nil {
			return err
		}
		return syncUpgradeProfile(ctx, o)
	}
	dir, c, err := loadCluster(o)
	if err != nil {
		return err
	}
	provider, err := groupProvider(dir)
	if err != nil {
		return err
	}
	if provider != "qemu" {
		return fmt.Errorf("upgrade currently supports QEMU only")
	}
	nodes, err := selectedQEMUNodes(c, o.Instance)
	if err != nil {
		return err
	}
	n := nodes[0]
	host := c.NodeAddress(n)
	vmDir := filepath.Join(dir, n.Name)
	sourcePath := upgradeProfilePath(dir, n.Name)
	backup := filepath.Join(vmDir, "upgrade-"+strings.ReplaceAll(K3sVersion, "+", "-"))
	if err := fsutil.PrivateDir(backup); err != nil {
		return err
	}
	lock, err := lockMachine(backup)
	if err != nil {
		return fmt.Errorf("another upgrade is running: %w", err)
	}
	defer lock.Close()
	vmLock, err := lockMachine(vmDir)
	if err != nil {
		return err
	}
	defer vmLock.Close()
	// Keep the lifecycle lock across drain, shutdown, offline copy and restart.
	_, machine, err := load(Options{Name: n.Name, StateDir: dir})
	if err != nil {
		return err
	}
	previous, err := os.ReadFile(sourcePath)
	if err != nil {
		return fmt.Errorf("read owned instance profile: %w", err)
	}
	source, err := profileNode(previous)
	if err != nil {
		return err
	}
	if source.Node != n.Name {
		return fmt.Errorf("profile node differs from owned instance")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	recordPath := filepath.Join(backup, "progress.json")
	var r upgradeRecord
	if err := fsutil.ReadJSON(recordPath, &r); err != nil && !os.IsNotExist(err) {
		return err
	}
	if r.Node != "" && r.Node != n.Name {
		return fmt.Errorf("upgrade checkpoint belongs to another node")
	}
	save := func(stage string) error { r.Stage = stage; return fsutil.WriteJSON(recordPath, r) }
	node := Options{Name: n.Name, StateDir: dir}
	if r.Stage == "" {
		if _, err := qmp(ctx, vmDir, "query-status"); err != nil {
			return fmt.Errorf("selected VM must be running before upgrade")
		}
		raw, err := upgradeSSH(ctx, host, "cat /etc/infestation/bootstrap.json", nil)
		if err != nil {
			return err
		}
		var live nodebootstrap.Config
		if json.Unmarshal(raw, &live) != nil || live.Node != n.Name || live.Cluster.Tailscale == nil || live.Cluster.Tailscale.Domain != c.Tailscale.Domain {
			return fmt.Errorf("guest identity differs from owned node")
		}
		if err := upgradeTransition(live.Cluster.K3sExtensionVersion); err != nil {
			return err
		}
		// Database format/version transitions need their own supported procedure.
		if live.Cluster.CockroachImage != CockroachImage {
			return fmt.Errorf("database image differs from this binary's pin; database version transitions are not supported yet")
		}
		commit, err := ignition.ResolveHelperSource(ctx, ignition.DefaultHelperSourceRef)
		if err != nil {
			return err
		}
		profile, err := ignition.ReprovisionRelease(previous, ignition.Release{K3s: K3sVersion, Tailscale: TailscaleVersion, Cockroach: CockroachImage, HelperCommit: commit})
		if err != nil {
			return err
		}
		profile, err = retainedExtensionProfile(profile)
		if err != nil {
			return err
		}
		r = upgradeRecord{Node: n.Name, HelperCommit: commit, Profile: profile}
		if err := fsutil.CreatePrivateFile(filepath.Join(backup, "original.ign"), previous); err != nil && !os.IsExist(err) {
			return err
		}
		if err := fsutil.CreatePrivateFile(filepath.Join(backup, "original-bootstrap.json"), raw); err != nil && !os.IsExist(err) {
			return err
		}
		r.Identity, r.TailID, r.DatabaseIdentity, err = inspectUpgradeIdentity(ctx, host)
		if err != nil {
			return err
		}
		if err := save("prepared"); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "%s: target Flatcar %s, K3s %s, Tailscale %s, %s; helper %s\n", n.Name, FlatcarVersion, K3sVersion, TailscaleVersion, CockroachImage, r.HelperCommit)
	fmt.Fprintln(out, "One-node upgrade uses CockroachDB drain; it does not certify surviving range quorum.")
	if r.Stage == "prepared" {
		// Refuse local data paths the retained-state reset would erase.
		if err := checkUpgradeStorage(ctx, host, n.Name); err != nil {
			return err
		}
		if err := stageUpgrade(ctx, host, r); err != nil {
			return err
		}
		fmt.Fprintln(out, n.Name+": artifacts staged; draining Kubernetes and CockroachDB")
		if _, err := upgradeSSH(ctx, host, "/usr/local/bin/k3s kubectl drain "+shellQuote(n.Name)+" --ignore-daemonsets --timeout=2m", nil); err != nil {
			return fmt.Errorf("Kubernetes drain failed; node may remain cordoned: %w", err)
		}
		if err := drainDatabase(ctx, executil.OSRunner{}, host); err != nil {
			return err
		}
		if err := save("drained"); err != nil {
			return err
		}
	}
	if r.Stage == "drained" {
		if err := stageReset(ctx, host); err != nil {
			return err
		}
		if err := save("reset-staged"); err != nil {
			return err
		}
	}
	if r.Stage == "reset-staged" {
		if err := stopQEMULocked(ctx, vmDir, n.Name, out); err != nil {
			return err
		}
		if err := save("stopped"); err != nil {
			return err
		}
	}
	if r.Stage == "stopped" {
		present, err := processPresent(vmDir)
		if err != nil || present {
			return fmt.Errorf("VM must remain stopped during disk recovery copy")
		}
		// Never overwrite a recovery image, and never restore it automatically.
		snapshot := filepath.Join(backup, "before-reset.qcow2")
		if _, err := os.Stat(snapshot); os.IsNotExist(err) {
			tmp := snapshot + ".tmp"
			result := (executil.OSRunner{}).Run(ctx, "qemu-img", "convert", "-O", "qcow2", filepath.Join(vmDir, "disk.qcow2"), tmp)
			if result.Code != 0 {
				return fmt.Errorf("offline disk copy failed; VM remains stopped; rerun to resume")
			}
			if result := (executil.OSRunner{}).Run(ctx, "qemu-img", "check", tmp); result.Code != 0 {
				return fmt.Errorf("offline disk copy check failed; VM remains stopped")
			}
			if err := os.Rename(tmp, snapshot); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err := fsutil.WriteFile(sourcePath, r.Profile, 0600); err != nil {
			return err
		}
		if err := fsutil.WriteFile(filepath.Join(dir, "output", n.Name+".ign"), r.Profile, 0600); err != nil {
			return err
		}
		if err := save("installed"); err != nil {
			return err
		}
	}
	if r.Stage == "installed" {
		present, err := inspectQEMUStop(ctx, vmDir, n.Name)
		if err != nil {
			return err
		}
		if !present {
			if err := startQEMULocked(ctx, node, vmDir, machine, out); err != nil {
				return err
			}
		}
		if err := save("started"); err != nil {
			return err
		}
	}
	if r.Stage != "started" && r.Stage != "complete" {
		return fmt.Errorf("unknown upgrade checkpoint stage; inspect %s", recordPath)
	}
	fmt.Fprintln(out, n.Name+": waiting for retained identity and target versions")
	if err := waitUpgrade(ctx, host, r); err != nil {
		return fmt.Errorf("upgrade verification failed; node remains cordoned; rerun to resume: %w", err)
	}
	if _, err := upgradeSSH(ctx, host, "/usr/local/bin/k3s kubectl uncordon "+shellQuote(n.Name), nil); err != nil {
		return err
	}
	if err := save("complete"); err != nil {
		return err
	}
	if err := updateUpgradeInventory(dir); err != nil {
		return err
	}
	fmt.Fprintln(out, n.Name+": upgrade verified; uncordoned")
	return nil
}

func inspectUpgradeIdentity(ctx context.Context, host string) (string, string, string, error) {
	raw, err := upgradeSSH(ctx, host, "sha256sum /etc/machine-id /etc/cockroach/certs/ca.crt /etc/rancher/node/password /etc/ssh/ssh_host_ed25519_key.pub", nil)
	if err != nil {
		return "", "", "", fmt.Errorf("read retained identity hashes: %w", err)
	}
	identity := fmt.Sprintf("%x", sha256.Sum256(raw))
	raw, err = upgradeSSH(ctx, host, "/usr/bin/tailscale status --json --peers=false", nil)
	if err != nil {
		return "", "", "", fmt.Errorf("read tailnet identity: %w", err)
	}
	var status struct{ Self struct{ ID string } }
	if json.Unmarshal(raw, &status) != nil || status.Self.ID == "" {
		return "", "", "", fmt.Errorf("missing tailnet identity")
	}
	db, err := upgradeDatabaseIdentity(ctx, host)
	if err != nil {
		return "", "", "", fmt.Errorf("read database identity: %w", err)
	}
	return identity, status.Self.ID, db, nil
}

func upgradeDatabaseIdentity(ctx context.Context, host string) (string, error) {
	u := "postgresql://kine@" + host + ":26257/kubernetes?sslmode=verify-full&sslrootcert=/cockroach/certs/ca.crt&sslcert=/cockroach/certs/client.kine.crt&sslkey=/cockroach/certs/client.kine.key&options=-c%20allow_unsafe_internals%3Dtrue"
	raw, err := upgradeSSH(ctx, host, "/usr/bin/docker exec --user 0 cockroachdb /cockroach/cockroach sql --url="+shellQuote(u)+" --format=csv --execute='SELECT crdb_internal.cluster_id(), crdb_internal.node_id()'", nil)
	if err != nil {
		return "", err
	}
	if len(bytes.Split(bytes.TrimSpace(raw), []byte("\n"))) != 2 {
		return "", fmt.Errorf("unexpected database identity response")
	}
	return strings.TrimSpace(string(raw)), nil
}

func checkUpgradeStorage(ctx context.Context, host, name string) error {
	raw, err := upgradeSSH(ctx, host, "/usr/local/bin/k3s kubectl get pods -A --field-selector="+shellQuote("spec.nodeName="+name)+" -o json", nil)
	if err != nil {
		return err
	}
	var pods struct {
		Items []struct {
			Spec struct {
				Volumes []struct{ HostPath *struct{ Path string } }
			}
		}
	}
	if json.Unmarshal(raw, &pods) != nil {
		return fmt.Errorf("cannot inspect local workload paths")
	}
	for _, p := range pods.Items {
		for _, v := range p.Spec.Volumes {
			if v.HostPath != nil && !upgradeRetains(v.HostPath.Path) {
				return fmt.Errorf("workload uses an unretained hostPath; refusing reset")
			}
		}
	}
	raw, err = upgradeSSH(ctx, host, "/usr/local/bin/k3s kubectl get pv -o json", nil)
	if err != nil {
		return err
	}
	var pvs struct {
		Items []struct {
			Spec struct {
				HostPath *struct{ Path string }
				Local    *struct{ Path string }
			}
		}
	}
	if json.Unmarshal(raw, &pvs) != nil {
		return fmt.Errorf("cannot inspect persistent volume paths")
	}
	for _, pv := range pvs.Items {
		for _, p := range []*struct{ Path string }{pv.Spec.HostPath, pv.Spec.Local} {
			if p != nil && !upgradeRetains(p.Path) {
				return fmt.Errorf("persistent volume uses an unretained path; refusing reset")
			}
		}
	}
	return nil
}

func upgradeRetains(path string) bool {
	if path != filepath.Clean(path) || !filepath.IsAbs(path) {
		return false
	}
	for _, keep := range ignition.ReprovisionKeepPaths() {
		if path == keep || strings.HasPrefix(path, keep+"/") {
			return true
		}
	}
	// These host paths are kernel/runtime interfaces, not persistent data.
	for _, p := range []string{"/proc", "/sys", "/dev", "/run"} {
		if path == p || strings.HasPrefix(path, p+"/") {
			return true
		}
	}
	return false
}

func stageUpgrade(ctx context.Context, host string, r upgradeRecord) error {
	raw, err := upgradeSSH(ctx, host, "cat /var/lib/infestation/join.json", nil)
	if err != nil {
		return err
	}
	var bundle nodebootstrap.JoinBundle
	if json.Unmarshal(raw, &bundle) != nil || bundle.Cluster.Tailscale == nil {
		return fmt.Errorf("invalid retained join bundle")
	}
	bundle.Cluster.K3sExtensionVersion = K3sVersion
	bundle.Cluster.Tailscale.Version = TailscaleVersion
	bundle.Cluster.CockroachImage = CockroachImage
	raw, err = json.Marshal(bundle)
	if err != nil {
		return err
	}
	if err := uploadUpgrade(ctx, host, "/var/lib/infestation/upgrade/join.json", raw, "0600"); err != nil {
		return err
	}
	if err := uploadUpgrade(ctx, host, "/var/lib/infestation/upgrade/replacement.ign", r.Profile, "0600"); err != nil {
		return err
	}
	doc, report, err := ignitionconfig.Parse(r.Profile)
	if err != nil || report.IsFatal() {
		return fmt.Errorf("invalid checkpoint profile")
	}
	cfg, err := profileNode(r.Profile)
	if err != nil {
		return err
	}
	for name, version := range map[string]string{"k3s": K3sVersion, "tailscale": TailscaleVersion} {
		source := fmt.Sprintf("https://extensions.flatcar.org/extensions/%s-%s-%s.raw", name, version, cfg.Cluster.Architecture)
		path := "/var/lib/infestation/upgrade/" + name + ".raw"
		if _, err := upgradeSSH(ctx, host, "curl -fL --retry 3 --max-time 300 "+shellQuote(source)+" -o "+shellQuote(path+".tmp")+" && test -s "+shellQuote(path+".tmp")+" && mv "+shellQuote(path+".tmp")+" "+shellQuote(path), nil); err != nil {
			return fmt.Errorf("stage extension: %w", err)
		}
	}
	for _, f := range doc.Storage.Files {
		if f.Path == "/opt/infestation/build-helper" {
			data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(*f.Contents.Source, "data:;base64,"))
			if err != nil {
				return err
			}
			if err := uploadUpgrade(ctx, host, "/var/lib/infestation/upgrade/build-helper", data, "0700"); err != nil {
				return err
			}
		}
	}
	// Existing helper processes remain running; only the next service start uses
	// the freshly built binary. Reset retains this verified build cache.
	if _, err := upgradeSSH(ctx, host, "/var/lib/infestation/upgrade/build-helper", nil); err != nil {
		return fmt.Errorf("stage helper build: %w", err)
	}
	contract, err := upgradeSSH(ctx, host, "/opt/infestation/infestation-node --contract-version", nil)
	if err != nil || strings.TrimSpace(string(contract)) != nodebootstrap.HelperContractVersion {
		return fmt.Errorf("staged helper has an incompatible contract")
	}
	return stageFlatcar(ctx, host)
}

func readFlatcarUpdateStatus(ctx context.Context, host string) (string, string, error) {
	raw, err := upgradeSSH(ctx, host, "/usr/bin/update_engine_client -status", nil)
	if err != nil {
		return "", "", fmt.Errorf("read Flatcar updater status: %w", err)
	}
	values := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || (key != "CURRENT_OP" && key != "NEW_VERSION") {
			continue
		}
		if _, exists := values[key]; exists {
			return "", "", fmt.Errorf("ambiguous Flatcar updater status")
		}
		values[key] = value
	}
	if values["CURRENT_OP"] == "" {
		return "", "", fmt.Errorf("Flatcar updater did not report its state")
	}
	return values["CURRENT_OP"], values["NEW_VERSION"], nil
}

func stageFlatcar(ctx context.Context, host string) error {
	version, err := upgradeSSH(ctx, host, ". /etc/os-release; printf '%s' \"$VERSION_ID\"", nil)
	if err != nil {
		return err
	}
	current := strings.TrimSpace(string(version))
	op, pending, err := readFlatcarUpdateStatus(ctx, host)
	if err != nil {
		return err
	}
	// flatcar-update refuses another update until an already-staged update boots.
	// Reuse that payload only when it matches this binary's exact OS target.
	if op == "UPDATE_STATUS_UPDATED_NEED_REBOOT" {
		if pending != FlatcarVersion {
			return fmt.Errorf("Flatcar %s is already staged for reboot; this binary targets %s; pending update retained", pending, FlatcarVersion)
		}
		return nil
	}
	if op != "UPDATE_STATUS_IDLE" {
		return fmt.Errorf("Flatcar updater is busy (%s); wait and rerun this command", op)
	}
	if current == FlatcarVersion {
		return nil
	}
	if compareFlatcar(current, FlatcarVersion) >= 0 {
		return fmt.Errorf("refusing Flatcar downgrade or unknown version")
	}
	if _, err := upgradeSSH(ctx, host, "/usr/bin/flatcar-update --to-version "+FlatcarVersion, nil); err != nil {
		return fmt.Errorf("stage Flatcar update: %w", err)
	}
	op, pending, err = readFlatcarUpdateStatus(ctx, host)
	if err != nil {
		return err
	}
	if op != "UPDATE_STATUS_UPDATED_NEED_REBOOT" || pending != FlatcarVersion {
		return fmt.Errorf("Flatcar target update is not staged for reboot; node retained")
	}
	return nil
}

func compareFlatcar(a, b string) int {
	aa, bb := strings.Split(a, "."), strings.Split(b, ".")
	if len(aa) != 3 || len(bb) != 3 {
		return 1
	}
	for i := range aa {
		x, e := strconv.Atoi(aa[i])
		y, f := strconv.Atoi(bb[i])
		if e != nil || f != nil {
			return 1
		}
		if x < y {
			return -1
		}
		if x > y {
			return 1
		}
	}
	return 0
}

func upgradeKeepPaths() []string {
	return append(ignition.ReprovisionKeepPaths(), "/etc/extensions/k3s.raw", "/etc/extensions/tailscale.raw", "/var/lib/docker", "/var/lib/containerd", "/var/log", "/opt/infestation/infestation-node", "/opt/infestation/helper-build.identity", "/opt/infestation/helper-build.sha256")
}

// Match flatcar-reset's generate_regex, verified against its staging marker.
func upgradeRetentionRegex(paths []string) string {
	result := "("
	for _, path := range paths {
		path = strings.TrimSuffix(path, "/")
		if strings.HasPrefix(path, "/") && !strings.ContainsAny(path[len(path)-1:], "$)*]?") {
			result += path + "/.*|"
		}
		result += path + "|"
	}
	return result + "/flatcar-backup|/flatcar-backup/.*|/selective-os-reset)"
}

func stageReset(ctx context.Context, host string) error {
	// Retain downloaded extensions at their final paths; Ignition uses local copies
	// from the retained staging directory, avoiding a network dependency at reset.
	script := "set -eu\nfor name in k3s tailscale; do cp /var/lib/infestation/upgrade/$name.raw /etc/extensions/$name.raw.tmp; chmod 0644 /etc/extensions/$name.raw.tmp; mv /etc/extensions/$name.raw.tmp /etc/extensions/$name.raw; done\n/usr/bin/tailscale serve --service=svc:k8s-api --tcp=443 off\nif ! test -e /var/lib/infestation/upgrade/original-join.json; then cp -p /var/lib/infestation/join.json /var/lib/infestation/upgrade/original-join.json; fi\ncp /var/lib/infestation/upgrade/join.json /var/lib/infestation/join.json.tmp\nchmod 0600 /var/lib/infestation/join.json.tmp\nmv /var/lib/infestation/join.json.tmp /var/lib/infestation/join.json\nflatcar-reset --ignition-file /var/lib/infestation/upgrade/replacement.ign --keep-machine-id --keep-paths"
	for _, p := range upgradeKeepPaths() {
		script += " " + shellQuote(p)
	}
	script += "\ncmp /var/lib/infestation/upgrade/replacement.ign /oem/config.ign\ntest -e /boot/flatcar/first_boot\ntest \"$(cat /selective-os-reset)\" = " + shellQuote(upgradeRetentionRegex(upgradeKeepPaths())) + "\ngrep -F \"systemd.machine_id=$(cat /etc/machine-id)\" /oem/grub.cfg >/dev/null\n"
	_, err := upgradeSSH(ctx, host, script, nil)
	return err
}

func waitUpgrade(ctx context.Context, host string, r upgradeRecord) error {
	var last error
	for {
		child, cancel := context.WithTimeout(ctx, 30*time.Second)
		last = verifyUpgrade(child, host, r)
		cancel()
		if last == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: %v", ctx.Err(), last)
		case <-time.After(5 * time.Second):
		}
	}
}

func verifyUpgrade(ctx context.Context, host string, r upgradeRecord) error {
	identity, tail, db, err := inspectUpgradeIdentity(ctx, host)
	if err != nil {
		return err
	}
	if identity != r.Identity || tail != r.TailID || db != r.DatabaseIdentity {
		return fmt.Errorf("retained node identity changed")
	}
	script := "set -eu\n. /etc/os-release\ntest \"$VERSION_ID\" = " + shellQuote(FlatcarVersion) + "\ntest \"$(/usr/bin/docker inspect --format '{{.Config.Image}}' cockroachdb)\" = " + shellQuote(CockroachImage) + "\n/usr/local/bin/k3s --version\n/usr/bin/tailscale version\ncat /opt/infestation/helper-build.identity\n/usr/local/bin/k3s kubectl get --raw=/readyz\n/usr/local/bin/k3s kubectl get node " + shellQuote(r.Node) + " -o json"
	raw, err := upgradeSSH(ctx, host, script, nil)
	if err != nil {
		return err
	}
	if !bytes.Contains(raw, []byte("k3s version "+K3sVersion)) || !bytes.Contains(raw, []byte(strings.TrimPrefix(TailscaleVersion, "v"))) || !bytes.Contains(raw, []byte(ignition.HelperBuildIdentity(r.HelperCommit, ""))) {
		return fmt.Errorf("software versions have not converged")
	}
	// Parse only the trailing node JSON, never print kubeconfig or pod data.
	start := bytes.Index(raw, []byte("{\n"))
	if start < 0 {
		return fmt.Errorf("missing Kubernetes node status")
	}
	var n struct {
		Status struct {
			NodeInfo   struct{ KubeletVersion string }
			Conditions []struct{ Type, Status string }
		}
	}
	if json.Unmarshal(raw[start:], &n) != nil || n.Status.NodeInfo.KubeletVersion != K3sVersion {
		return fmt.Errorf("Kubernetes node version has not converged")
	}
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" && c.Status == "True" {
			return nil
		}
	}
	return fmt.Errorf("Kubernetes node is not Ready")
}

func syncUpgradeProfile(ctx context.Context, o Options) error {
	dir, c, err := loadCluster(o)
	if err != nil {
		return err
	}
	nodes, err := selectedQEMUNodes(c, o.Instance)
	if err != nil {
		return err
	}
	var link sshCompute
	if err := fsutil.ReadJSON(filepath.Join(dir, "ssh.json"), &link); err != nil {
		return err
	}
	var output bytes.Buffer
	if err := sshCall(ctx, link.Target, sshRequest{Operation: "upgrade-result", Owner: link.Owner, Options: Options{Name: o.Name, StateDir: link.StateDir, Instance: o.Instance}}, &output); err != nil {
		return err
	}
	var result upgradeExport
	if json.Unmarshal(output.Bytes(), &result) != nil {
		return fmt.Errorf("invalid remote upgrade result")
	}
	cfg, err := profileNode(result.Profile)
	if err != nil || cfg.Node != nodes[0].Name || cfg.Cluster.K3sExtensionVersion != K3sVersion {
		return fmt.Errorf("remote upgrade profile identity/version mismatch")
	}
	// Workstation state has no machine directory for SSH-backed groups.
	if err := fsutil.WriteFile(filepath.Join(dir, "output", nodes[0].Name+".ign"), result.Profile, 0600); err != nil {
		return err
	}
	return updateUpgradeInventory(dir)
}

func retainedExtensionProfile(raw []byte) ([]byte, error) {
	doc, report, err := ignitionconfig.Parse(raw)
	if err != nil || report.IsFatal() {
		return nil, fmt.Errorf("invalid upgrade profile")
	}
	files := doc.Storage.Files[:0]
	for _, f := range doc.Storage.Files {
		if f.Path == "/etc/extensions/k3s.raw" || f.Path == "/etc/extensions/tailscale.raw" {
			continue
		}
		if f.Path == "/opt/infestation/check-retained-state" {
			data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(*f.Contents.Source, "data:;base64,"))
			if err != nil {
				return nil, err
			}
			data = append(data, []byte("test -s /etc/extensions/k3s.raw\ntest -s /etc/extensions/tailscale.raw\n")...)
			f = ignition.File(f.Path, data, 0755)
		}
		files = append(files, f)
	}
	doc.Storage.Files = files
	result, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	if _, report, err := ignitionconfig.Parse(result); err != nil || report.IsFatal() {
		return nil, fmt.Errorf("invalid retained extension profile")
	}
	return append(result, '\n'), nil
}

// A group's configuration cannot represent mixed instance versions. Keep each
// per-node profile authoritative, and advance the group pins once every saved
// instance profile has reached this release. Ownership and disks are unchanged.
func updateUpgradeInventory(dir string) error {
	config, err := cluster.Load(inventoryPath(dir))
	if err != nil {
		return err
	}
	for _, n := range config.Nodes {
		raw, err := os.ReadFile(upgradeProfilePath(dir, n.Name))
		if os.IsNotExist(err) {
			raw, err = os.ReadFile(filepath.Join(dir, "output", n.Name+".ign"))
		}
		if err != nil {
			return fmt.Errorf("read instance profile before recording group pins: %w", err)
		}
		cfg, err := profileNode(raw)
		if err != nil {
			return err
		}
		if cfg.Node != n.Name || cfg.Cluster.Tailscale == nil {
			return fmt.Errorf("instance profile identity differs from inventory")
		}
		if cfg.Cluster.K3sExtensionVersion != K3sVersion || cfg.Cluster.Tailscale.Version != TailscaleVersion || cfg.Cluster.CockroachImage != CockroachImage {
			return nil
		}
	}
	config.K3sExtensionVersion = K3sVersion
	config.Tailscale.Version = TailscaleVersion
	config.CockroachImage = CockroachImage
	return fsutil.WriteJSON(inventoryPath(dir), config)
}
