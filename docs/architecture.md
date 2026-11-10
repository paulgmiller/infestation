# Architecture

Infestation shares a typed Go inventory, native PKI, and validated Ignition across
providers. The inventory describes roles, addresses, datastore participation,
locality, and TLS SANs. Provider settings describe VM sizes, storage, networks,
firewall rules, and placement. Credentials are per-node, not VMSS-wide secrets.

CockroachDB is a systemd-managed host Docker container, with each K3s server
using Kine over TLS. The tested egress-only path installs host Tailscale, builds
its helper in a pinned Go container, and uses K3s's native Tailscale/Flannel
integration with kube-proxy and CoreDNS. Nodes require outbound connectivity,
not a connected underlay or public API load balancer. The mixed local/Azure
prototype and its repair/verification limits are in [the runbook](mixed-cluster.md).

The older Azure Pulumi/VMSS profiles still install Cilium and use Blob-backed
bootstrap/discovery and public API/egress infrastructure. Their migration is
explicitly tracked in [TODO.md](../TODO.md); source cleanup must preserve stack
identity and database initialization guards. Public Cilium Gateway examples,
web probes/forwarding/firewall rules and the public smoke path are retired. The
2026-10-27 Gateway experiment remains historical evidence in the Azure runbook.

Go packages under `internal/provider` own Pulumi resource lifecycle. Planning
resolves prices, quota and zones once and saves the result; subsequent previews
do not choose a different VM or move a database node because prices changed.
The deployment CLI invokes Pulumi and then uses the bootstrap package. Node
provisioning and bootstrap are separate operations with explicit recovery guards.

Azure uses individually configured Flatcar VMs in a Flexible VMSS. A Standard
Load Balancer has separate API and all-node egress pools, sharing one frontend.
QEMU uses the parameterized libvirt provider, with a private NAT bridge, stable
DHCP reservations and protected copy-on-write disks. Its small in-tree Pulumi
adapter registers the same provider parameters/resource tokens as the generated
SDK, without making generated SDK source a build prerequisite.

AWS EC2 and Google Compute Engine need their own infrastructure packages around
the same portable node preparation. Managed EKS/GKE/AKS would be a different
architecture because they own their Kubernetes control planes.

`infestation azure check` is an opt-in disposable lifecycle check. Its explicit
`smoke_test` topology uses two API servers plus a database-hosting worker, while
normal clusters retain three API servers. It uses the shared deployment and
smoke packages, verifies directed internal DNS/HTTP checks, and deletes only its
fresh, ownership-tagged resource group. The command has local tests; its full
three-node Azure cycle remains unverified. It does not implement upgrades.
