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

## Implementation boundary

This change adds Azure RA-GZRS artifact storage, private identity-authenticated
access, delete protection, deployment discovery outputs, publication through the
existing bootstrap, and `infestation azure kubeconfig` fetching. Unit and Pulumi
mock tests do not establish actual regional availability or failure recovery.

The Azure planner defaults to private nodes and a public API. Its output directory
defaults to `state/<name>` and SSH keys/flags have been removed. Existing saved
plans and PKI are not rewritten. Default node names are `kube-0` through `kube-2`.

The node-local SQL credential path is wired: deployment mints a one-hour SQL
root client certificate/key for `-0`'s private Ignition only, and the node removes
it after recording successful Kine SQL setup. A pinned-image Docker test confirms
initialization, SQL setup, cleanup and subsequent Kine authentication. The CA and
long-lived admin keys remain on the workstation.

Complete Azure node-local first boot, license activation, Cockroach stand-ins/host
limits, backups/retention/restore, AZ failure tests, application placement defaults
and certificate-renewal publication remain work to implement or verify. The
Flexible VMSS currently groups static VM resources; automatic scale-out requires
worker certificate enrollment and a shared scaling profile. Do not treat
unit/mock or isolated Docker results as a successful Azure deployment. No cloud
resources are provisioned as part of this source change.
