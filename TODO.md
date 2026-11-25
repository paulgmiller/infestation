# Infestation TODO

Outstanding work, with the current priorities first. The provider direction is
direct Go libraries for Azure and QEMU/QMP, replacing Pulumi and legacy deployment
paths. See [goals](docs/goals.md) and [architecture](docs/architecture.md) for the
provider boundary and current implementation.

## Current priorities

- [ ] Replace the upgrade command's `kubectl` subprocess drain with direct
  Kubernetes API calls through a typed Go client. Cordon only the selected node,
  evict pods through the eviction API while respecting disruption budgets, and
  wait for deletion with bounded retries, cancellation and actionable pod-level
  errors. Preserve the current policies: ignore DaemonSet-managed pods, refuse
  unmanaged pods, and permit `emptyDir` data deletion. Keep a failed upgrade
  cordoned until recovery is verified; never log kubeconfig credentials. Test
  eviction refusal/throttling, timeouts, interrupted drains and safe retries.

- [ ] Fix Kine/CockroachDB compaction reliability: handle serialization failures
  by retrying the complete transaction without exiting K3s. Verify the pinned
  Kine implementation's explicit serializable compaction and fatal commit path;
  test concurrent API writes and multiple servers compacting, retry limits,
  cancellation and unchanged revision/compaction correctness. Investigate slow
  Kine list/count scans and compaction backlog with representative data sizes.

- [ ] Extend the single-instance QEMU `upgrade` command into a provider `reprovision`
  command for an explicitly selected
  owned node. Own profile generation, per-range quorum/capacity and local-volume
  checks, Kubernetes drain/API withdrawal, recovery artifacts, retained-state
  reset, compute restart, identity/SQL/API verification and uncordoning. Persist
  progress for safe retries and actionable failures; no operator shell checklist
  or workstation-only file hunt. Preserve disks, SSH/Tailscale/K3s identity and
  cluster credentials, and coordinate the separate persistent-disk layout.

- [ ] Add whole-batch, per-range voting-quorum preflight to temporary stop before
  any power-off. Successful database drains do not establish quorum after the
  selected batch stops. Check actual voters against live surviving stores,
  account for already unavailable members and transitional replicas, and refuse
  unsafe batches without changing replication. Test individually drainable nodes
  whose combined shutdown loses quorum, preexisting dead voters, partial failures
  and concurrent membership changes. Keep the explicit outage override separate.

