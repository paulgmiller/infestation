# Agreed operations design

Decisions from the 2026-10-26 architecture interview. This is a target design;
the implementation status below distinguishes it from deployed behavior.

- Small persistent application clusters; QEMU is the local test environment.
  Complete Azure first, retaining the shared Go design for AWS/Google Cloud VMs.
- Kubernetes and CockroachDB must survive losing one availability zone.
  Applications need zone-spread placement and sufficient remaining capacity.
- After losing two zones or a region, an operator triggers recovery into a
  different region without pre-created compute/networking. Target: all logical
  Cockroach databases restored and cluster ready within one hour, up to fifteen
  minutes of data loss. Data size and demonstrated recovery throughput are TBD.
  The `kubernetes` Kine database is included, along with matching K3s recovery
  inputs. No other database engines are in scope.
- One coordinated recovery restores the databases together and holds
  applications until their databases are ready. Parallelism is an implementation
  detail; database consistency must be preserved.
- Backups go to region-redundant Blob Storage in the cluster resource group,
  with one day of recovery-point retention. Protect the account against deletion.
  Preserve complete backup-chain dependencies required by retained restore points.
  Backup cadence and cross-region replication lag together determine achievable
  data loss; a fifteen-minute schedule alone is insufficient evidence.
- Recovery configuration/secrets also live in protected storage so recovery does
  not depend on the original workstation. Operators obtain cloud access through
  the provider portal. The kubeconfig has a CLI fetch command.
- Region-qualified API and application endpoints. No automatic DNS changes or
  traffic cutover: operators handle Traffic Manager/CNAMEs/routing. The one-hour
  target ends at a healthy restored cluster and available endpoints/kubeconfig.
- Alerting is deferred pending the user's alerting infrastructure.
- Host Cockroach remains outside Kubernetes. Proposed host-network DaemonSet
  stand-ins provide readiness, Service endpoints and fixed configurable scheduler
  resource requests. Actual host controls separately constrain the DB process;
  do not double-count its budget. Keep Kine's direct/local DB bootstrap path.
- Initial Cockroach budget: 500m CPU request and 2 GiB memory reservation/host
  limit, pending measurement. Allow CPU bursting with host CPU weights. Proposed
  OS/K3s reservation is 500m CPU and 1.5 GiB memory. Application requests remain
  optional, so their undeclared demand is not reflected in scheduling decisions.
- No SSH or public node addresses. SSH services/sockets are masked for now;
  later operator access through Tailscale remains to be designed.
  The Kubernetes API remains public with TLS/authentication for now.
- Deployment should self-bootstrap through Ignition-installed node services;
  no external SSH runner. K3s generates its own PKI; publish the resulting
  regional admin kubeconfig to protected storage and refresh after renewal.
- For now the `-0` server VM (`kube-0` in the default inventory) is the only
  initializer, regardless of inventory order. No automatic failover to another
  initializer. A future Blob lease is a TODO; see [bootstrap design](bootstrap.md)
  for the failure behavior and portability requirements.
- Accept a Cockroach license secret and activate it during bootstrap. Licensing
  eligibility, provisioning the license and ongoing renewal remain operator duties.

## Implementation boundary (updated 2026-10-27)

The Azure Uniform VMSS test completed fresh CLI-driven first boot without SSH,
using three API servers and initially one worker. Scaling the worker pool to two
produced five Ready Kubernetes nodes and five live Cockroach members across
three zones. All 20 directed DNS/service checks passed. Kubeconfig publication
and fetch, IMDS locality, temporary SQL-key cleanup, SSH masking and worker-role
reconciliation were verified. See the [Azure runbook](../infra/azure/RUNBOOK.md)
for evidence, limits and teardown history.

New Azure plans use two shared role profiles, dynamic private addresses and
`api-N` / `worker-N` identities derived from IMDS. Shared wildcard node leaf
credentials support scale-out without a certificate enrollment service; CA and
long-lived administrator keys stay local. A separate one-hour SQL root credential
is in the shared API profile, used only by API instance 0; other API members
remove their copies. The default output directory is `state/<name>`.

Azure cloud-controller-manager is not installed. K3s ServiceLB is disabled;
application LoadBalancer Services do not configure the Pulumi API/egress SLB.

New generated profiles disable kube-proxy and configure Cilium replacement
against the public API LB on 6443. This supersedes the kube-proxy-enabled baseline
for future profiles; it has not been deployment-tested. Ports 80/443 remain
available for future gateway listeners. Existing deployments are not migrated.

Safe decommission/scale-in, coordinated upgrades, license activation, Cockroach
stand-ins/host limits, backups/retention/restore, AZ failure tests and certificate-
renewal publication remain unimplemented or unverified. Upgrade discovery changes
are local preparation, not a tested rollout. Both Azure test resource groups were deleted and their absence verified.
The source, private local state and test evidence remain for planning.

## Open research

The user requested investigation of ephemeral container storage, external cloud
providers, Tailscale Funnel, simpler CNI/gateway options, and coordinated automatic
maintenance. These are questions for later design, not approvals to change the
current baseline. The detailed backlog and additional lessons are in
[research-questions.md](research-questions.md).
