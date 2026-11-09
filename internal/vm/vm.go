// Package vm runs private, outbound-only Flatcar enrollment VMs without libvirt.
package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	config "github.com/coreos/ignition/v2/config/v3_3"
	"github.com/coreos/ignition/v2/config/v3_3/types"
	"infestation/internal/executil"
	"infestation/internal/fsutil"
	"infestation/internal/ignition"
	"infestation/internal/image"
	"infestation/internal/tailnet"
)

const FlatcarVersion = "4757.2.2"
const TailscaleVersion = "v1.104.2"

type Options struct {
	Name, StateDir, AuthKey, AuthKeyFile, BaseImage, Accel string
	Wait                                                   time.Duration
}
type machine struct{ Name, BaseImage, Accel string }

func DefaultStateDir() string {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "infestation", "vms")
}
func directory(o Options) (string, error) {
	if !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`).MatchString(o.Name) {
		return "", fmt.Errorf("--name must be a lowercase DNS label")
	}
	if o.StateDir == "" {
		o.StateDir = DefaultStateDir()
	}
	dir, err := filepath.Abs(filepath.Join(o.StateDir, o.Name))
	// QEMU key/value argument syntax treats commas specially. Reject rather than
	// escaping only some paths (including the QMP socket and backing chain).
	if strings.ContainsAny(dir, ",\n\r") || len(filepath.Join(dir, "qmp.sock")) > 100 {
		return "", fmt.Errorf("VM state path must be short and contain no commas or newlines")
	}
	return dir, err
}

func authKey(o Options) (string, error) {
	if o.AuthKey != "" && o.AuthKeyFile != "" {
		return "", fmt.Errorf("provide one auth key source")
	}
	key := o.AuthKey
	if key == "" && o.AuthKeyFile == "" {
		key = os.Getenv("TAILSCALE_AUTH_KEY")
	}
	if o.AuthKeyFile != "" {
		data, err := os.ReadFile(o.AuthKeyFile)
		if err != nil {
			return "", err
		}
		key = string(data)
	}
	key = strings.TrimSpace(key)
	if !strings.HasPrefix(key, "tskey-auth-") || strings.ContainsAny(key, " \t\r\n") {
		return "", fmt.Errorf("provide --tailscale-auth-key, TAILSCALE_AUTH_KEY or --tailscale-auth-key-file with a valid auth key")
	}
	return key, nil
}

// Profile is enrollment-only: no Kubernetes, database, SSH, inbound port or
// subnet advertisement is configured. The supplied auth key controls identity.
func Profile(name, key string) ([]byte, error) {
	yes := true
	doc := types.Config{Ignition: types.Ignition{Version: "3.3.0"}}
	add := func(path, text string, mode int) {
		doc.Storage.Files = append(doc.Storage.Files, ignition.File(path, []byte(text), mode))
	}
	add("/etc/hostname", name+"\n", 0644)
	add("/etc/flatcar/update.conf", "REBOOT_STRATEGY=off\n", 0644)
	add(tailnet.AuthPath, key+"\n", 0600)
	add("/etc/default/tailscaled", "PORT=41641\nFLAGS=\n", 0644)
	source := "https://extensions.flatcar.org/extensions/tailscale-" + TailscaleVersion + "-x86-64.raw"
	mode := 0644
	doc.Storage.Files = append(doc.Storage.Files, types.File{Node: types.Node{Path: "/etc/extensions/tailscale.raw", Overwrite: &yes}, FileEmbedded1: types.FileEmbedded1{Mode: &mode, Contents: types.Resource{Source: &source}}})
	// Never emit auth errors/URLs or the auth key on the serial console. Status
	// goes only to a private host file over the unidirectional virtio channel.
	script := `#!/bin/sh
set -eu
umask 077
report() {
  /usr/bin/tailscale status --json --peers=false >/run/infestation-tailnet-status.json
  cat /run/infestation-tailnet-status.json >/dev/virtio-ports/org.infestation.enrollment
}
if ! /usr/bin/tailscale ip -4 >/dev/null 2>&1; then
  if ! /usr/bin/tailscale up --auth-key=file:/etc/infestation/tailscale-authkey --hostname=` + name + ` --timeout=60s >/dev/null 2>&1; then
    report
    exit 1
  fi
fi
report
rm -f /etc/infestation/tailscale-authkey
`
	add("/opt/infestation/enroll", script, 0700)
	unit := `[Unit]
Description=Enroll this Flatcar VM in Tailscale
Wants=network-online.target
Requires=tailscaled.service
After=network-online.target tailscaled.service
StartLimitIntervalSec=0
[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/opt/infestation/enroll
Restart=on-failure
RestartSec=15s
TimeoutStartSec=90s
[Install]
WantedBy=multi-user.target
`
	doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "infestation-enroll.service", Enabled: &yes, Contents: &unit})
	// Enabling by name allows sysext's unit location to change without a dangling
	// explicit symlink. Current sysext also pre-enables tailscaled itself.
	doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: "tailscaled.service", Enabled: &yes})
	for _, name := range []string{"sshd.service", "sshd.socket", "ssh.service", "ssh.socket", "sshd@.service"} {
		doc.Systemd.Units = append(doc.Systemd.Units, types.Unit{Name: name, Mask: &yes})
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil, err
	}
	if _, report, err := config.Parse(raw); err != nil {
		return nil, fmt.Errorf("invalid VM Ignition: %v: %s", err, report.String())
	}
	return raw, nil
}

func validateHost(accel string) error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		return fmt.Errorf("VM commands currently require Linux x86-64")
	}
	for _, tool := range []string{"qemu-system-x86_64", "qemu-img"} {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("install qemu-system-x86 and qemu-utils first: %s is missing", tool)
		}
	}
	if accel != "kvm" && accel != "tcg" {
		return fmt.Errorf("--accel must be kvm or tcg")
	}
	if accel == "kvm" {
		f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("KVM unavailable; enable virtualization and access to /dev/kvm, or use --accel tcg for slow software emulation")
		}
		f.Close()
	}
	return nil
}

func Create(ctx context.Context, o Options, out io.Writer) error {
	dir, err := directory(o)
	if err != nil {
		return err
	}
	if err = validateHost(o.Accel); err != nil {
		return err
	}
	key, err := authKey(o)
	if err != nil {
		return err
	}
	// Reserve a new directory before generating secrets or disks. Never overwrite
	// another VM's Ignition, identity, base image or overlay, including on retries.
	if err = os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return err
	}
	if err = os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("create VM state (use a new --name, or vm start for an existing VM): %w", err)
	}
	base := o.BaseImage
	if base == "" {
		fmt.Fprintln(out, "Downloading and verifying Flatcar image…")
		base, err = image.Prepare(ctx, filepath.Join(filepath.Dir(dir), "images"), FlatcarVersion)
		if err != nil {
			return err
		}
	}
	base, err = filepath.Abs(base)
	if err != nil {
		return err
	}
	if strings.ContainsAny(base, ",\r\n") {
		return fmt.Errorf("base image path cannot contain commas or newlines")
	}
	result := executil.OSRunner{}.Run(ctx, "qemu-img", "info", "--output=json", base)
	if err = result.Err("inspect base image"); err != nil {
		return err
	}
	var info struct {
		Format  string
		Backing string `json:"backing-filename"`
	}
	if json.Unmarshal([]byte(result.Stdout), &info) != nil || info.Format != "qcow2" || info.Backing != "" {
		return fmt.Errorf("base image must be a standalone qcow2")
	}
	raw, err := Profile(o.Name, key)
	if err != nil {
		return err
	}
	if err = fsutil.CreatePrivateFile(filepath.Join(dir, "config.ign"), raw); err != nil {
		return err
	}
	disk := filepath.Join(dir, "disk.qcow2")
	result = executil.OSRunner{}.Run(ctx, "qemu-img", "create", "-f", "qcow2", "-F", "qcow2", "-b", base, disk, "20G")
	if err = result.Err("create VM overlay"); err != nil {
		return err
	}
	if err = os.Chmod(disk, 0600); err != nil {
		return err
	}
	if err = fsutil.WriteJSON(filepath.Join(dir, "machine.json"), machine{o.Name, base, o.Accel}); err != nil {
		return err
	}
	return Start(ctx, o, out)
}

func arguments(dir string, m machine) []string {
	cpu := "host"
	if m.Accel == "tcg" {
		cpu = "max"
	}
	return []string{"-name", m.Name, "-machine", "q35,accel=" + m.Accel, "-cpu", cpu, "-smp", "2", "-m", "1024", "-display", "none", "-daemonize", "-pidfile", filepath.Join(dir, "qemu.pid"), "-qmp", "unix:" + filepath.Join(dir, "qmp.sock") + ",server=on,wait=off", "-serial", "file:" + filepath.Join(dir, "console.log"), "-drive", "file=" + filepath.Join(dir, "disk.qcow2") + ",format=qcow2,if=virtio", "-fw_cfg", "name=opt/org.flatcar-linux/config,file=" + filepath.Join(dir, "config.ign"), "-netdev", "user,id=egress", "-device", "virtio-net-pci,netdev=egress", "-device", "virtio-serial-pci", "-chardev", "file,id=enrollment,path=" + filepath.Join(dir, "enrollment.json"), "-device", "virtserialport,chardev=enrollment,name=org.infestation.enrollment"}
}

func load(o Options) (string, machine, error) {
	dir, err := directory(o)
	if err != nil {
		return "", machine{}, err
	}
	var m machine
	err = fsutil.ReadJSON(filepath.Join(dir, "machine.json"), &m)
	if err == nil && m.Name != o.Name {
		err = fmt.Errorf("VM state identity mismatch")
	}
	return dir, m, err
}

func Start(ctx context.Context, o Options, out io.Writer) error {
	dir, m, err := load(o)
	if err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "start.lock"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("another start may be in progress: %w", err)
	}
	lock.Close()
	defer os.Remove(filepath.Join(dir, "start.lock"))
	if err = validateHost(m.Accel); err != nil {
		return err
	}
	// QMP socket ownership/liveness is authoritative; never kill a bare PID.
	if _, err = os.Lstat(filepath.Join(dir, "qmp.sock")); err == nil {
		return fmt.Errorf("VM QMP socket exists; use vm status/stop before starting again")
	}
	for _, name := range []string{"console.log", "enrollment.json"} {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		f.Close()
	}
	result := executil.OSRunner{}.Run(ctx, "qemu-system-x86_64", arguments(dir, m)...)
	if err = result.Err("start QEMU"); err != nil {
		return err
	}
	fmt.Fprintln(out, "VM started:", o.Name, "(outbound-only networking)")
	fmt.Fprintln(out, "Private state:", dir)
	if o.Wait == 0 {
		return nil
	}
	deadline, cancel := context.WithTimeout(ctx, o.Wait)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if s, err := enrollment(dir, m.Name); err == nil {
			fmt.Fprintf(out, "Enrolled: %s %s\n", strings.TrimSuffix(s.Self.DNSName, "."), strings.Join(s.TailscaleIPs, ", "))
			return nil
		}
		select {
		case <-deadline.Done():
			return fmt.Errorf("enrollment not confirmed: %w; VM remains running; use vm status --name %s and inspect private console.log", deadline.Err(), o.Name)
		case <-ticker.C:
		}
	}
}

func enrollment(dir, name string) (tailnet.Status, error) {
	f, err := os.Open(filepath.Join(dir, "enrollment.json"))
	if err != nil {
		return tailnet.Status{}, err
	}
	defer f.Close()
	var s tailnet.Status
	dec := json.NewDecoder(io.LimitReader(f, 4<<20))
	found := false
	for {
		var next tailnet.Status
		if err = dec.Decode(&next); err != nil {
			break
		}
		s = next
		found = true
	}
	if !found {
		return s, fmt.Errorf("guest has not reported Tailscale status yet")
	}
	if s.BackendState != "Running" || s.Self.HostName != name || len(s.Self.Tags) == 0 || len(s.TailscaleIPs) == 0 {
		return s, fmt.Errorf("VM has not reported the expected running, tagged Tailscale identity")
	}
	return s, nil
}

// qmp connects only to a socket inside this VM's private state directory.
func qmp(ctx context.Context, dir, command string) (json.RawMessage, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "qmp.sock"))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	dec, enc := json.NewDecoder(conn), json.NewEncoder(conn)
	var greeting map[string]json.RawMessage
	if err = dec.Decode(&greeting); err != nil {
		return nil, err
	}
	for _, cmd := range []string{"qmp_capabilities", command} {
		if err = enc.Encode(map[string]string{"execute": cmd}); err != nil {
			return nil, err
		}
		for {
			var msg map[string]json.RawMessage
			if err = dec.Decode(&msg); err != nil {
				return nil, err
			}
			if msg["error"] != nil {
				return nil, fmt.Errorf("QEMU command %s failed", cmd)
			}
			if ret, ok := msg["return"]; ok {
				if cmd == command {
					return ret, nil
				}
				break
			}
		}
	}
	return nil, nil
}
func Status(ctx context.Context, o Options, out io.Writer) error {
	dir, m, err := load(o)
	if err != nil {
		return err
	}
	raw, err := qmp(ctx, dir, "query-status")
	if err != nil {
		return fmt.Errorf("VM is stopped or QMP unavailable: %w", err)
	}
	var status struct{ Status string }
	if err = json.Unmarshal(raw, &status); err != nil {
		return err
	}
	fmt.Fprintln(out, "VM:", m.Name, status.Status)
	s, err := enrollment(dir, m.Name)
	if err != nil {
		fmt.Fprintln(out, "Enrollment not confirmed")
		return err
	}
	fmt.Fprintf(out, "Tailnet: %s %s\n", strings.TrimSuffix(s.Self.DNSName, "."), strings.Join(s.TailscaleIPs, ", "))
	fmt.Fprintln(out, "Enrollment was reported at boot; this is not a live peer reachability check.")
	return nil
}
func Stop(ctx context.Context, o Options, out io.Writer) error {
	dir, _, err := load(o)
	if err != nil {
		return err
	}
	if _, err = qmp(ctx, dir, "system_powerdown"); err != nil {
		return err
	}
	fmt.Fprintln(out, "Graceful shutdown requested; disk and Tailscale identity are preserved.")
	return nil
}
