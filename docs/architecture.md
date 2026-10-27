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
gateway resources and forwarding rules are not yet installed.

This networking change has local test coverage only. The recorded Azure
bootstrap/connectivity deployment used kube-proxy. Fresh bootstrap, all-node
access through the public LB (including backend-to-frontend connections), reboot,
and service/gateway failover still require deployment tests.

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
