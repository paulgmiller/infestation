package vm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coreos/ignition/v2/config/v3_3/types"
	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/ignition"
	"github.com/paulgmiller/infestation/internal/tailnet"
)

type exportRunner struct {
	info        string
	calls       [][]string
	failConvert bool
}

func (r *exportRunner) Run(_ context.Context, name string, args ...string) executil.Result {
	r.calls = append(r.calls, append([]string{name}, args...))
	if name != "qemu-img" {
		return executil.Result{Code: 1}
	}
	if args[0] == "info" {
		return executil.Result{Stdout: r.info}
	}
	if args[0] == "convert" && !r.failConvert {
		if err := os.WriteFile(args[len(args)-1], []byte("mock VHD output"), 0600); err != nil {
			return executil.Result{Code: 1}
		}
		return executil.Result{}
	}
	return executil.Result{Code: 1}
}

func exportFixture(t *testing.T) (ClusterOptions, string, *exportRunner) {
	t.Helper()
	t.Setenv("TAILSCALE_AUTH_KEY", "")
	root := t.TempDir()
	base := filepath.Join(root, "base.qcow2")
	if err := os.WriteFile(base, []byte("mock base"), 0444); err != nil {
		t.Fatal(err)
	}
	o := ClusterOptions{Options: Options{Name: "friend-0", BaseImage: base, AuthKey: "tskey-auth-PRIVATE-TEST-KEY"}, Domain: "example.ts.net", HelperSourceRef: strings.Repeat("a", 40), HelperBuilderImage: ignition.DefaultHelperBuilderImage, Host: "friend-proxmox", Site: "friend-home"}
	return o, filepath.Join(root, "export"), &exportRunner{info: `{"format":"qcow2"}`}
}

func TestProxmoxExportVHDJoinOnlyAndPrivate(t *testing.T) {
	o, dir, runner := exportFixture(t)
	var out bytes.Buffer
	if err := exportProxmox(context.Background(), o, dir, &out, runner); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 2 || strings.Join(runner.calls[1][:9], " ") != "qemu-img convert -f qcow2 -O vpc -o subformat=dynamic,force_size=on "+o.BaseImage {
		t.Fatal("missing VHD conversion", runner.calls)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "node.ign"))
	if err != nil {
		t.Fatal(err)
	}
	var doc types.Config
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	seenAuth, seenConfig := false, false
	for _, file := range doc.Storage.Files {
		if strings.Contains(file.Path, "ca.key") || strings.Contains(file.Path, "client.root") || file.Path == "/var/lib/infestation/join.json" {
			t.Fatal("initializer secret exported", file.Path)
		}
		if file.Path == tailnet.AuthPath || file.Path == "/etc/infestation/bootstrap.json" {
			data, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(*file.Contents.Source, "data:;base64,"))
			if err != nil {
				t.Fatal(err)
			}
			if file.Path == tailnet.AuthPath {
				seenAuth = true
				if strings.TrimSpace(string(data)) != o.AuthKey {
					t.Fatal("auth key not delivered")
				}
			} else {
				seenConfig = true
				if !bytes.Contains(data, []byte("cloud=proxmox")) || bytes.Contains(data, []byte(`"init": true`)) {
					t.Fatal("wrong topology or init authority")
				}
			}
		}
	}
	if !seenAuth || !seenConfig {
		t.Fatal("incomplete join profile")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 4 {
		t.Fatal("unexpected export files", entries)
	}
	for _, e := range entries {
		info, _ := e.Info()
		if info.Mode().Perm() != 0600 {
			t.Fatal("export not private", e.Name())
		}
		if strings.HasSuffix(e.Name(), ".py") || strings.HasSuffix(e.Name(), ".sh") {
			t.Fatal("generated script", e.Name())
		}
	}
	if strings.Contains(out.String(), o.AuthKey) {
		t.Fatal("secret in output")
	}
	if err := exportProxmox(context.Background(), o, dir, &out, runner); err == nil {
		t.Fatal("existing export overwritten")
	}
}

func TestProxmoxExportRefusesExternalDisk(t *testing.T) {
	for _, info := range []string{
		`{"format":"qcow2","backing-filename":"live-base"}`,
		`{"format":"qcow2","data-file":"other-disk"}`,
		`{"format":"qcow2","format-specific":{"data":{"data-file":"other-disk"}}}`,
		`{"format":"qcow2","children":[{"name":"data-file"}]}`,
	} {
		t.Run(info, func(t *testing.T) {
			o, dir, runner := exportFixture(t)
			runner.info = info
			if err := exportProxmox(context.Background(), o, dir, &bytes.Buffer{}, runner); err == nil {
				t.Fatal("exported external storage")
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatal("unsafe image reserved output")
			}
		})
	}
}

func TestProxmoxExportRequiresKeyAndRetainsFailedExport(t *testing.T) {
	o, dir, runner := exportFixture(t)
	o.AuthKey = ""
	if err := exportProxmox(context.Background(), o, dir, &bytes.Buffer{}, runner); err == nil {
		t.Fatal("missing key accepted")
	}
	if len(runner.calls) != 0 {
		t.Fatal("missing key invoked conversion")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("missing key wrote output")
	}
	o.AuthKey = "tskey-auth-PRIVATE-TEST-KEY"
	runner.failConvert = true
	if err := exportProxmox(context.Background(), o, dir, &bytes.Buffer{}, runner); err == nil {
		t.Fatal("failed conversion accepted")
	}
	if _, err := os.Stat(filepath.Join(dir, "node.ign")); !os.IsNotExist(err) {
		t.Fatal("failed conversion published credentials")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("failed export not retained")
	}
}

func TestProxmoxVHDConversionPreservesDisk(t *testing.T) {
	qemu, err := exec.LookPath("qemu-img")
	if err != nil {
		t.Skip("qemu-img unavailable")
	}
	o, dir, _ := exportFixture(t)
	// Use a separate blank test disk, never a live VM or cached vendor image.
	o.BaseImage = filepath.Join(t.TempDir(), "blank.qcow2")
	if output, err := exec.Command(qemu, "create", "-f", "qcow2", o.BaseImage, "1M").CombinedOutput(); err != nil {
		t.Fatal(string(output), err)
	}
	if err := ExportProxmox(context.Background(), o, dir, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	vhd := filepath.Join(dir, "flatcar.vhd")
	if output, err := exec.Command(qemu, "compare", "-f", "qcow2", "-F", "vpc", o.BaseImage, vhd).CombinedOutput(); err != nil {
		t.Fatal(string(output), err)
	}
	output, err := exec.Command(qemu, "info", "--output=json", vhd).Output()
	if err != nil {
		t.Fatal(err)
	}
	var info struct {
		Format string
		Size   int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal(output, &info); err != nil {
		t.Fatal(err)
	}
	if info.Format != "vpc" || info.Size != 1<<20 {
		t.Fatal("VHD format or virtual size changed", info)
	}
}
