package vm

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	config "github.com/coreos/ignition/v2/config/v3_3"
	"github.com/paulgmiller/infestation/internal/cluster"
)

// ComputeProvider is the compute boundary after shared node preparation. Create
// receives private, already-rendered Ignition, never a CA signing key, cluster
// token input, or a request to discover/configure cluster membership. Lifecycle
// methods operate only on saved owned resources; they must not create replacements.
// Stop and Remove include the cluster safety gates, not just power operations.
type ComputeProvider interface {
	Name() string
	Capabilities() ComputeCapabilities
	Create(context.Context, PreparedGroup, io.Writer) error
	Start(context.Context, Options, io.Writer) error
	Stop(context.Context, Options, io.Writer) error
	Status(context.Context, Options, io.Writer) error
	Remove(context.Context, Options, RemovalOptions, io.Writer) error
}

// ComputeCapabilities records actual provider behavior, including prototype
// limitations. It does not promise that a provider has verified network egress.
type ComputeCapabilities struct {
	IgnitionDelivery   string
	Egress             string
	RootDiskGiB        int
	RetainDiskOnStop   bool
	RetainDiskOnRemove bool
}

const (
	IgnitionPerVM  = "private-per-vm-file"
	IgnitionShared = "private-shared-custom-data"
	EgressSLIRP    = "qemu-user-network-nat"
	// Azure default outbound remains an explicit prototype limitation. A future
	// egress implementation must change this declaration rather than add hidden
	// NAT/load-balancer dependencies behind the shared bootstrap boundary.
	EgressAzureDefault = "azure-default-outbound-access"
)

// PreparedGroup references a private provider-group directory reserved by the
// shared preparer. It contains no cluster configuration or credential values.
// Profiles are delivered per VM on QEMU. Azure VMSS uses the first profile as its
// shared customData; its IMDS bootstrap derives each instance's final identity.
type PreparedGroup struct {
	Name, StateDir                        string
	Nodes                                 []PreparedNode
	ImageVersion, Architecture, ImagePath string
	MemoryMiB                             int
	Accelerator, MachineType, Region      string
	Subscription                          string
	Egress                                string
}

type PreparedNode struct {
	Name, IgnitionPath string
}

// ProviderFor resolves a provider without inspecting resources or running tools.
func ProviderFor(name string) (ComputeProvider, error) {
	switch name {
	case "", "qemu":
		return qemuCompute{}, nil
	case "azure":
		return azureCompute{}, nil
	default:
		return nil, fmt.Errorf("unknown compute provider %q", name)
	}
}

type qemuCompute struct{}
type azureCompute struct{}

var _ ComputeProvider = qemuCompute{}
var _ ComputeProvider = azureCompute{}

func (qemuCompute) Name() string  { return "qemu" }
func (azureCompute) Name() string { return "azure" }
func (qemuCompute) Capabilities() ComputeCapabilities {
	return ComputeCapabilities{IgnitionDelivery: IgnitionPerVM, Egress: EgressSLIRP, RootDiskGiB: 20, RetainDiskOnStop: true, RetainDiskOnRemove: true}
}
func (azureCompute) Capabilities() ComputeCapabilities {
	return ComputeCapabilities{IgnitionDelivery: IgnitionShared, Egress: EgressAzureDefault, RootDiskGiB: 32, RetainDiskOnStop: true, RetainDiskOnRemove: false}
}

func preparedComputeGroup(o ClusterOptions, dir, base string, nodes []cluster.Node) PreparedGroup {
	p, _ := ProviderFor(o.Provider) // CreateCluster has already resolved it.
	r := PreparedGroup{Name: o.Name, StateDir: dir, ImageVersion: FlatcarVersion, Architecture: "x86-64", ImagePath: base, MemoryMiB: o.MemoryMiB, Accelerator: o.Accel, MachineType: o.AzureVMSize, Region: o.AzureLocation, Subscription: o.AzureSubscription, Egress: p.Capabilities().Egress}
	for _, n := range nodes {
		r.Nodes = append(r.Nodes, PreparedNode{Name: n.Name, IgnitionPath: filepath.Join(dir, "output", n.Name+".ign")})
	}
	return r
}

// validate checks the whole batch before a provider can create compute. Profile
// bytes remain private and never appear in errors. Validation cannot substitute
// for provider-side ownership checks against actual resources.
func (r PreparedGroup) validate(p ComputeProvider) error {
	if !filepath.IsAbs(r.StateDir) || filepath.Clean(r.StateDir) != r.StateDir {
		return fmt.Errorf("prepared compute state must be an absolute directory")
	}
	dir, c, err := loadCluster(Options{Name: r.Name, StateDir: filepath.Dir(r.StateDir)})
	if err != nil {
		return err
	}
	if dir != r.StateDir {
		return fmt.Errorf("prepared compute group identity mismatch")
	}
	for _, path := range []string{dir, filepath.Join(dir, "output")} {
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("prepared compute state and profiles must use private directories")
		}
	}
	actual, err := groupProvider(dir)
	if err != nil {
		return err
	}
	if actual != p.Name() {
		return fmt.Errorf("provider ownership mismatch")
	}
	if r.ImageVersion != FlatcarVersion || r.Architecture != "x86-64" {
		return fmt.Errorf("unsupported prepared compute image")
	}
	if r.Egress != p.Capabilities().Egress {
		return fmt.Errorf("unsupported egress mode for %s", p.Name())
	}
	if len(r.Nodes) != len(c.Nodes) {
		return fmt.Errorf("prepared compute inventory mismatch")
	}
	for i, n := range r.Nodes {
		if n.Name != c.Nodes[i].Name || n.IgnitionPath != filepath.Join(dir, "output", n.Name+".ign") {
			return fmt.Errorf("prepared compute node identity mismatch")
		}
		info, err := os.Lstat(n.IgnitionPath)
		if err != nil {
			return fmt.Errorf("read private Ignition profile for %s: %w", n.Name, err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("Ignition profile for %s must be a private regular file", n.Name)
		}
		raw, err := os.ReadFile(n.IgnitionPath)
		if err != nil {
			return err
		}
		if _, _, err := config.Parse(raw); err != nil {
			// Ignition reports can repeat values from the document. Do not
			// expose auth keys or certificates from a malformed private profile.
			return fmt.Errorf("invalid private Ignition profile for %s; inspect the private file", n.Name)
		}
	}
	return nil
}