- [ ] Design user-facing CockroachDB licensing setup to replace the shared test
  license default: explain eligibility, obtaining and privately supplying a key,
  telemetry requirements, renewal and expiry. Preserve an installed operator
  license, expose actionable license/throttling health before K3s startup, and
  test operation beyond the initial seven-day unlicensed grace period. See
  [CockroachDB v26.2 licensing](https://docs.cockroachlabs.com/docs/v26.2/licensing-faqs).

- [ ] Tune VM RAM and CockroachDB `--max-sql-memory` / `--max-go-memory`
  together under representative Kine and Kubernetes workload. Measure GC CPU,
  collection rate, query memory failures and latency, total database RSS and
  Kubernetes workload headroom. Choose explicit budgets for the host database,
  K3s/system services and pods; account for host services in Kubernetes allocatable
  memory. Validate defaults across supported VM sizes and providers without
  treating the Go soft limit as a total-process memory cap.

- [ ] Live-verify default-on [QEMU host autostart](docs/autostart.md) after reboot or
  power loss: recovery of saved disks and tailnet connectivity, intentional-stop
  exclusion and boot-service behavior on the compute host.

- [ ] Close the remaining historical gaps in the replica-replacement incident:
  recover n1/n3/beeroach identity mappings and removal times, effective dead-store
  and suspect settings, constraints/capacity, and which suspect predicate renewed
  on s5 during the reported Ready interval. The captured logs establish suspect
  destination rejection; source explains why a suspect store can still count as
  live, but neither proves its root cause or continuous duration. Use the
  [investigation and collection runbook](docs/quorum-investigation.md).
- [ ] Implement the missing guest `retire` helper behind the
  [normal removal guard](docs/removal.md), then live-verify strict whole-batch
  CockroachDB decommissioning, placement/capacity refusal, zero-replica completion
  and healthy survivors before compute deletion. Implement safe retry reconciliation
  when an earlier attempt has already stopped/deleted some verified targets;
  current normal retries require every target identity reachable.
  Coordinate Kubernetes workload drain/API withdrawal and credential custody with
  their separate TODOs. Never lower replication to bypass insufficient capacity.
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
  workstations and disposable provider-group state. Every enrolled node must be
  able to obtain the authorized files and credentials needed for upgrades and
  database administration, including license changes, without access to the
  original client workstation. Include desired configuration, version pins,
  Ignition regeneration inputs and provider ownership records as well as secrets;
  locating an old workstation state directory must not be an operational step.
  Choose an off-workstation authoritative store and independent encrypted
  recovery copy for CockroachDB
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
  the creator workstation and all its local files: from each surviving node,
  obtain the required material, perform an authorized database/license change,
  and prepare/apply a rolling upgrade. Also verify enrollment, decommissioning
  and recovery from another operator machine. Credentials alone cannot restore
  database/Kine data after store loss; coordinate with the separate data
  backup/restore work.
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

- [ ] Expose Azure per-instance start/stop selection with saved ownership checks.
- [ ] Live-verify [temporary stop](docs/removal.md#temporary-stops) across local and
  SSH-backed QEMU and Azure, including partial drain/power failures, retries, and
  restarting with the same disks and membership.

- [ ] Add a shared `scale` command for owned provider groups, including local and
  SSH-backed QEMU groups and Azure VMSS. Scale up through shared node preparation
  and enrollment; scale down only after explicit target selection, whole-batch
  CockroachDB quorum/replica evacuation checks and Kubernetes draining. Preserve
  surviving identities, private state and disks according to explicit retention
  policy; reconcile partial failures without replacing existing resources. Test
  growth, shrink, insufficient capacity and interrupted/retried operations.

- [ ] Live-verify automatic final shutdown in the [three-VM lifecycle test](docs/vm-lifecycle-test.md)
  after both passing and failing assertions. Prevent/diagnose scratch-quota
  exhaustion before it produces guest disk-write errors, and verify graceful
  cleanup without forced power-off. The first lifecycle probe passed but final
  local shutdown hit quota-related guest I/O failures; evidence is in the runbook.
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

- [ ] Add temporary isolated tailnets for E2E runs using the alpha
  [Tailscale Tailnets API](https://tailscale.com/docs/features/tailnets-api).
  Accept an explicitly configured OAuth/federated credential with the `tailnets`
  scope; create the test tailnet, tagged enrollment keys, route/API Service/SSH
  policies and an isolated test-runner client for SQL/Kubernetes access. Preserve
  host management connectivity and avoid switching the hosts' normal tailnets.
  Shut down test compute before revoking keys/deleting the temporary tailnet,
  handle failed/interrupted cleanup, and test isolation and credential redaction.

- [ ] Add an opt-in E2E test with two independently created `--init` servers,
  including concurrent startup and different peer-discovery timing. Verify that
  conflicting founding authorities cannot silently form divergent clusters,
  overwrite CA/token state, or reinitialize an existing store. Require explicit
  refusal/fencing and preserve both groups' disks and credentials for diagnosis;
  verify safe behavior when one initializer is delayed or temporarily unreachable.

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
- [ ] Surface host CockroachDB health in Kubernetes and mark database-hosting
  nodes NotReady when their database/store is unavailable or suspect and cannot
  accept replicas. Expose actionable Node conditions/events with database node
  and store IDs, liveness/suspect state, replica-placement blockers and lost-quorum
  details so `kubectl describe node` explains the failure. Integrate with kubelet
  readiness without racing its managed Ready condition; define debounce and
  recovery behavior for transient suspect periods and restore readiness after
  stable database health. Keep local store health distinct from cluster-wide
  range health and avoid blocking database recovery when Kubernetes is unavailable.
  Test that Kubernetes cannot report a database-hosting node Ready while its
  store remains ineligible due to suspect status, plus recovery and API outages.
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

- [ ] Separate disposable OS disks from persistent node state across providers.
  Define the retained layout for CockroachDB stores, Tailscale identity, K3s
  state/local volumes, leaf credentials and bootstrap/recovery markers. Add an
  explicit migration for existing single-disk nodes, mount/ownership guards and
  independent retention/backup policy; never format a retained state disk during
  OS provisioning. Test replacing only the OS disk with regenerated Ignition,
  preserving node/store identity and data, missing/wrong state disks, rollback,
  and interrupted migration. Coordinate with immutable upgrades and credential
  custody; migration must not depend on the creator workstation surviving.

- [ ] Benchmark nodes before admitting them to CockroachDB membership, especially
  voting-replica placement. Measure durable disk write/fsync latency, CPU capacity,
  scheduling stalls and tailnet latency/loss under representative sustained load;
  report distributions and worst stalls rather than averages alone. Define
  configurable minimum thresholds and a stable observation period, with clear
  results visible through CLI diagnostics and Kubernetes conditions/events.
  Quarantine failing or unmeasured stores before database join so they cannot
  receive voting replicas; define whether they may contribute Kubernetes worker
  capacity independently. Recheck after admission to detect degradation, with
  guarded replica evacuation that preserves quorum and capacity. Do not infer
  fitness from VM/provider type or Kubernetes Ready, automatically delete failing
  nodes, or equate a benchmark failure with proven cause of a liveness incident.
  Test admission/refusal, overrides, transient stalls and insufficient healthy
  founding capacity without silently weakening replication requirements.
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
- [ ] Investigate and build a CockroachDB system extension, with possible
  contribution to Flatcar's sysext-bakery. Package only pinned binaries and
  service definitions; keep database stores, certificates, configuration and
  bootstrap state outside the read-only extension. Check licensing/distribution,
  supported architectures, library dependencies and release maintenance. Test
  clean startup, retained-store upgrades and service lifecycle against the current
  Docker packaging before adopting it; an extension does not coordinate database
  quorum, data-format migrations or rollback safety.
- [ ] Implement and test immutable Flatcar, K3s and Cockroach upgrades with
  regenerated Ignition, coordinated drains, rollback and rejoin. Choose and
  validate reprovisioning with explicit retained paths or separate persistent
  data disks; preserve database stores, tailnet/K3s identity, credentials and
  bootstrap markers. Test configuration-only and supported version transitions
  on a three-node cluster, one member at a time, with recovery/health gates.
  Support the K3s 1.35 → 1.36 → 1.37 path without skipping minors; replace the
  enrollment helper's exact software-pin equality requirement with validated
  rolling-version compatibility and coherent desired-version propagation for new
  joiners during rollout. Existing-member upgrades update both local pins and the
  retained join bundle; the current command does not coordinate new enrollment.
  See the [upgrade design notes](docs/cockroach-memory-rollout.md).
  Cap rolling batches by a configurable percentage, choose specific nodes that
  preserve per-range voting quorum, Kubernetes disruption budgets and API
  availability, and verify recovery before each next batch.
- [ ] Investigate Kine/Cockroach maintenance-status incompatibilities, including
  the rejected `pg_total_relation_size()` query, and watch timeout logs under
  sustained WAN latency. Verify metrics API discovery after startup converges.
- [ ] Test application storage, placement, zone/site failures and resource sizing.

## Distribution and portability

- [ ] Cache the pinned guest helper builder image and Go module/build inputs
  across fresh E2E VMs. Key caches by architecture, helper source commit and
  builder digest; verify artifacts before reuse and keep credentials out of cache
  inputs/outputs. Measure cold versus warm stage timings and retain a cold-cache
  bootstrap scenario.
- [ ] Evaluate a prebuilt guest-helper option for fast E2E runs while keeping
  standard Flatcar images and the current pinned-source build path available.
  Preserve architecture/provenance/checksum validation and credential isolation;
  do not introduce required provider object storage or reuse booted VM identities.

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
