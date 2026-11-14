package vm

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	config "github.com/coreos/ignition/v2/config/v3_3"
	"github.com/paulgmiller/infestation/internal/cluster"
	"github.com/paulgmiller/infestation/internal/executil"
	"github.com/paulgmiller/infestation/internal/fsutil"
	"github.com/paulgmiller/infestation/internal/ignition"
	"github.com/paulgmiller/infestation/internal/image"
)

// ExportProxmox writes an offline, join-only bundle. It never starts compute,
// initializes a database, or claims ownership of a VM managed by Proxmox.
// The image is the generic QEMU image; its Ignition transport is fw_cfg, not
// the config-drive transport of Flatcar's separate proxmoxve image.
func ExportProxmox(ctx context.Context, o ClusterOptions, destination string, out io.Writer) error {
	return exportProxmox(ctx, o, destination, out, executil.OSRunner{})
}

func exportProxmox(ctx context.Context, o ClusterOptions, destination string, out io.Writer, runner executil.Runner) error {
	if o.Init || o.Size > 1 {
		return fmt.Errorf("Proxmox export supports one joining node only")
	}
	if !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`).MatchString(o.Name) {
		return fmt.Errorf("--name must be a lowercase DNS label")
	}
	if destination == "" {
		return fmt.Errorf("--out-dir is required")
	}
	for _, value := range []string{o.Host, o.Site} {
		if value != "" && !regexp.MustCompile(`^[A-Za-z0-9_.-]+$`).MatchString(value) {
			return fmt.Errorf("host/site labels contain invalid characters")
		}
	}
	dir, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dir); err == nil {
		return fmt.Errorf("export directory already exists; refusing replacement")
	} else if !os.IsNotExist(err) {
		return err
	}
	o.Domain, err = resolveDomain(ctx, o.Domain, executil.OSRunner{})
	if err != nil {
		return err
	}
	key, err := authKey(o.Options)
	if err != nil {
		return err
	}
	commit, err := ignition.ResolveHelperSource(ctx, o.HelperSourceRef)
	if err != nil {
		return err
	}
	o.Size, o.Provider = 1, "qemu"
	c := clusterConfig(o)
	c.Nodes[0].Name = o.Name
	c.Nodes[0].Locality = (cluster.Topology{Cloud: "proxmox", Host: o.Host, Site: o.Site}).Locality()
	if err := c.Validate(); err != nil {
		return err
	}
	doc, err := ignition.Render(c, c.Nodes[0], "", "", ignition.Bootstrap{HelperSourceCommit: commit, HelperBuilderImage: o.HelperBuilderImage, TailscaleAuthKey: key})
	if err != nil {
		return err
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if _, report, err := config.Parse(raw); err != nil {
		return fmt.Errorf("invalid exported Ignition: %v: %s", err, report.String())
	}
	base := o.BaseImage
	if base == "" {
		cache := o.StateDir
		if cache == "" {
			cache = DefaultStateDir()
		}
		base, err = image.Prepare(ctx, filepath.Join(cache, "images"), FlatcarVersion)
		if err != nil {
			return err
		}
	}
	// Only accept standalone images. Never export a live overlay/backing chain.
	base, err = filepath.Abs(base)
	if err != nil {
		return err
	}
	result := runner.Run(ctx, "qemu-img", "info", "--output=json", base)
	if err := result.Err("inspect export base image"); err != nil {
		return err
	}
	var info struct {
		Format    string
		Backing   string `json:"backing-filename"`
		Encrypted bool
		DataFile  string `json:"data-file"`
		Children  []struct{ Name string }
		Specific  struct{ Data map[string]json.RawMessage } `json:"format-specific"`
	}
	if json.Unmarshal([]byte(result.Stdout), &info) != nil || info.Format != "qcow2" || info.Backing != "" || info.Encrypted {
		return fmt.Errorf("export requires a trusted, never-booted standalone QEMU qcow2 base image")
	}
	if info.DataFile != "" || info.Specific.Data["data-file"] != nil {
		return fmt.Errorf("export refuses qcow2 external data files")
	}
	for _, child := range info.Children {
		if child.Name != "file" {
			return fmt.Errorf("export refuses qcow2 external children")
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return fmt.Errorf("reserve export directory: %w", err)
	}
	// Leave any failed export available for inspection, never replace it on retry.
	disk := filepath.Join(dir, "flatcar.vhd")
	result = runner.Run(ctx, "qemu-img", "convert", "-f", "qcow2", "-O", "vpc", "-o", "subformat=dynamic,force_size=on", base, disk)
	if err := result.Err("convert Flatcar to VHD"); err != nil {
		return err
	}
	if err := os.Chmod(disk, 0600); err != nil {
		return err
	}
	f, err := os.Open(disk)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, f)
	closeErr := f.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	files := map[string][]byte{
		"node.ign":     append(raw, '\n'),
		"image.sha256": fmt.Appendf(nil, "%x  flatcar.vhd\n", hash.Sum(nil)),
	}
	for name, data := range files {
		if err := fsutil.CreatePrivateFile(filepath.Join(dir, name), data); err != nil {
			return err
		}
	}
	if err := fsutil.WriteJSON(filepath.Join(dir, "manifest.json"), map[string]any{"format": "infestation-proxmox-export-v1", "diskFormat": "vpc", "node": o.Name, "flatcarVersion": FlatcarVersion, "helperCommit": commit, "ignitionTransport": "qemu-fw_cfg", "databaseInitialize": false}); err != nil {
		return err
	}
	fmt.Fprintln(out, "Proxmox export:", dir)
	fmt.Fprintln(out, "VHD: flatcar.vhd; private enrollment profile: node.ign. Manual setup: docs/friend-vm.md#proxmox")
	return nil
}