func (p qemuCompute) Create(ctx context.Context, r PreparedGroup, out io.Writer) error {
	if err := r.validate(p); err != nil {
		return err
	}
	if r.Region != "" || r.MachineType != "" {
		return fmt.Errorf("QEMU does not accept a cloud region or machine type")
	}
	if r.ImagePath == "" || r.MemoryMiB < 2048 || r.MemoryMiB > 32768 {
		return fmt.Errorf("QEMU requires a prepared local image and 2048–32768 MiB of memory")
	}
	for _, n := range r.Nodes {
		raw, err := os.ReadFile(n.IgnitionPath)
		if err != nil {
			return err
		}
		raw, err = addClusterChannels(raw)
		if err != nil {
			return fmt.Errorf("cannot prepare diagnostic channels for %s; inspect the private Ignition profile", n.Name)
		}
		if err := Create(ctx, Options{Name: n.Name, StateDir: r.StateDir, BaseImage: r.ImagePath, Accel: r.Accelerator, Network: "slirp", MemoryMiB: r.MemoryMiB, profile: raw}, out); err != nil {
			return err
		}
	}
	return nil
}

func (p azureCompute) Create(ctx context.Context, r PreparedGroup, out io.Writer) error {
	if err := r.validate(p); err != nil {
		return err
	}
	if r.ImagePath != "" || r.Accelerator != "" {
		return fmt.Errorf("Azure uses its Flatcar marketplace image and does not accept a local image or accelerator")
	}
	nodes := make([]cluster.Node, len(r.Nodes))
	for i, n := range r.Nodes {
		nodes[i].Name = n.Name
	}
	client, err := newAzureClient()
	if err != nil {
		return err
	}
	return createAzureNodes(ctx, ClusterOptions{Options: Options{Name: r.Name}, AzureLocation: r.Region, AzureVMSize: r.MachineType, AzureSubscription: r.Subscription}, r.StateDir, nodes, out, client)
}

func (p qemuCompute) Start(ctx context.Context, o Options, out io.Writer) error {
	return p.action(ctx, o, "start", out)
}
func (p qemuCompute) Stop(ctx context.Context, o Options, out io.Writer) error {
	return refuseUnsafeStop(o, p.Name())
}
func (p qemuCompute) Status(ctx context.Context, o Options, out io.Writer) error {
	return p.action(ctx, o, "status", out)
}
func (p qemuCompute) action(ctx context.Context, o Options, action string, out io.Writer) error {
	return ClusterAction(ctx, o, action, out)
}
func (p azureCompute) Start(ctx context.Context, o Options, out io.Writer) error {
	return p.action(ctx, o, "start", out)
}
func (p azureCompute) Stop(ctx context.Context, o Options, out io.Writer) error {
	return refuseUnsafeStop(o, p.Name())
}
func (p azureCompute) Status(ctx context.Context, o Options, out io.Writer) error {
	return p.action(ctx, o, "status", out)
}
func (p azureCompute) action(ctx context.Context, o Options, action string, out io.Writer) error {
	dir, _, err := loadCluster(o)
	if err != nil {
		return err
	}
	actual, err := groupProvider(dir)
	if err != nil {
		return err
	}
	if actual != p.Name() {
		return fmt.Errorf("provider ownership mismatch; use %s %s", actual, action)
	}
	return AzureAction(ctx, o, action, out)
}
func (p qemuCompute) Remove(ctx context.Context, o Options, removal RemovalOptions, out io.Writer) error {
	return RemoveGroup(ctx, o, p.Name(), removal, out)
}
func (p azureCompute) Remove(ctx context.Context, o Options, removal RemovalOptions, out io.Writer) error {
	return RemoveGroup(ctx, o, p.Name(), removal, out)
}

func refuseUnsafeStop(o Options, provider string) error {
	dir, _, err := loadCluster(o)
	if err != nil {
		return err
	}
	actual, err := groupProvider(dir)
	if err != nil {
		return err
	}
	if actual != provider {
		return fmt.Errorf("provider ownership mismatch; use %s stop", actual)
	}
	return fmt.Errorf("stop requires a whole-group CockroachDB quorum check and temporary drain, which are not implemented; compute and disks retained")
}
