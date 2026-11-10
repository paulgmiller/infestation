# Networking and bootstrap review TODOs

Restored after accidental deletion. The overall prioritized index is
[TODO.md](../TODO.md). This document retains the detailed review items from the
[mixed local/Azure test](mixed-cluster.md), which worked after recorded repairs.

## Before calling deployment unattended

- [ ] Replace Azure default outbound access with explicit egress. The prototype
  set `defaultOutboundAccess: true`. New-subnet defaults changed with APIs after
  March 31, 2026; existing networks are not subject to a blanket shutdown. Compare
  NAT Gateway, Standard LB outbound rules and per-VM public IP with inbound denied
  against the minimal-infrastructure goals. Decide the tradeoff explicitly, then
  test boot with default outbound disabled: DNS, source/registry/module downloads,
  time synchronization, Tailscale control/DERP and direct peer paths. Record cost,
  failure scope and NAT behavior. [Azure guidance](https://learn.microsoft.com/en-us/azure/virtual-network/ip-services/default-outbound-access).
- [ ] Repeat a fresh mixed deployment with container DNS, publisher executable
  path, Azure login-profile and image-resume fixes present from first boot. Require
  no SSH repair or reboot; preserve failure logs before attempting recovery.
- [ ] Verify explicit Quad100 DNS inside Cockroach host containers across resolver
  configurations, especially systemd-resolved split DNS. Test a node starting long
  before its peers exist. Join retries were working; the observed failure was DNS.
- [ ] Verify kubeconfig publication at initial readiness and credential renewal.
  Keep the source unchanged, output atomic/root-only and retrieval over tailnet SSH.
- [ ] Test native arm64 container build and Flatcar boot. Check immutable source,
  multiarch image digest, CGO-disabled output and cached restart behavior.

## Resilience before production use

- [ ] Define durable initialization intent and expected cluster identity across
  initializer replacement or disk loss. The current guard exists on node 0's
  persistent disk. Test lost marker, missing initializer, partial joins and lost
  quorum without blindly reinitializing or regenerating CA/token.
- [ ] Define and test backups/restore for Cockroach and matching K3s recovery
  credentials/token/CA. Reconcile the older Blob-backed operations proposal.
- [ ] Add API readiness monitoring and Service drain/withdrawal; test a failed
  API on a live Tailscale host, host loss and client reconnection.
- [ ] Coordinate Flatcar updates, drains, database maintenance, rollback and rejoin.
  The Azure reboot applied a staged OS update despite the pinned original image.
- [ ] Investigate Kine's unsupported `pg_total_relation_size()` status calls and
  watch/resource-version timeout logs under sustained WAN operation.
- [ ] Spread redundant CoreDNS pods and test DNS during node outage/rescheduling.
- [ ] Choose and test NetworkPolicy enforcement, including same-node and cross-node
  traffic. Distinguish workload policy from host/tag/subnet access rules.
- [ ] Measure MTU, loss, throughput and long-lived connections on direct and relay
  paths; ten DISCO samples per pair are only a short host RTT baseline.

## Simplification

- [ ] Consolidate helper delivery, enrollment, bootstrap, readiness and kubeconfig
  retrieval across QEMU/cloud; deliberately retire old Blob and Cilium paths.
- [ ] Review cold guest builds: dependencies, bounded disk use, failed-container
  cleanup, retry/backoff, logs before enrollment, provenance and update/rollback.
- [ ] Persist complete create inputs for safe resume and an ownership-checked
  teardown workflow. Never recreate disks or PKI as a generic retry.
- [ ] Gate smoke clients on ready web endpoints; three initial mixed-test attempts
  hit connection-refused errors before retries succeeded.
- [ ] Make cluster Service identities and CIDRs explicit; clean up stale device,
  subnet and Service registrations before old clusters can rejoin a reused VIP.
- [ ] Reconcile documentation/defaults with tested Tailscale behavior and mark
  legacy Azure VMSS/Cilium paths explicitly.

## Evidence and teardown

- [x] Save all six directed latency paths, database health, SSH kubeconfig checks
  and Azure resource types. Private raw evidence remains in ignored test state.
- [x] Verify the mixed local QEMU processes stopped and Azure group `mix-1110-rg`
  was deleted, including its managed disk. Local disks, credentials and reports
  were preserved. Tailnet registration cleanup remains a separate task.


## Cleanup progress

The public Cilium Gateway path is removed from current CLI, smoke tests,
examples and Azure web ingress resources. API/egress and Blob-backed guards in
the older VMSS workflow remain until their replacement is defined. The overall
[TODO.md](../TODO.md) owns the ordering of those next cleanup passes.
