# Operations goals

These are operating targets and open decisions, not demonstrated guarantees.
Current work is tracked in [TODO.md](../TODO.md); the system goals are in
[goals.md](goals.md).

## Availability and recovery targets

- Tolerate losing one availability zone with sufficient remaining capacity for
  Kubernetes, CockroachDB and application workloads.
- After a larger outage, support operator-triggered recovery into another region
  without pre-created compute/networking. The recorded target is a healthy
  restored cluster within one hour, with at most fifteen minutes of data loss.
  Dataset size, backup lag and measured restore throughput remain open.
- Restore application databases and the Kine database with matching K3s recovery
  inputs. Hold applications until their required databases are ready.
- Keep recovery possible without the original workstation. Choose secure backup
  and recovery-material storage without making it a normal bootstrap dependency.
- Define backup retention, replication lag and complete backup-chain preservation.
  The recorded retention target is one day of recovery points; cadence alone
  does not establish an achievable recovery point.
- Keep traffic cutover operator-controlled. Recovery targets must distinguish a
  healthy restored cluster from external DNS/routing changes.

## Maintenance and resource ownership

CockroachDB remains a host service outside Kubernetes. Preserve its direct/local
Kine bootstrap path. Define how the scheduler accounts for host database CPU and
memory without double-counting requests and host reservations.

Initial sizing proposals were a 500m CPU request and 2 GiB database memory budget,
with CPU bursting, plus 500m CPU and 1.5 GiB for OS/K3s. These require measurement;
application requests can omit demand that the scheduler then cannot account for.
Test pressure, eviction, OOM and graceful database draining before adopting limits.

Updates must coordinate Flatcar reboots, K3s extensions and database upgrades one
node at a time. A pinned source image does not prevent a later boot from applying
an already staged OS update. Define rollback, store rejoin and compatibility
checks before automating maintenance.

Resource lifecycle must preserve disk ownership, CA/token and expected cluster
identity. Never treat a missing marker, ambiguous initialization result or lost
quorum as authorization to initialize another database. Replacement/decommission
is a distinct operation from starting or retrying provisioning.

## Access and observation

Use tailnet SSH for operator access, normally `core` with sudo, and the shared
private API Service with Kubernetes TLS validation. Publish kubeconfig locally
and refresh it after renewal. Preserve private diagnostics without logging keys,
auth URLs, profiles or kubeconfig contents.

Alerting integration remains deferred. Retain enough health, routing, latency,
resource and lifecycle evidence to support diagnosis. Database license activation
and renewal also need an explicit operator-owned workflow if required.

The [mixed test record](mixed-cluster.md) documents current evidence and limits.
Backup/restore, failure/replacement drills, coordinated maintenance, production
sizing and the recovery targets above remain unverified.
