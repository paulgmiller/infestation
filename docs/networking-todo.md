# Networking TODO

Outstanding networking work. The overall index is [TODO.md](../TODO.md), and
[the networking design](networking-plan.md) describes the current approach.

## Bootstrap and egress

- [ ] Replace Azure default outbound access with explicit egress. Compare NAT
  Gateway, outbound load-balancer rules and per-VM public IP with inbound denied
  against the minimal-infrastructure goals. Test boot with default outbound
  disabled: DNS, source/registry/module downloads, time synchronization,
  Tailscale control/DERP and direct peer paths. Record cost and failure scope.
  [Azure guidance](https://learn.microsoft.com/en-us/azure/virtual-network/ip-services/default-outbound-access).
- [ ] Run a fresh mixed deployment requiring no SSH repair or reboot.
- [ ] Test Quad100 DNS in host containers with systemd-resolved split DNS,
  delayed peer enrollment and unavailable upstream resolvers.
- [ ] Verify pod routes from ordinary tailnet clients, including Linux route
  acceptance and policy. Keep distinct CIDRs for concurrent clusters.
- [ ] Test native arm64 first boot and cached helper build behavior.

## Availability and policy

- [ ] Monitor local API readiness and withdraw/drain unhealthy Service hosts.
  Test API failure with a live Tailscale daemon, host loss and reconnection.
- [ ] Spread redundant CoreDNS pods and test rescheduling during a node outage.
- [ ] Choose and test NetworkPolicy enforcement for same-node and cross-node
  traffic; distinguish workload rules from host/tag/subnet access rules.
- [ ] Measure MTU, loss, throughput and long-lived connections across direct and
  relay paths. Short DISCO samples are not workload performance guarantees.
- [ ] Verify kubeconfig publication at initial readiness and credential renewal.
- [ ] Define public application ingress redundancy using outbound tunnels.

## Simplification and safe operations

- [ ] Consolidate enrollment, bootstrap, readiness and kubeconfig retrieval across
  compute providers under the common provisioning interface.
- [ ] Review cold guest builds, bounded disk use, failed-container cleanup,
  retry/backoff, pre-enrollment diagnostics, provenance and update/rollback.
- [ ] Gate smoke clients on ready web endpoints.
- [ ] Clean up stale devices, subnet advertisements and Service registrations.
- [ ] Test durable initialization guards, replacement, backup/restore and
  coordinated updates as tracked in [the overall TODO](../TODO.md).
