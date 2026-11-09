package qemu

import (
	"encoding/json"
	"fmt"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
)

// Register the same parameterized package as the generated libvirt SDK. Keeping
// this small adapter in-tree avoids requiring 13 MB of generated SDK source to
// build a fresh checkout. Resource tokens/versions remain unchanged for state.
func packageRef(ctx *pulumi.Context) (string, error) {
	return ctx.GetOrRegisterPackageRef("libvirt:0.9.9", func() (*pulumirpc.RegisterPackageRequest, error) {
		value, err := json.Marshal(map[string]any{"remote": map[string]string{"url": "registry.opentofu.org/dmacvicar/libvirt", "version": "0.9.9"}})
		if err != nil {
			return nil, err
		}
		return &pulumirpc.RegisterPackageRequest{Name: "terraform-provider", Version: "1.5.0", Parameterization: &pulumirpc.Parameterization{Name: "libvirt", Version: "0.9.9", Value: value}}, nil
	})
}

type libvirtResource struct {
	pulumi.CustomResourceState
	Name pulumi.StringOutput `pulumi:"name"`
	Path pulumi.StringOutput `pulumi:"path"`
}
type provider struct{ pulumi.ProviderResourceState }

func newProvider(ctx *pulumi.Context, name string, args pulumi.Map) (*provider, error) {
	ref, err := packageRef(ctx)
	if err != nil {
		return nil, err
	}
	r := &provider{}
	err = ctx.RegisterPackageResource("pulumi:providers:libvirt", name, args, r, ref, pulumi.Version("0.9.9"))
	return r, err
}
func newResource(ctx *pulumi.Context, module, kind, name string, args pulumi.Map, opts ...pulumi.ResourceOption) (*libvirtResource, error) {
	ref, err := packageRef(ctx)
	if err != nil {
		return nil, err
	}
	r := &libvirtResource{}
	opts = append([]pulumi.ResourceOption{pulumi.Version("0.9.9")}, opts...)
	err = ctx.RegisterPackageResource(fmt.Sprintf("libvirt:index/%s:%s", module, kind), name, args, r, ref, opts...)
	return r, err
}
