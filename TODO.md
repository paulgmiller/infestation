# Infestation TODO

Outstanding work, in priority order. [Goals](docs/goals.md) and
[architecture](docs/architecture.md) describe the intended system.

## Consolidate deployment

- [ ] Ensure a working `stop` command is available for both QEMU and Azure,
  preserving disks and private state.
- [ ] Ensure helper builds use `github.com/paulgmiller/infestation`'s published
  `master` branch by default, rather than local workstation source.
- [ ] Show more progress output while `create` runs, including the current stage
  and what it is waiting for.
- [ ] Make the Azure resource group clear in CLI output, including which group
  is being created or used.
- [ ] Verify independent enrollment from separate workstations with two local VMs
  (`--init --size 2`) and one Azure VM (`--size 1`), without shared local state.
  Publish the updated helper source before testing; earlier helper refs lack the
  enrollment endpoint/VMSS identity setup. Include a two-instance Azure VMSS,
  delayed peers and joining while the initializer is down.
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
- [ ] Make Pulumi the common provisioning layer for QEMU, Azure and additional
  VM compute providers; share the node/bootstrap contract across providers.
- [ ] Remove unused deployment paths, duplicated networking/bootstrap code and
  resource configuration. Preserve existing stack identities, disks, CA and token.
- [ ] Remove required cloud object storage from helper delivery, kubeconfig
  retrieval and bootstrap while preserving initialization/recovery safety.
- [ ] Separate user-facing configuration from test fixtures; make the role of
  `examples/cluster.json` explicit instead of using it as an implicit CLI default.
- [ ] Persist complete creation inputs and make explicit teardown ownership and
  partial-failure evidence consistent.

## Reliable bootstrap and networking

- [ ] Replace Azure default outbound access with an explicit egress design;
  evaluate dependency cost, failure scope and NAT traversal.
- [ ] Validate container DNS, delayed peers, MTU, packet loss and pod routes from
  ordinary tailnet clients. [Detailed networking work](docs/networking-todo.md).
- [ ] Add readiness-aware API Service withdrawal/failover and redundant DNS.
- [ ] Choose and test pod NetworkPolicy enforcement.
- [ ] Clean up stale tailnet device, route and Service registrations during
  replacement or teardown so they cannot interfere with the tailnet's cluster.
- [ ] Gate smoke clients on ready endpoints and retain failed-test evidence.
- [ ] Verify kubeconfig publication across credential renewal.

## Data safety and maintenance

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
- [ ] Coordinate Flatcar, K3s and Cockroach updates, drains, rollback and rejoin.
- [ ] Investigate Kine/Cockroach maintenance-status incompatibilities and watch
  timeout logs under sustained WAN latency.
- [ ] Test application storage, placement, zone/site failures and resource sizing.

## Distribution and portability

- [ ] Test native arm64 first boot, enrollment and workloads.
- [ ] Review guest build cost, availability, caching, provenance, update/rollback
  and diagnostics before enrollment.
- [ ] Add VM compute providers once the shared contract and egress design are clear.
- [ ] Design highly available public application ingress using outbound tunnels.

Further questions: [research](docs/research-questions.md). Test evidence lives in
[the mixed-cluster record](docs/mixed-cluster.md), outside the TODO lists.
