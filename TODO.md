# Infestation TODO

Outstanding work, with the current priorities first. The provider direction is
direct Go libraries for Azure and QEMU/QMP, replacing Pulumi and legacy deployment
paths. See [goals](docs/goals.md) and [architecture](docs/architecture.md) for the
provider boundary and current implementation.

## Current priorities

- [ ] Gate normal `remove` on completed CockroachDB decommissioning while target
  stores are still online: resolve and verify database node/store IDs against
  owned instances, preflight the remaining capacity and placement constraints,
  wait for replica relocation and completed decommissioning, and verify remaining
  range health before deleting any compute/disks. Refuse unsafe shrink (including
  only two eligible stores remaining with three-replica requirements); require
  replacement capacity rather than silently reducing replication. Use authorized
  operator credentials without depending on the creator workstation or embedding
  long-lived admin keys in node profiles. Drain temporary `stop` targets without
  permanently decommissioning them, and check the whole batch's quorum impact.
  Define retryable checkpoints and a distinct explicit destructive teardown path
  for an abandoned/unavailable cluster. See CockroachDB v26.2
  [node shutdown](https://docs.cockroachlabs.com/docs/v26.2/node-shutdown) and
  [replication controls](https://docs.cockroachlabs.com/docs/v26.2/configure-replication-zones).
- [ ] Define one shared Go provider contract for create/start/stop/status/remove,
  persistent disks, private Ignition delivery and explicit egress. Keep node
  preparation and enrollment provider-independent. Preserve resource ownership
  checks, VM disks and private state across the SDK migrations; backward
  compatibility with retired interfaces/formats is not required before v1.
- [ ] Live-verify typed Azure Go SDK provisioning and lifecycle: credentials and
  subscription selection, VMSS bootstrap, power operations, paginated status,
  canceled/failed creation evidence and retryable owned-group removal. Verify
  disks survive stop and ownership mismatches prevent mutation. Run only when
  provisioning is explicitly requested.
- [ ] Replace handwritten QMP transport with digitalocean/go-qemu's direct socket
  client, without libvirt. Keep QEMU process launch and qemu-img disk preparation
  managed from Go. Preserve existing VM launch settings, private sockets, process
  identity checks, graceful shutdown, bounded waits and retained disks. Test QMP
  errors/disconnects without provisioning.
- [ ] Bulletproof unattended bootstrap: live-verify joiners starting before the
  initializer, delayed enrollment and temporarily unavailable databases. Verify
  automatic recovery of canceled dependency jobs and stuck K3s startup/shutdown
  without manual service commands; fix any remaining gaps.
- [ ] Design durable cluster credential custody independent of creator
  workstations and disposable provider-group state. Choose an off-workstation
  authoritative store and independent encrypted recovery copy for CockroachDB
  CA/signing keys, admin credentials, K3s token and cluster trust/settings. Define
  authorized operator access from a replacement workstation, encryption/key
  custody, availability during cluster outages, rotation and recovery. Never make
  the failed cluster the only source of the secrets needed to restore it. Provider
  removal/replacement must not delete or regenerate credentials. Joining nodes
  receive only permitted leaf/join material, never CA signing or long-lived admin
  keys. Update the workstation-only custody rules when the replacement design is
  selected; do not distribute signing/admin keys through ordinary node profiles.
  Define recovery/discovery of provider ownership metadata so loss of the creator
  workstation does not strand cloud lifecycle operations. Test permanent loss of
  that workstation, enrollment, decommissioning and recovery from another operator
  machine. Credentials alone cannot restore database/Kine data after store loss;
  coordinate with the separate data backup/restore work.
- [ ] Model contributors as failure boundaries across providers/sites. Issue each
  contributor a distinct Tailscale auth key carrying `tag:roach` plus exactly one
  administrator-controlled `tag:contributor-<id>` tag. Derive a leading
  `contributor=<id>` CockroachDB locality tier from the enrolled device's
  `tailscale status --json` tags, preserving the common physical locality tiers;
  reject missing or ambiguous contributor identities before database startup.
  Keep contributor-tag assignment restricted through tailnet `tagOwners`.
  Enforce and verify a surviving voting majority for every range after loss of
  any one contributor's entire contribution, including system and Kine ranges.
  For the three-contributor test, verify one voter per contributor per three-voter
  range, then test withdrawal of all of one contributor's home and Azure nodes.
  Locality preference alone is not a quorum guarantee. See
  [Tailscale auth keys](https://tailscale.com/docs/features/access-control/auth-keys),
  [tag ownership](https://tailscale.com/docs/features/tags) and
  [CockroachDB v26.2 replication controls](https://docs.cockroachlabs.com/docs/v26.2/configure-replication-zones).
- [ ] Replace Azure default outbound access with an explicit egress design;
  evaluate dependency cost, failure scope and NAT traversal.

## Consolidate deployment

- [ ] Ensure a working `stop` command is available for both QEMU and Azure,
  preserving disks and private state.
- [ ] Ensure helper builds use `github.com/paulgmiller/infestation`'s published
  `master` branch by default, rather than local workstation source.
- [ ] Show more progress output while `create` runs, including the current stage
  and what it is waiting for.
- [ ] Make the Azure resource group clear in CLI output, including which group
  is being created or used.
- [ ] Split cluster settings, per-node settings and per-owner state in the Go API;
  remove static fixture/default assumptions about complete membership.
- [ ] Design rotation/revocation of shared database/Kine leaf credentials, cluster
  tokens and cached enrollment bundles while preserving the pinned cluster identity.
- [ ] Future: replace explicit `--init` with automatic one-time bootstrap election over discovered tailnet peers:
  seal an agreed founding voter set, persist votes/initialization target, and
  handle ambiguous init results without authorizing another database. Do not
  treat failure to discover an initialized database as proof none exists.
- [ ] Replace inventory-wide bootstrap/readiness barriers with local join
  readiness and runtime membership discovery.
- [ ] Separate user-facing configuration from test fixtures; make the role of
  `examples/cluster.json` explicit instead of using it as an implicit CLI default.
- [ ] Persist complete creation inputs and make explicit teardown ownership and
  partial-failure evidence consistent.

## Reliable bootstrap and networking

- [ ] Live-verify rejection of incompatible pinned helpers and missing database
  environment values. Verify reboot recreates runtime files without manual repairs.
- [ ] Live-verify status identifies bootstrap blockers, including enrollment/
  membership, failed dependencies, discovered peers and database listeners.
- [ ] Live-verify host/site and IMDS region/zone/subscription/resource-group
  locality after independent enrollment. Plan safe updates of existing profiles
  with placeholder labels. Map Azure logical zones to physical domains across
  subscriptions before claiming cross-subscription zone isolation.
- [ ] Verify three-node database health and directed workload DNS/HTTP connectivity
  after independent enrollment; Kubernetes Ready alone is insufficient evidence.
- [ ] Validate container DNS, delayed peers, MTU, packet loss and pod routes from
  ordinary tailnet clients. [Detailed networking work](docs/networking-todo.md).
- [ ] Add readiness-aware API Service withdrawal/failover and redundant DNS.
- [ ] Choose and test pod NetworkPolicy enforcement.
- [ ] Clean up stale tailnet device, route and Service registrations during
  replacement or teardown so they cannot interfere with the tailnet's cluster.
- [ ] Gate smoke clients on ready endpoints and retain failed-test evidence.
- [ ] Verify kubeconfig publication across credential renewal.
- [ ] Make the CockroachDB DB Console viewable by anyone on the tailnet through
  a stable private HTTPS endpoint, with browser-trusted TLS and no public ingress.
  Define tailnet access policy and dashboard authentication so tailnet members can
  inspect locality, node health, replication and network latency without receiving
  SQL admin credentials. Keep CA signing keys and long-lived admin keys on the
  workstation. Verify access from another member's device and endpoint behavior
  when the serving node is unavailable. See
  [CockroachDB v26.2 DB Console](https://docs.cockroachlabs.com/docs/v26.2/ui-overview).

## Data safety and maintenance

- [ ] Design adaptive CockroachDB voting-replica counts as cluster membership
  grows or deliberately shrinks: consider three voters below ten nodes and five
  above ten, defining behavior at ten, hysteresis and a healthy settling period.
  Three voters require quorum two; five require quorum three. Gate increases on
  eligible healthy capacity and independent contributor/site domains, preserving
  survival of any one contributor's withdrawal. Reconcile effective replication
  and voter settings, including system-range defaults and child overrides, then
  verify actual per-range voter placement after convergence. Reduce replication
  only during an explicit guarded shrink while quorum is healthy, before removing
  capacity; never interpret unreachable nodes as authorization to lower replication
  or treat a voter-count change as recovery from lost quorum. Test transitions,
  threshold flapping, partial convergence and contributor/site outages. See
  [CockroachDB v26.2 replication controls](https://docs.cockroachlabs.com/docs/v26.2/configure-replication-zones).
- [ ] Enforce and verify voting-replica placement across at least three independent
  domains for three-voter ranges, including system and Kine data. Locality labels
  alone are not a verified survival guarantee. Test loss of an entire VMSS and
  an entire QEMU host, and report unsatisfied placement constraints.
- [ ] Make both providers' `stop` and `remove` cordon and gracefully drain owned
  Kubernetes nodes before powering off compute, respecting disruption budgets
  and pod termination grace periods. Keep stopped nodes registered and restore
  scheduling after a successful start. For `remove`, delete the Kubernetes Node
  objects after compute is confirmed stopped so kubelets cannot re-register;
  support retrying cleanup of already stopped/NotReady nodes. Define bounded
  waits, failure behavior and an explicit force path for unavailable clusters.
  Coordinate host-level CockroachDB drain/decommission separately; Kubernetes
  draining does not cover the database or establish safe quorum/data retention.
- [ ] Drain `svc:k8s-api` advertisements before stopping/removing API hosts,
  restore them on start only after API readiness, and test client failover.
  See [Tailscale Service drain](https://tailscale.com/docs/reference/tailscale-cli/serve).
- [ ] Define/test database drain, instance replacement and disk-retention/backup
  behavior for safe member replacement and VMSS scale-in. Explicit group teardown
  is available but does not drain members. Uniform VMSS cannot rely on
  the Flexible-only OS-disk `Detach` setting; power-off is not instance deletion.

- [ ] Configure and verify clock synchronization on every Flatcar node across
  local hosts and cloud providers. Check compatible leap-second smearing, Azure
  host-time interactions, time-source health, startup convergence and drift
  after suspend/reboot or network loss, following CockroachDB's
  [clock synchronization guidance](https://docs.cockroachlabs.com/docs/stable/deploy-cockroachdb-on-premises#step-1-synchronize-clocks).

- [ ] Investigate removing redundant CockroachDB inter-node TLS when all peer
  communication is enforced over Tailscale/WireGuard. Confirm a supported
  configuration and its interaction with SQL client authentication; scope this
  change to database peer traffic, preserving Kubernetes API TLS.
- [ ] Preserve initialization intent and cluster identity across initializer
  replacement/disk loss; test partial joins, lost marker and lost quorum without
  ambiguous reinitialization or new PKI/token. [Bootstrap](docs/bootstrap.md).
- [ ] Define and test coordinated backup/restore of application data, the Kine
  database and matching K3s credentials. [Operations targets](docs/operations-plan.md).
- [ ] Provide an operator-initiated disaster recovery workflow: select and verify
  a usable backup for restore, or explicitly choose manual loss-of-quorum recovery
  when no suitable backup exists. Before modifying stores, stop writes and
  snapshot/copy all surviving stores and preserve matching credentials/cluster
  identity. Collect replica information from every surviving store using the
  matching CockroachDB version, produce a reviewable recovery plan, report missing
  ranges and possible data loss/inconsistency, and require explicit approval before
  applying it. Never initiate forced recovery automatically or treat an unavailable
  cluster as permission to initialize a new one. Test backup restore and forced
  recovery separately, including a range with no surviving replica; verify database
  health and K3s/Kine behavior before resuming workloads. Document the tested
  version-specific procedure and evidence in a recovery runbook.
- [ ] Coordinate Flatcar, K3s and Cockroach updates, drains, rollback and rejoin.
- [ ] Investigate Kine/Cockroach maintenance-status incompatibilities, including
  the rejected `pg_total_relation_size()` query, and watch timeout logs under
  sustained WAN latency. Verify metrics API discovery after startup converges.
- [ ] Test application storage, placement, zone/site failures and resource sizing.

## Distribution and portability

- [ ] Test native arm64 first boot, enrollment and workloads.
- [ ] Review guest build cost, availability, caching, provenance, update/rollback
  and diagnostics before enrollment.
- [ ] Add an AWS provider using the official Go SDK: EC2 Launch Templates and
  Auto Scaling Groups behind the shared provider contract. Coordinate replacement
  and scale-in with database decommissioning and explicit disk-retention rules.
- [ ] Add a Google Cloud provider using the official Go SDK: Instance Templates
  and Managed Instance Groups behind the shared provider contract. Coordinate
  replacement and scale-in with database decommissioning and disk retention.
- [ ] Add a Vultr VM provider behind the shared Go provider contract, with shared
  Ignition/enrollment, explicit egress, ownership checks and disk lifecycle.
  Establish the provider capabilities after the shared contract and egress design
  are clear.
- [ ] Design highly available public application ingress using outbound tunnels.

Further questions: [research](docs/research-questions.md). Test evidence lives in
[the mixed-cluster record](docs/mixed-cluster.md), outside the TODO lists.

## Completed

- [x] Verify independent enrollment from separate workstations with two local VMs
  (`--init --size 2`) and one Azure VM (`--size 1`), without shared local state.
  Publish the updated helper source before testing; earlier helper refs lack the
  enrollment endpoint/VMSS identity setup. Include a two-instance Azure VMSS,
  delayed peers and joining while the initializer is down.
