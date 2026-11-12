# Azure node groups

`infestation azure create --name cloud --size 2` creates one **Uniform VM Scale
Set** with capacity two. Each instance runs CockroachDB and K3s, with an API
server by default or a K3s agent when `--agent` is supplied. The CLI currently
accepts 1–32 instances per call, matching the local provider's count bound.
Azure's `sku.capacity` is the instance count; see the
[VMSS schema](https://learn.microsoft.com/en-us/azure/templates/microsoft.compute/2024-07-01/virtualmachinescalesets#sku).

The deployment contains the VMSS, a VNet/subnet and NSG. Instances share that
subnet, but cluster traffic uses Tailscale; the NSG denies unsolicited inbound
traffic. No load balancer, public IP, object storage, managed identity, private
DNS zone or remote command extension is added. The prototype still uses Azure
default outbound access; the explicit-egress decision remains in TODO.md.

## Identity and bootstrap

The creator resolves helper source once and supplies one shared private Ignition
profile. A host service runs after the Go helper build and before Tailscale:

1. Read Azure instance metadata and validate the set, group, region and instance
   resource ID.
2. Derive the actual node name `NAME-<instance-id>`; update the hostname, K3s
   identity and tailnet settings before enrollment.
3. Retain `--init` only on actual instance 0 of an explicitly initialized group.
   Every other instance removes its temporary SQL root credential and overrides
   the shared bootstrap command to omit `--init`.
4. Enroll, discover peers and supply runtime database advertise/join addresses.

Shared node/Kine leaf credentials and the one-hour SQL bootstrap exception are
as documented in [bootstrap](bootstrap.md). CA signing and long-lived SQL admin
keys remain on the workstation. Guest cleanup does not remove copies from
private provider state or the original custom-data profile.

No automatic initializer election/failover is added. The initial database still
requires three listening participants across the tailnet, which may come from
other hosts/providers.

## Lifecycle and state

The CLI saves `azure-scaleset.json` with the subscription, group, set name,
region, initial capacity and ownership identity. Saved template/parameters and
node inventory remain private. Start/stop/status operate the entire owned set;
resource-group ownership is checked before VMSS operations. Earlier single-VM
groups remain controllable through their saved records and are not migrated.

Upgrade policy is Manual, overprovisioning is disabled, and no autoscale or
instance-repair policy is configured. Start/stop do not resize the set.
[VMSS upgrade policies](https://learn.microsoft.com/en-us/azure/virtual-machine-scale-sets/virtual-machine-scale-sets-upgrade-policy).

Stop deallocates compute while retaining disks. Instance deletion/scale-in is a
different operation: the Uniform template cannot use the Flexible-only OS-disk
`deleteOption: Detach`. The CLI does not expose scale-in or deletion, and safe
drain/data retention/backup remains outstanding. The disk setting's scope is in
the [OS-disk schema](https://learn.microsoft.com/en-us/azure/templates/microsoft.compute/2024-07-01/virtualmachinescalesets#virtualmachinescalesetosdisk).

Creation refuses existing groups/state. A failed deployment retains its partial
resources and evidence; there is no partial repair command.

## Verification

Unit/mocked tests cover VMSS capacity, shared secure profile, permitted resources,
manual upgrade mode, host identity ordering, instance-zero authority, temporary
credential cleanup and lifecycle ownership guards. The shared profile is checked
against the custom-data size limit. This VMSS path has not been provisioned live;
the earlier mixed test used standalone Azure compute and does not verify it.
The updated helper code must be published before a fresh VMSS boot test.
