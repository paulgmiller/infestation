# Removing compute safely

The workstation orchestration is implemented, but the guest `retire` command
described below is still missing. Normal removal therefore fails closed and
retains compute. The protocol below records the intended guest behavior, not
live-verified functionality.

Normal `vm remove` and `azure remove` ask the guests to decommission their
host CockroachDB members before deleting compute. The workstation needs saved
provider ownership state and authorized Tailscale SSH access as `core` with
noninteractive sudo. It needs no database certificates or CockroachDB executable.

```sh
infestation vm remove --name old-pool --decommission-timeout 30m
infestation azure remove --name old-cloud --decommission-timeout 30m
```

Each database guest must run the current helper implementing retirement contract
version 1. An older helper fails closed with an explicit upgrade hint; removal
never installs software on guests or falls back to workstation administrator
credentials. Upgrade helpers through a separately authorized deployment workflow.

The workstation reads each owned guest's retirement identity using:

```text
tailscale ssh core@OWNED-NODE sudo -n /opt/infestation/infestation-node retire --identify
```

The helper returns bounded JSON containing the local node name, advertised
address, cluster UUID, database CA fingerprint, database node ID and store IDs.
The workstation checks these against owned names/addresses, requires a consistent
cluster and CA, rejects duplicate identities, and compares previous checkpoints.
Guest output never selects an SSH destination.

One verified guest receives the complete batch in a bounded base64 JSON request.
The request contains only identity metadata and a fresh request UUID. The guest
uses its existing database node certificate locally; it does not export a key,
CA signing material, administrator credential or enrollment bundle. There is no
arbitrary SQL or command tunnel in the retirement protocol.

The guest verifies live identities for every target, checks the entire batch's
capacity and health, and requires at least three healthy survivors. It runs
CockroachDB's strict allocator/placement prechecks and waits for all target
replicas to relocate and membership to become decommissioned. It verifies zero
replicas and healthy remaining members before returning a receipt. Replication
requirements are never lowered to make a shrink succeed. Kubernetes Ready and a
dead-store timeout are insufficient evidence; the database runs outside Kubernetes.

The workstation accepts a receipt only when its protocol version, request UUID,
cluster, CA, complete target identities and coordinator match the verified
request, and it proves completion, zero replicas and healthy survivors. Only
then does provider-specific compute deletion begin. The request UUID prevents an
old response from being accepted for a new attempt. An error, timeout, missing
helper, malformed response or mismatched receipt retains compute and disks.

The version-specific database behavior is documented in
[`cockroach node`](https://docs.cockroachlabs.com/docs/v26.2/cockroach-node)
and its [v26.2.7 implementation](https://github.com/cockroachdb/cockroach/blob/v26.2.7/pkg/cli/node.go).
The built-in strict allocator checks remain authoritative for placement readiness;
node-status metrics alone cannot prove actual replica placement.

Local `decommission.json` privately records the verified identities, request UUID
and last completed stage. Retrying online targets verifies identities again.
Cancellation may leave members decommissioning; inspect their status before
retrying. Infestation does not automatically recommission members. Normal retries
still require reachable target helpers, including after compute deletion partly
succeeds; offline-target retry reconciliation remains outstanding. An old successful
checkpoint never substitutes for fresh guest health evidence.

QEMU ownership, process and monitor checks still apply. `vm remove` retains disk
overlays unless `--delete-disks` is supplied. Azure validates saved provider and
resource-group ownership, compares current VMSS instance IDs against the complete
saved batch, then waits for SDK deletion of the resource group, including disks
and networking. Both providers retain local private state and credentials.

## Abandoned clusters

Intentional teardown after deciding to abandon the database uses:

```sh
infestation vm remove --name abandoned-pool --abandon-cluster --delete-disks
infestation azure remove --name abandoned-cloud --abandon-cluster
```

`--abandon-cluster` skips guest retirement and database health checks. It can
permanently lose data and quorum for surviving members. Provider ownership checks
still apply; private state remains. Do not use it to work around insufficient
replacement capacity in a cluster that should survive. It is also the explicit
cleanup path for failed initial provisioning that never formed a database.

## Scope and verification

Kubernetes workload draining, API Service withdrawal, Kubernetes Node deletion
and stale tailnet-registration cleanup remain outstanding. Database evacuation
does not establish application availability. Temporary stop must not permanently
decommission members; the public provider contract refuses stop until its
whole-batch quorum check and temporary drain are implemented.

Local orchestration tests use a fake Tailscale runner to cover exact commands,
whole-batch delegation, changed cluster/CA/node/store identity, old helper failure,
response bounds, malformed JSON, stale/mismatched receipts, checkpoint privacy
and retention of compute on failed verification. Guest-side retirement and its
tests remain unimplemented. Mocks do not constitute a live decommission test.
