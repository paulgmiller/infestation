# VM lifecycle end-to-end test

`TestLiveVMLifecycle` exercises the public CLI with exactly three independently
created QEMU servers. Only the first receives `--init`; all three host CockroachDB.
It waits for three voting replicas and no learners on every range, including
system and Kine ranges. It stops only the third VM, checks that the two survivors continue serving SQL
and Kubernetes/Kine operations for a minute, then starts the same VM and verifies
recovery. It checks persisted SQL and ConfigMap data, Cockroach node/store IDs and
the restarted node's stable Tailscale ID/DNS name. Three nodes permit a surviving
majority for three-voter ranges; the actual read/write probes are required evidence
that the cluster continues serving.

This test is opt-in. The lifecycle assertions passed on 2026-11-16 UTC using
two VMs on millerbee and one SSH-backed VM on millerbase. Final shutdown exposed
a temporary-storage quota failure; see the run record below. Unit tests use
mocked SSH and temporary state and remain separate evidence.

## Prerequisites

Use a dedicated test tailnet with no existing Infestation cluster: there is one
cluster per tailnet, and the test creates a new database. Existing VM resources
are never selected by name for testing. Complete the
[tailnet policy setup](tailnet-policy.md), including enrollment, pod-route and
API Service approvals and Tailscale SSH access as `core`.

The test workstation needs Linux x86-64, QEMU/KVM access, sufficient memory for
three 3072 MiB VMs, outbound connectivity, Tailscale, kubectl, and the CockroachDB
v26.2.7 CLI. The optional remote host takes the third VM's memory requirement.
Supply a reusable preauthorized `tag:roach` enrollment key through the environment;
do not put credentials in test command arguments or commit them.

Run only when explicitly intending to provision and initialize a fresh cluster:

```sh
export INFESTATION_LIFECYCLE_TEST=1
export INFESTATION_TEST_DOMAIN=YOUR-TEST-TAILNET.ts.net
# Set TAILSCALE_AUTH_KEY privately before running.
go test ./internal/cli -run '^TestLiveVMLifecycle$' -count=1 -timeout=80m -v
```

Optional inputs:

- `INFESTATION_VM_ACCEL`: `kvm` (default) or `tcg` (slow).
- `INFESTATION_VM_BASE_IMAGE`: trusted local standalone Flatcar qcow2 base.
- `INFESTATION_HELPER_SOURCE_REF`: published helper source branch/tag/commit;
  default is `master`. Local source changes must be available at the selected
  published ref before testing guest behavior.
- `INFESTATION_TEST_SSH`: SSH alias or `user@host` for the third VM.
- `INFESTATION_TEST_SSH_STATE_DIR`: absolute private remote state root.
- `INFESTATION_TEST_SSH_BASE_IMAGE`: trusted base image path on that remote host;
  otherwise the remote host downloads the pinned image.

The test logs its private workstation state directory and shuts down every
attempted test group on success or failure, with a bounded cleanup deadline. It
retains all disks, credentials and state; it never deletes test resources. Cleanup
failures fail the test and leave state for diagnosis. Inspect any shutdown failure
before assuming all test VMs are stopped. No tests provision unless their opt-in
variable is 1.

## Remote QEMU creation

Install the same Infestation revision on both machines. The remote account must
find `infestation` on its noninteractive SSH PATH or at `~/go/bin/infestation`,
and have QEMU/KVM access. Use an
SSH config alias for custom ports, identity files or proxy configuration. SSH uses
batch mode and normal host-key verification; establish authentication beforehand.

```sh
infestation vm create --name friend --ssh alice@compute-host \
  --ssh-state-dir /home/alice/inf-vms \
  --tailnet-domain YOUR-TAILNET.ts.net \
  --tailscale-auth-key-file secrets/tsauth
infestation vm status --name friend
infestation vm stop --name friend --allow-outage
infestation vm start --name friend --wait 15m
```

`--state-dir` remains the workstation ownership/credential directory. The remote
state root defaults to the SSH user's Infestation data directory. `--base-image`
is interpreted on the compute host. The remote host's hashed machine identity
supplies the default locality host label. Remote creation can also use `--init`;
CA signing and long-lived administrator credentials remain on the workstation.
Only rendered node profiles and inventory are sent through SSH stdin. The
separate one-hour SQL bootstrap credential is included only for the selected
initializer, following the ordinary bootstrap exception.

