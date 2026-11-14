# Architecture

Infestation is intended to run on any compatible VM compute provider. QEMU and
Azure are the current providers; they are implementations of the provisioning
boundary, not requirements of the cluster architecture. The target is direct Go
provider implementations using Azure Go SDK and go-qemu, with explicit resource
ownership and lifecycle state.

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

Provider implementations share the `internal/vm.ComputeProvider` Go contract
around these operations. Shared preparation passes a private `PreparedGroup` to
the selected adapter; lifecycle dispatch uses the same provider contract. See
[provider contract](provider-contract.md) for disk, Ignition and egress semantics.
Cluster topology, credentials and bootstrap should not depend on a provider's
managed identity, object storage, private DNS or remote command service.
Changes must preserve resource identities, disks and ownership of saved state.

## Cluster configuration and independent enrollment

The intended workflow is to create initial nodes across hosts/providers, then
give friends the tailnet enrollment details and cluster name/endpoint so they can
contribute more VMs. No participant should need a complete list of current or
future nodes, or edit a central inventory when another person adds a VM.

Configuration should have three separate scopes:

- **Cluster settings:** stable cluster identity, tailnet domain, API endpoint,
  network ranges and shared version/policy defaults. No complete node list.
- **Node settings:** that node's identity, selected roles, storage, resources and
  locality. Roles determine whether it joins Kubernetes, the database, or both.
- **Provisioning state:** only the VMs/disks/network resources owned by a person's
  host or provider group. One group need not manage everybody else's machines.

For now there is one cluster per tailnet. Participants can have arbitrary names,
such as `miller`, `sullivan` and `locke`, and come from different people/providers.
Tailnet peer discovery uses `tag:roach` and the helper’s host bootstrap endpoint
on port 7444. A fixed hostname convention is not required for discovery.

The current bootstrap choice is explicit: authorize one founding node with
`--init`. It waits for two database peers, joins an existing database if found,
and otherwise initializes its own store. All other nodes only join. Grouped
provisioning explicitly designates one server; no hostname convention grants
independent nodes initialization authority. See [bootstrap](bootstrap.md).

An automatic one-time election is deferred. A lowest-IP rule can rank candidates,
but cannot prevent different peer views from initializing separate databases.
A future protocol must establish an agreed founding voter set and persist one
initialization target before invoking Cockroach's init RPC. Merely observing
three available peers does not establish the same voter set everywhere.

Once initialization might have occurred, a timeout or unavailable target must
not authorize initialization on a different store. Persist the resulting database
identity and have later nodes join it. Election/formation recovery remains design work. Independent credential
delivery uses shared leaf identities over the tailnet; CA signing keys stay on
the initializer’s workstation.

A joining VM enrolls in Tailscale, contacts the existing cluster's bootstrap
endpoint and obtains the settings/join credentials appropriate to its roles.
Kubernetes and CockroachDB establish membership through their join protocols.
The separate `infestation-smoke` checker discovers all target nodes through the
Kubernetes API using kubeconfig, without provider inventories or cloud credentials.
The initial database/control-plane group still needs an explicit, safely fenced
first-bootstrap step; that group is not a permanent enumeration of the cluster.

Tailnet membership is the chosen administrator trust boundary. Nodes obtain
join material and shared database/Kine leaf credentials from an enrolled peer’s
helper endpoint. The exchange stays on the tailnet, pins the database CA and keeps
CA signing keys out of ordinary node profiles. Small stable bootstrap/seed endpoints are
acceptable; requiring all peers to be known and present is not the target.

### Current implementation

The public interface is `infestation <vm|azure> create`. Every node runs
CockroachDB and K3s; `--agent` selects K3s workers without API servers, while the
default creates servers. Only servers may use `--init`. Both providers share
`start`, `stop`, `remove`, `status`, `wait` and `kubeconfig` verbs. PKI, Ignition,
image preparation and infrastructure helpers are internal implementation steps.

Each provisioning call targets one provider and writes a private `nodes.json`
containing only the nodes that call owns, alongside their shared settings. It is
per provider batch, not a complete cluster membership list.
[Local state](local-state.md) documents the files and their lifecycle uses. For example, `vm create --init
--size 2` and `azure create --size 1` use different local state directories and
may run on different workstations. The node helper discovers runtime membership
over the tailnet instead of requiring the callers to exchange a global inventory.

`--init` creates the cluster CA/token and one-hour SQL credential locally, with
only leaf/join credentials and the temporary SQL credential delivered to the
selected VM. Joining calls generate no second CA/token. They enroll in Tailscale,
fetch the pinned bundle and start Cockroach/K3s with discovered join seeds. See
[bootstrap](bootstrap.md) for the protocol and trust boundary.

The shared `cluster.Config` type still represents both provisioning and runtime
settings. Static examples still use full inventories; splitting
those concerns remains outstanding. Both providers support 1–32 nodes per call.
Azure creates one Uniform VMSS whose capacity comes from `--size`; shared guest
profiles derive identity from IMDS before tailnet enrollment. See
[Azure VMSS behavior](azure-vmss.md). Independent enrollment has protocol/unit
coverage but needs fresh VM verification. The earlier mixed test used a complete
static inventory and does not verify this new path.

## Shared node preparation

The `--init` call generates cluster PKI and a persistent K3s token. Joining calls
render Ignition without those credentials and fetch leaves/token after enrollment. Long-lived
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

Node [locality](locality.md) records provider placement, Azure resource groups and
QEMU physical hosts/sites. These labels aid replica diversity; enforced placement
and verified host/site/zone failure survival remain outstanding.

CockroachDB is a TLS-enabled, systemd-managed host container outside Kubernetes.
K3s servers connect through Kine. The selected initializer performs the guarded
first SQL setup using a separate one-hour credential, then removes it. Its current
persistent-disk marker does not establish safe initializer replacement.

Control-plane nodes advertise a shared Tailscale API Service forwarding raw TCP
to localhost:6443. Kubernetes retains CA/hostname validation and client authentication.
Each server publishes a separate root-only kubeconfig. Tailnet members can retrieve
it through Tailscale SSH or the helper endpoint; routine sessions use `core`
with sudo.

## Implementation boundary

The VM CLI directly starts QEMU processes using a handwritten QMP transport.
Azure uses typed Go SDK objects and clients for resource groups, NSGs, VNets and
Uniform VMSS. Creation waits for each dependency before creating the next;
start/stop/remove wait on SDK long-running operations. Status reads every page
of instance views and prints only identity and health fields. No ARM deployment
template or Azure CLI control-plane subprocess is used.

Azure authentication uses DefaultAzureCredential. Create selects a subscription
from `--subscription` or `AZURE_SUBSCRIPTION_ID`; lifecycle operations use the
saved subscription. See [Azure authentication](azure-vmss.md#authentication).
The shared Go provider contract is implemented; migration to go-qemu remains
outstanding. Public stop refuses until its whole-batch quorum and temporary drain
guards are implemented. Normal remove gates SDK deletion on verified database
decommissioning; explicit abandoned-cluster teardown retains ownership checks.
The Azure SDK path has local mocked transport coverage and needs live validation.

Backward compatibility is not required before the v1 tag. Retired interfaces and
profile formats can be removed without guards or migration support. Private
state and VM disks remain protected from deletion during source refactoring.

The [mixed test](mixed-cluster.md) proves the repaired topology can communicate
across local and Azure networks. It does not prove unattended fresh bootstrap,
arm64 execution, failure recovery, coordinated updates or production availability.
See [goals](goals.md), [networking](networking-plan.md), [bootstrap](bootstrap.md)
and [TODO.md](../TODO.md).
