# QEMU provider

Local VMs use persistent copy-on-write disks and independent NAT networking.
Cluster traffic uses Tailscale; a shared bridge, host port forward or common
private subnet is not required.

- [Standalone enrollment](../../docs/friend-vm.md)
- [Three-node local cluster](../../docs/isolated-vm-cluster.md)
- [Mixed QEMU/Azure cluster](../../docs/mixed-cluster.md)

The VM CLI starts QEMU directly. The infrastructure package also contains a
Pulumi entry point with project ID `flatcar-k3s-qemu`. Consolidating provisioning
under the common Pulumi contract is outstanding work; preserve existing stack
IDs, state and disks while doing so. See [architecture](../../docs/architecture.md)
and [TODO.md](../../TODO.md).
