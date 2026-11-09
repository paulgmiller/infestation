# Architecture

Infestation shares a typed Go inventory, native PKI, and validated Ignition across
providers. The inventory describes roles, addresses, datastore participation,
locality, and TLS SANs. Provider settings describe VM sizes, storage, networks,
firewall rules, and placement. Credentials are per-node, not VMSS-wide secrets.

CockroachDB is a systemd-managed host Docker container. Each K3s server connects
through Kine to its configured TLS SQL endpoint. Workers run agents and host the
database by default. Cilium is installed after the Kubernetes API becomes ready;
new profiles disable kube-proxy and enable Cilium's replacement. Cilium connects
to the direct API endpoint, never the Kubernetes Service VIP during bootstrap.
Azure uses the public API load balancer on 6443; local/static profiles use their
configured API endpoint or a server address. The bootstrap Helm job uses host
networking and the local API. Ports 80/443 are reserved for future gateway use;
the optional Azure `--gateway` plan flag adds LB forwarding, and the example
HelmChartConfig/Gateway manifests enable the listeners.

The 2026-10-27 gateway experiment verified Cilium replacement on four nodes,
12 directed DNS/Service checks, and public HTTP through Envoy. It required a
worker startup repair: `disable-kube-proxy` is a server-only K3s flag; agents
inherit that setting. Reboot, gateway failover and a clean unattended fresh
bootstrap with the corrected profiles remain unverified.

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
smoke packages, verifies public HTTP via Cilium Gateway, and deletes only its
fresh, ownership-tagged resource group. The command has local tests; its full
three-node Azure cycle remains unverified. It does not implement upgrades.
