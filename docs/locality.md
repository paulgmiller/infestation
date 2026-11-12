# Node locality and failure domains

New node profiles pass these CockroachDB locality tiers in a common order:
`cloud,region,zone,site,subscription,resource_group,host`. Unavailable values are
`unknown`, shared by all nodes with missing information. VM names, ordinals and
create-group names must not invent separate physical failure domains.
CockroachDB v26.2 expects consistent keys/order and uses the earlier tiers first
when diversifying replicas. See [locality semantics](https://docs.cockroachlabs.com/docs/v26.2/cockroach-start#locality).

## QEMU

All VMs on a workstation use the same `host` label, including VMs from separate
create calls. By default it is a namespaced SHA-256 digest of `/etc/machine-id`
(128 bits retained); the raw machine ID is never put in a profile. Use `--host`
to supply a stable physical-host label if machine-id is unavailable, cloned, or
changes after reinstalling the host. Labels must be unique per physical host
across the tailnet, and remain the same across calls on that host.

Use `--site` for a shared home/building/power/router boundary across multiple
hosts. Choose the same site label for all calls at that location. An unspecified
site is `unknown`; it is not inferred from the hostname, VM ordinal or public IP.
Host/site labels accept letters, digits, dots, underscores and dashes.

```sh
infestation vm create --name lab --host desktop-a --site miller-home \
  --tailnet-domain YOUR-TAILNET.ts.net --tailscale-auth-key-file secrets/tsauth
```

This records `cloud=qemu,region=unknown,zone=unknown,site=miller-home,subscription=unknown,resource_group=unknown,host=desktop-a`.
For QEMU, physical region and cloud availability zone remain unknown.

## Azure

The guest validates IMDS against its configured VMSS identity, then records the
actual region, logical zone, subscription and resource group before enrollment.
Group/subscription casing is normalized. Missing zone information is `unknown`;
it never falls back to an instance number. The host and site remain unknown.
The workstation's `nodes.json` contains requested region/group and unknown
runtime fields; the guest's bootstrap configuration and CockroachDB locality
contain the IMDS-resolved values.

New VMSS templates use compute API `2026-03-01` with top-level
`placement.zonePlacementPolicy: Auto`. Azure selects zones; no zone list,
include/exclude list, maximum zone count or strict balancing policy is requested.
See [Azure VMSS placement](https://learn.microsoft.com/en-us/azure/templates/microsoft.compute/2026-03-01/virtualmachinescalesets).
Actual placement and available capacity must still be inspected after creation.

Azure logical zone numbers are subscription-specific. Zone 1 in two
subscriptions need not identify the same physical zone. Cross-subscription
physical zone mapping is not implemented; do not use these labels as proof of
physical isolation. See [Azure zone mapping](https://learn.microsoft.com/en-us/azure/availability-zones/az-overview).

Resource groups and subscriptions are administrative failure/removal boundaries,
not physical locations. They can span zones. Their tiers follow physical placement
so group diversity does not take priority over zone diversity. CockroachDB's
hierarchical locality cannot simultaneously enforce every overlapping boundary;
recording a group does not guarantee survival of deleting that group. The current
public create path has one VMSS per dedicated group.

## What survival is established?

Zonal failure survival is not yet established. CockroachDB's locality-aware
allocator seeks diversity, but availability requires a surviving voting majority
for **every range**, including system and Kine ranges. Three VMs on one QEMU host
are one host failure domain. Two VMs at home plus one in Azure cannot survive
loss of the home if all three hold a three-voter range.

There is no enforced cluster-wide placement policy or range-placement readiness
check yet. Before claiming survival, inspect effective replication/voter constraints
and every range's placement after convergence, ensure sufficient independent
capacity, and test entire host/site/zone/group loss along with Kine/API continuity.
See [CockroachDB v26.2 replication controls](https://docs.cockroachlabs.com/docs/v26.2/configure-replication-zones).

Other overlapping domains include shared power, router/ISP, storage, owner and
account permissions. An observed public IP can suggest common egress, but carrier
NAT, proxies, VPN exits, dynamic addressing and multiple egress paths prevent it
from proving a common site or independent connectivity. Public-IP discovery and
owner-based withdrawal coordination are deferred; neither is part of placement.

Existing private state, running nodes and disks are not rewritten by these source
changes. Older profiles retain their old labels until an explicitly planned update.
The default guest helper is built from published source: publish the compatible
helper before creating profiles that depend on these changes. No live provisioning
or failure test is part of local validation.