Subsequent start, stop, status, netcheck and remove operations automatically use
the SSH host and a saved ownership token. Keep both workstation ownership state
and remote VM state. A failed create retains routing/state and cannot be retried
as a replacement create over the same directory. Wait and kubeconfig retrieval
use workstation tailnet access. Removal retains its existing database safety
checks and disk-retention defaults.

`vm stop --allow-outage` explicitly skips the unfinished quorum/drain guard. It
requests graceful guest shutdown, verifies the QEMU process/monitor identity,
and waits up to two minutes for exit without forcing power off. This is intended
for controlled lifecycle/outage testing; it does not implement database draining,
Kubernetes workload draining or API Service withdrawal. Ordinary stop continues
to refuse until those safeguards are implemented. Stopping a multi-node group
with this flag can take the entire database offline.

The SQL identity probe opts its session into CockroachDB's internal tables;
these are version-specific diagnostic interfaces. See
[CockroachDB v26.2 internal tables](https://www.cockroachlabs.com/docs/v26.2/crdb-internal), [v26.2.7 internal-table test schemas](https://github.com/cockroachdb/cockroach/blob/v26.2.7/pkg/sql/logictest/testdata/logic_test/crdb_internal),
and [SQL client flags](https://www.cockroachlabs.com/docs/v26.2/cockroach-sql).

## Shut down all local groups

```sh
infestation vm stop --all --allow-outage
# For groups created under a custom state root:
infestation vm stop --all --allow-outage --state-dir state/vms
```

This discovers all named local groups and standalone VMs immediately under the
selected state root. It skips SSH-backed groups, Azure groups and directory
symlinks. It does not search other users' state or arbitrary custom directories;
repeat with each state root when needed. It validates saved machine identities
before requesting shutdown, attempts every selected VM even if one fails, and
reports failures. Shutdown retains disks and private state and never forces off
an unresponsive guest. No shutdown command is run by ordinary unit tests.

## Live run: 2026-11-16 UTC

The run used KVM, cached Flatcar 4757.2.2 images, CockroachDB v26.2.7 and
`quoll-duck.ts.net`. Old Infestation VMs on millerbee and millerbase were stopped
before provisioning. No old VM disks or credentials were deleted.

`TestLiveVMLifecycle` passed its assertions in 262.92 seconds. The three servers
were `life-1794813204s-0` and `life-1794813204p-0` on millerbee, and
`life-1794813204j-0` on millerbase through SSH. The test verified three voting
replicas per range, surviving SQL and Kine writes during the remote node's
outage, its subsequent restart, persisted data and unchanged Cockroach node/store
and Tailscale identities. K3s writes used a ConfigMap: create before the outage,
patch through a surviving API server during it, and read the patched value
through the restarted server's API afterward.

This invocation was compiled before automatic shutdown cleanup was added, so
final shutdown ran separately. Millerbase shut down cleanly. On millerbee, growing
local disks exhausted the user's `/tmp` quota after the lifecycle assertions
passed. Both local guests logged disk-write errors and became unreachable over
SSH. Graceful shutdown timed out; verified QMP identity/process checks preceded
forced power-off of only those two test VMs. Their filesystems may need recovery.
Zero QEMU processes remained on either host after cleanup. This is a passing
lifecycle probe with a failed graceful final cleanup, not evidence of a clean
end-to-end shutdown on millerbee under storage exhaustion.

The retained workstation state was moved to ignored
`state/e2e-3014792880/`, freeing roughly 8 GB in `/tmp`. Each copied disk was
verified with `qemu-img compare` before its temporary source was removed. The
private test output is `state/e2e-3014792880/run.log`. Millerbase retains its
joining VM under `~/.local/share/infestation/vms/life-1794813204j/`. CA/admin
credentials remain on millerbee. The test continues to default to `/tmp`, per
the operator's preference; clear sufficient scratch space before provisioning.
The updated test attempts bounded graceful shutdown on both pass and failure,
reports cleanup errors and retains disks/state. It does not force power off or
delete test disks automatically.
