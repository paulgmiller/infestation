# Infestation TODO

This is the overall prioritized work list. Detailed plans and historical test
records are linked below; this file is the starting point for current work.

## Current checkpoint

- [x] Verify a cluster with two independent local NAT VMs and one Azure VM over
  Tailscale, with host CockroachDB, K3s native Tailscale/Flannel, kube-proxy and
  CoreDNS. [Evidence and repairs](docs/mixed-cluster.md).
- [x] Build the helper natively on Flatcar using a pinned multiarchitecture Go
  container and immutable GitHub commit; no custom OS image or helper release
  binary is required. Native arm64 execution remains untested.
- [x] Retrieve a locally prepared kubeconfig over Tailscale SSH (`core` + sudo)
  and verify Kubernetes TLS through the shared API Service.
- [x] Capture all six directed latency paths and 60 ping replies, then stop local
  compute and delete the owned Azure test resources. Local state is preserved.

The successful mixed test required repairs. It does not establish unattended
fresh provisioning, safe replacement, backups/restore or production resilience.

## 1. Cleanup and one supported deployment path

- [x] Remove obsolete public Cilium Gateway examples, smoke tests, CLI flags,
  Azure web firewall rules, forwarding rules and probes. Existing enabled stacks
  require deliberate migration; no infrastructure was applied.
- [ ] Retire the remaining legacy Cilium CNI/kube-proxy-replacement profile after
  migrating or explicitly retiring the older Azure VMSS deployment workflow.
  Reject unsupported configurations rather than generating a cluster with no CNI.
- [ ] Remove Blob-backed helper and kubeconfig delivery from the older Azure
  workflow. Use the tested guest build and local kubeconfig + tailnet SSH path.
- [ ] Separate the durable initialization guard from artifact/kubeconfig delivery;
  choose replacement/recovery behavior before deleting its storage backend.
  [Bootstrap gaps](docs/bootstrap.md).
- [ ] Migrate or retire Azure-specific private DNS discovery, managed identity,
  artifact RBAC/locks and unused public API ingress. Preserve existing Pulumi
  project IDs, stacks, CA, token and disks; source cleanup must not apply a
  destructive infrastructure update or recreate stopped resources.
- [ ] Consolidate overlapping local, mixed, Pulumi and Azure-check entry points.
  Decide the fate of legacy VMSS support explicitly instead of maintaining two
  implicit architectures. Keep old state usable for deliberate recovery/teardown.
- [ ] Reconcile README, contributor instructions and older architecture/operations
  documents with the supported Tailscale path; label historical evidence clearly.

## 2. Reliable fresh bootstrap and networking

- [ ] Run a fresh mixed test with all recorded fixes present from first boot and
  require no SSH repair/reboot. Capture failures before taking recovery actions.
- [ ] Replace the prototype Azure default outbound access with explicit egress;
  compare dependencies, cost, failure scope and direct/relay behavior. Do not
  silently add an ingress load balancer. [Detailed networking TODOs](docs/networking-todo.md).
- [ ] Validate DNS inside host containers, delayed peer enrollment, MTU, packet
  loss and routes from ordinary tailnet clients, not only other cluster nodes.
- [ ] Add readiness-aware API Service withdrawal/failover tests and DNS redundancy.
- [ ] Choose and test pod NetworkPolicy enforcement; it is currently disabled.
- [ ] Make Service names and pod/service CIDRs explicit for concurrent clusters;
  retire stale tailnet hosts, subnet advertisements and Service registrations.
- [ ] Gate smoke clients on ready web endpoints and retain structured evidence
  for failed as well as successful attempts.

## 3. Data safety, replacement and maintenance

- [ ] Design durable initialization intent and cluster identity across initializer
  replacement/disk loss; refuse ambiguous reinitialization. Test quorum loss,
  partial joins, missing node 0 and replacement without new PKI/token.
- [ ] Define and test backup/restore, including the Kine database and matching
  K3s recovery inputs. Reconcile the older Blob-backed proposal with the current
  goal of no mandatory cloud storage service. [Operations plan](docs/operations-plan.md).
- [ ] Coordinate Flatcar, K3s and Cockroach updates/drains one node at a time;
  test rollback and rejoin. A pinned image does not pin a later booted OS update.
- [ ] Investigate Kine/Cockroach maintenance-status incompatibilities and watch
  timeout logs under sustained WAN latency, even when basic API checks pass.
- [ ] Test application storage, placement, zone/site failures and resource sizing.
  Running replicas and local-path storage are not a demonstrated recovery plan.

## 4. Portability and distribution

- [ ] Test native arm64 first boot, enrollment and workloads.
- [ ] Review cold guest build cost, availability, caching, provenance, update/
  rollback and diagnostics before enrollment. Improve distribution only after
  measuring; Packer or published helper binaries are not required decisions.
- [ ] Make create/resume/teardown ownership and partial-failure handling consistent.
- [ ] Add other clouds after the common bootstrap and egress contract is clear.

## Detailed references

- [Networking and bootstrap review](docs/networking-todo.md)
- [Networking architecture and checkpoints](docs/networking-plan.md)
- [Mixed-test runbook and actual evidence](docs/mixed-cluster.md)
- [Operations/recovery design](docs/operations-plan.md) — older assumptions need review
- [Research questions](docs/research-questions.md)
- [Bootstrap design](docs/bootstrap.md)
