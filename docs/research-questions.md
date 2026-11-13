# Open design questions

Use [TODO.md](../TODO.md) for priorities. These questions elaborate unresolved
choices within [the project goals](goals.md).

## VM compute provider boundary

What is the smallest common Go provider contract for VM, disk, Ignition delivery and
egress? How should topology, roles and locality stay provider-independent? How do
we preserve state and ownership across partial creation, retries and deletion?
Which host prerequisites and capabilities must a provider report explicitly?

## Helper distribution

What are the cold-build time, disk and memory costs on amd64 and arm64? How do
source, image and module outages affect first boot? What diagnostics are available
before enrollment? How should cached builds, provenance, update and rollback work?
Measure these before choosing a different distribution mechanism.

## Egress and public application access

Which explicit Azure egress mechanism best fits minimal infrastructure, stable
operation, cost and direct Tailscale paths? How does the contract map onto other
VM providers? What is the failure scope of each egress option?

How can outbound application tunnels provide multiple active proxies, health-aware
routing and redundancy? Where should TLS terminate, and how do long-lived streams
behave during proxy replacement? Private Kubernetes API access already has a
separate tailnet Service and must retain its own authentication/TLS semantics.

## Pod policy and observability

Which controller should enforce NetworkPolicy with native K3s Tailscale/Flannel?
How do policy, source preservation, same-node traffic and route approvals interact?
Which host and workload signals distinguish DNS failure, missing route, policy
rejection, relay use and an unhealthy application?

## Disk, application storage and scheduling

Can reconstructible container images/data use temporary storage without risking
database durability? Keep ephemeral and durable data visibly separate and fail
closed when required storage is unavailable. What survives reboot, reimage,
replacement and zone/site loss for each provider?

How should host Cockroach resource use be represented to Kubernetes without
counting it twice? Are readiness/endpoint stand-ins useful? How do scheduler
requests, host limits and OS reservations interact with optional application
requests, memory pressure and evictions?

## Initialization, replacement and recovery

What durable evidence fences initialization across a replaced initializer or
lost marker? How do we distinguish an uninitialized store from an unavailable
existing database? How can partial SQL setup and expired bootstrap credentials
be resumed without reinitialization or regenerated CA/token?

What backup/recovery material is needed beyond the SQL data, and where is it
stored so recovery survives loss of the original workstation? What dataset size,
backup lag and restore throughput support the targets in [operations](operations-plan.md)?

## Coordinated maintenance

Which components should stage updates, drain Kubernetes, drain database traffic,
restart hosts and verify rejoin? How do we preserve quorum and capacity across
these operations? What does rollback mean for a changed database store or OS?
Which compatibility checks must precede each K3s/Flatcar/Cockroach transition?
