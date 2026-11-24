package vm

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/fsutil"
	"github.com/paulgmiller/infestation/internal/image"
)

// Only prepared node profiles cross this boundary. Workstation CA/admin keys
// and the PKI directory are never serialized or copied to the compute host.
type sshCompute struct {
	Target   string
	StateDir string
	Owner    string
}
type sshRequest struct {
	Operation string
	Owner     string
	Options   Options
	Config    cluster.Config
	Group     PreparedGroup
	Profiles  [][]byte
	Removal   RemovalOptions
}

func sshCall(ctx context.Context, target string, request sshRequest, out io.Writer) error {
	// Destinations are SSH aliases or user@host, never shell expressions/options.
	if !regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@:\[\]-]*$`).MatchString(target) {
		return fmt.Errorf("invalid SSH destination; use an SSH config alias for ports or other options")
	}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "ssh", "-T", "-o", "BatchMode=yes", "--", target, `if command -v infestation >/dev/null 2>&1; then exec infestation __vm-ssh; else exec "$HOME/go/bin/infestation" __vm-ssh; fi`)
	cmd.Stdin = bytes.NewReader(raw)
	cmd.Stdout = out
	// SSH/helper failures can contain authentication URLs or private profile data.
	// Never include raw stderr or the stdin request in a returned error.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("SSH VM %s failed on %s; check SSH access, matching infestation binary and retained remote state", request.Operation, target)
	}
	return nil
}

type sshPreflight struct{ Host, StateDir string }

func remotePreflight(ctx context.Context, o ClusterOptions) (sshPreflight, error) {
	var out bytes.Buffer
	err := sshCall(ctx, o.SSHTarget, sshRequest{Operation: "preflight", Options: Options{Accel: o.Accel, MemoryMiB: o.MemoryMiB, StateDir: o.SSHStateDir}}, &out)
	if err != nil {
		return sshPreflight{}, err
	}
	var result sshPreflight
	if json.Unmarshal(out.Bytes(), &result) != nil || !regexp.MustCompile(`^host-[a-f0-9]{32}$`).MatchString(result.Host) || !filepath.IsAbs(result.StateDir) {
		return sshPreflight{}, fmt.Errorf("invalid remote host identity or state directory")
	}
	return result, nil
}

func createRemote(ctx context.Context, r PreparedGroup, out io.Writer) error {
	_, c, err := loadCluster(Options{Name: r.Name, StateDir: filepath.Dir(r.StateDir)})
	if err != nil {
		return err
	}
	link := sshCompute{Target: r.SSHTarget, StateDir: r.SSHStateDir, Owner: uuid.NewString()}
	request := sshRequest{Operation: "create", Owner: link.Owner, Config: c, Group: r, Options: Options{Name: r.Name, StateDir: link.StateDir}}
	for _, n := range r.Nodes {
		raw, err := os.ReadFile(n.IgnitionPath)
		if err != nil {
			return err
		}
		request.Profiles = append(request.Profiles, raw)
	}
	// Save routing/ownership before sending: a disconnected create must remain
	// inspectable and must never silently provision replacement disks on retry.
	if err := fsutil.CreatePrivateFile(filepath.Join(r.StateDir, "ssh.json"), mustSSHJSON(link)); err != nil {
		return err
	}
	return sshCall(ctx, link.Target, request, out)
}
func mustSSHJSON(value sshCompute) []byte { raw, _ := json.Marshal(value); return raw }

func remoteAction(ctx context.Context, o Options, action string, removal RemovalOptions, out io.Writer) (bool, error) {
	if o.Instance != "" && action != "start" && action != "stop" {
		return true, fmt.Errorf("--instance applies only to start and stop")
	}
	dir, err := directory(o)
	if err != nil {
		return true, err
	}
	var link sshCompute
	err = fsutil.ReadJSON(filepath.Join(dir, "ssh.json"), &link)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return true, err
	}
	_, inventory, err := loadCluster(o)
	if err != nil {
		return true, err
	}
	provider, err := groupProvider(dir)
	if err != nil {
		return true, err
	}
	if provider != "qemu" {
		return true, fmt.Errorf("SSH provider ownership mismatch")
	}
	if _, err := selectedQEMUNodes(inventory, o.Instance); err != nil {
		return true, err
	}
	// Readiness runs on the workstation over the tailnet, not on the SSH host.
	request := sshRequest{Operation: action, Owner: link.Owner, Options: Options{Name: o.Name, StateDir: link.StateDir, Accel: o.Accel, AllowOutage: o.AllowOutage, Instance: o.Instance}, Removal: removal}
	err = sshCall(ctx, link.Target, request, out)
	if err == nil && action == "start" && o.Wait > 0 {
		err = WaitCluster(ctx, o, out)
	}
	return true, err
}

// ServeSSH is an internal stdin protocol invoked through authenticated SSH.
// The host needs the same infestation binary, QEMU, and outbound connectivity.
func ServeSSH(ctx context.Context, input io.Reader, out io.Writer) error {
	var r sshRequest
	dec := json.NewDecoder(io.LimitReader(input, 32<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return fmt.Errorf("invalid SSH VM request")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return fmt.Errorf("unexpected SSH VM request data")
	}
	if r.Options.StateDir != "" && !filepath.IsAbs(r.Options.StateDir) {
		return fmt.Errorf("remote state directory must be absolute")
	}
	if r.Operation == "preflight" {
		if err := validateHost(r.Options.Accel); err != nil {
			return err
		}
		if r.Options.MemoryMiB < 2048 || r.Options.MemoryMiB > 32768 {
			return fmt.Errorf("cluster VMs require 2048–32768 MiB")
		}
		raw, err := os.ReadFile("/etc/machine-id")
		if err != nil || len(bytes.TrimSpace(raw)) == 0 {
			return fmt.Errorf("cannot determine remote host identity")
		}
		id := sha256.Sum256([]byte("infestation-host:" + strings.TrimSpace(string(raw))))
		stateRoot := r.Options.StateDir
		if stateRoot == "" {
			stateRoot = DefaultStateDir()
		}
		stateRoot, err = filepath.Abs(stateRoot)
		if err != nil {
			return err
		}
		return json.NewEncoder(out).Encode(sshPreflight{Host: fmt.Sprintf("host-%x", id[:16]), StateDir: stateRoot})
	}
	if _, err := uuid.Parse(r.Owner); err != nil {
		return fmt.Errorf("invalid SSH compute ownership")
	}
	dir, err := directory(r.Options)
	if err != nil {
		return err
	}
	if r.Operation == "create" {
		return receiveSSHCreate(ctx, r, dir, out)
	}
	var owner string
	if err := fsutil.ReadJSON(filepath.Join(dir, "ssh-owner.json"), &owner); err != nil {
		return err
	}
	if owner != r.Owner {
		return fmt.Errorf("SSH compute ownership mismatch")
	}
	switch r.Operation {
	case "start", "status":
		return ClusterAction(ctx, r.Options, r.Operation, out)
	case "netcheck":
		return GroupNetcheck(ctx, r.Options, out)
	case "stop":
		err := stopQEMUGroup(ctx, r.Options, out)
		if err != nil {
			// Stop errors omit raw provider/guest command output. Forward
			// per-VM failures because the SSH client discards stderr.
			fmt.Fprintln(out, err)
		}
		return err
	case "remove":
		return RemoveGroup(ctx, r.Options, "qemu", r.Removal, out)
	default:
		return fmt.Errorf("unsupported SSH VM operation")
	}
}

func receiveSSHCreate(ctx context.Context, r sshRequest, dir string, out io.Writer) error {
	if r.Group.Name != r.Options.Name || len(r.Config.Nodes) == 0 || len(r.Config.Nodes) != len(r.Profiles) {
		return fmt.Errorf("SSH compute inventory mismatch")
	}
	if err := r.Config.Validate(); err != nil {
		return err
	}
	for i, n := range r.Config.Nodes {
		if n.Name != fmt.Sprintf("%s-%d", r.Options.Name, i) {
			return fmt.Errorf("SSH node identity mismatch")
		}
	}
	if err := validateHost(r.Group.Accelerator); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("remote state already exists or cannot be reserved; retained unchanged")
	}
	if err := fsutil.WriteJSON(filepath.Join(dir, "ssh-owner.json"), r.Owner); err != nil {
		return err
	}
	if err := fsutil.WriteJSON(filepath.Join(dir, "nodes.json"), r.Config); err != nil {
		return err
	}
	if err := fsutil.WriteJSON(filepath.Join(dir, "provider.json"), "qemu"); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(dir, "output"), 0700); err != nil {
		return err
	}
	group := r.Group
	group.StateDir, group.SSHTarget, group.SSHStateDir = dir, "", ""
	group.Nodes = nil
	for i, n := range r.Config.Nodes {
		path := filepath.Join(dir, "output", n.Name+".ign")
		if err := fsutil.CreatePrivateFile(path, r.Profiles[i]); err != nil {
			return err
		}
		group.Nodes = append(group.Nodes, PreparedNode{Name: n.Name, IgnitionPath: path})
	}
	if group.ImagePath == "" {
		var err error
		group.ImagePath, err = image.Prepare(ctx, filepath.Join(filepath.Dir(dir), "images"), FlatcarVersion)
		if err != nil {
			return err
		}
	}
	return (qemuCompute{}).Create(ctx, group, out)
}
