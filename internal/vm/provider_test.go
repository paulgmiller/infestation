package vm

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/paulgmiller/infestation/internal/fsutil"
)

func preparedProviderFixture(t *testing.T, name string) PreparedGroup {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "inf-prov-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	o := ClusterOptions{Options: Options{Name: "pool", Accel: "tcg", MemoryMiB: 3072}, Provider: name, Size: 2, Domain: "example.ts.net"}
	if name == "azure" {
		o.Accel, o.AzureLocation, o.AzureVMSize = "", "westus2", "Standard_B2s"
	}
	c := clusterConfig(o)
	dir := filepath.Join(root, o.Name)
	if err := fsutil.WriteJSON(filepath.Join(dir, "nodes.json"), c); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteJSON(filepath.Join(dir, "provider.json"), name); err != nil {
		t.Fatal(err)
	}
	r := preparedComputeGroup(o, dir, "/trusted/image.qcow2", c.Nodes)
	if name == "azure" {
		r.ImagePath = ""
	}
	for _, n := range r.Nodes {
		if err := fsutil.WriteFile(n.IgnitionPath, []byte(`{"ignition":{"version":"3.3.0"}}`), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestPreparedProviderOwnershipAndPrivateProfiles(t *testing.T) {
	for _, name := range []string{"qemu", "azure"} {
		t.Run(name, func(t *testing.T) {
			p, err := ProviderFor(name)
			if err != nil {
				t.Fatal(err)
			}
			r := preparedProviderFixture(t, name)
			if err := r.validate(p); err != nil {
				t.Fatal(err)
			}
			other := "azure"
			if name == other {
				other = "qemu"
			}
			wrong, _ := ProviderFor(other)
			if err := wrong.Create(context.Background(), r, io.Discard); err == nil || !strings.Contains(err.Error(), "ownership mismatch") {
				t.Fatal("cross-provider prepared request accepted", err)
			}
			// The final node's secret permissions must be checked before starting
			// any node in the batch (no QEMU/Azure tools are installed for this test).
			if err := os.Chmod(r.Nodes[1].IgnitionPath, 0644); err != nil {
				t.Fatal(err)
			}
			if err := p.Create(context.Background(), r, io.Discard); err == nil || !strings.Contains(err.Error(), "private regular file") {
				t.Fatal("world-readable profile accepted", err)
			}
		})
	}
}

func TestPreparedProviderRejectsIdentityAndDeliveryChanges(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*PreparedGroup)
		want   string
	}{
		{"node", func(r *PreparedGroup) { r.Nodes[0].Name = "unowned-0" }, "node identity"},
		{"profile", func(r *PreparedGroup) { r.Nodes[0].IgnitionPath = "/tmp/other-profile.ign" }, "node identity"},
		{"missing node", func(r *PreparedGroup) { r.Nodes = r.Nodes[:1] }, "inventory mismatch"},
		{"image", func(r *PreparedGroup) { r.Architecture = "aarch64" }, "unsupported prepared compute image"},
		{"egress", func(r *PreparedGroup) { r.Egress = "public-load-balancer" }, "unsupported egress"},
		{"group", func(r *PreparedGroup) { r.StateDir = r.StateDir + "/." }, "absolute directory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := preparedProviderFixture(t, "qemu")
			test.change(&r)
			err := (qemuCompute{}).Create(context.Background(), r, io.Discard)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q; got %v", test.want, err)
			}
		})
	}
}

func TestPreparedProviderRejectsSymlinkedSecretAndOutput(t *testing.T) {
	for _, directory := range []bool{false, true} {
		r := preparedProviderFixture(t, "qemu")
		path := r.Nodes[0].IgnitionPath
		if directory {
			path = filepath.Join(r.StateDir, "output")
		}
		if err := os.Rename(path, path+".saved"); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path+".saved", path); err != nil {
			t.Fatal(err)
		}
		if err := (qemuCompute{}).Create(context.Background(), r, io.Discard); err == nil || !strings.Contains(err.Error(), "private") {
			t.Fatal("symlinked private state accepted", err)
		}
	}
}

func TestPreparedProviderValidatesWholeBatchWithoutLeakingSecrets(t *testing.T) {
	for _, name := range []string{"qemu", "azure"} {
		r := preparedProviderFixture(t, name)
		secret := "tskey-auth-PRIVATE-MARKER"
		if err := os.WriteFile(r.Nodes[1].IgnitionPath, []byte(`{"ignition":{"version":"`+secret+`"}}`), 0600); err != nil {
			t.Fatal(err)
		}
		p, _ := ProviderFor(name)
		err := p.Create(context.Background(), r, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "invalid private Ignition") || strings.Contains(err.Error(), secret) {
			t.Fatal("malformed final profile must reject the batch without exposing private values", err)
		}
	}
}

func TestComputeLifecycleRefusesOtherProvidersSavedInventory(t *testing.T) {
	for _, name := range []string{"qemu", "azure"} {
		r := preparedProviderFixture(t, name)
		other := "azure"
		if name == other {
			other = "qemu"
		}
		p, _ := ProviderFor(other)
		for _, action := range []func(context.Context, Options, io.Writer) error{p.Start, p.Stop, p.Status} {
			err := action(context.Background(), Options{Name: r.Name, StateDir: filepath.Dir(r.StateDir)}, io.Discard)
			if err == nil || (!strings.Contains(err.Error(), "ownership mismatch") && !strings.Contains(err.Error(), "Azure-owned")) {
				t.Fatal("lifecycle accepted other provider's state", err)
			}
		}
	}
}

func TestProviderCapabilitiesDeclareStorageAndEgress(t *testing.T) {
	qemu, _ := ProviderFor("")
	azure, _ := ProviderFor("azure")
	q, a := qemu.Capabilities(), azure.Capabilities()
	if !q.RetainDiskOnStop || !a.RetainDiskOnStop || !q.RetainDiskOnRemove || a.RetainDiskOnRemove {
		t.Fatal("disk lifecycle capability would misrepresent destructive removal")
	}
	if q.RootDiskGiB != 20 || a.RootDiskGiB != 32 || q.Egress != EgressSLIRP || a.Egress != EgressAzureDefault || q.IgnitionDelivery != IgnitionPerVM || a.IgnitionDelivery != IgnitionShared {
		t.Fatal("actual provider requirements must be explicit")
	}
	if p, err := ProviderFor("unowned"); err == nil || p != nil {
		t.Fatal("unknown provider resolved")
	}
}

func TestComputeStopRetainsResourcesUntilWholeGroupDrainExists(t *testing.T) {
	for _, name := range []string{"qemu", "azure"} {
		r := preparedProviderFixture(t, name)
		p, _ := ProviderFor(name)
		err := p.Stop(context.Background(), Options{Name: r.Name, StateDir: filepath.Dir(r.StateDir)}, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "temporary drain") || !strings.Contains(err.Error(), "retained") {
			t.Fatal("unsafe temporary stop accepted", err)
		}
		for _, n := range r.Nodes {
			if _, err := os.Stat(n.IgnitionPath); err != nil {
				t.Fatal("stop altered private state", err)
			}
		}
	}
}

func TestPreparedAzurePreservesExplicitSubscription(t *testing.T) {
	o := ClusterOptions{Options: Options{Name: "pool"}, Provider: "azure", Size: 2, AzureSubscription: "explicit-subscription"}
	c := clusterConfig(o)
	prepared := preparedComputeGroup(o, "/private/pool", "", c.Nodes)
	if prepared.Subscription != o.AzureSubscription {
		t.Fatal("provider boundary dropped explicit Azure subscription")
	}
}
