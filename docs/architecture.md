# Architecture

Infestation is intended to run on any compatible VM compute provider. QEMU and
Azure are the current providers; they are implementations of the provisioning
boundary, not requirements of the cluster architecture. Pulumi is the preferred
layer for describing infrastructure, previews, resource ownership and lifecycle.

## Provider contract

A provider supplies:

- A Flatcar-compatible VM with the requested architecture and resources.
- Persistent disk and explicit storage lifecycle/ownership.
- A way to deliver private Ignition configuration at first boot.
- A NIC and working outbound connectivity, using whatever networking plumbing
  that provider requires.
- VM/disk status, start, stop and deletion operations with safe retry behavior.

Nodes do not require a shared VNet/VPC, peering, inbound port forwarding, a
public node address or an external ingress load balancer. A cloud can require
its own VNet/subnet for a NIC without making that network the cluster transport.
Egress infrastructure is provider-specific and remains an explicit design choice.

Provider implementations should be small Pulumi components around this contract.
Cluster topology, credentials and bootstrap should not depend on a provider's
managed identity, object storage, private DNS or remote command service.
Existing Pulumi project IDs must remain stable so changing code does not replace
resources or lose ownership of stored state.

## Shared node preparation

The Go inventory describes node identities/roles, versions, locality, storage and
TLS SANs. Go generates PKI, a persistent K3s token and validated Ignition. Long-lived
administrator keys and CA signing keys stay on the workstation. Ignition installs
configuration and systemd services; it is first-boot configuration, not continuous
reconciliation.

A pinned multiarchitecture Go container fetches an immutable source commit and
builds the native helper on each node. It mounts only a temporary output directory.
The helper is installed atomically and cached with its identity/checksum. Building
runs before tailnet enrollment, using ordinary outbound internet access.

Host Tailscale supplies peer connectivity. The helper enrolls the node, validates
its identity and writes the assigned address into K3s configuration. K3s's native
Tailscale/Flannel integration handles API-assigned PodCIDRs. kube-proxy implements
ordinary Services and CoreDNS provides Kubernetes DNS.

## Database and access

CockroachDB is a TLS-enabled, systemd-managed host container outside Kubernetes.
K3s servers connect through Kine. The selected initializer performs the guarded
first SQL setup using a separate one-hour credential, then removes it. Its current
persistent-disk marker does not establish safe initializer replacement.

Control-plane nodes advertise a shared Tailscale API Service forwarding raw TCP
to localhost:6443. Kubernetes retains CA/hostname validation and client authentication.
Each server publishes a separate root-only kubeconfig. Tailnet members can retrieve
it through Tailscale SSH; routine sessions use `core` with sudo.

## Implementation boundary

The tested VM CLI directly starts QEMU processes and provisions the Azure test
through ARM and the Azure CLI. Pulumi provider packages also exist. These paths
are not yet unified: consolidating them under Pulumi is outstanding work, not an
already implemented provider abstraction.

The [mixed test](mixed-cluster.md) proves the repaired topology can communicate
across local and Azure networks. It does not prove unattended fresh bootstrap,
arm64 execution, failure recovery, coordinated updates or production availability.
See [goals](goals.md), [networking](networking-plan.md), [bootstrap](bootstrap.md)
and [TODO.md](../TODO.md).
