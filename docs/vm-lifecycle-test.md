# VM lifecycle end-to-end test

`infestation-e2e run` exercises the public CLI with exactly three independently
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
three 3072 MiB VMs, outbound connectivity, Tailscale and kubectl. SQL probes use
the E2E binary's Go PostgreSQL client with the workstation's existing admin leaf
certificates; no CockroachDB CLI or `e2e-tools` directory is required. The optional
remote host takes the third VM's memory requirement.
Supply a reusable preauthorized `tag:roach` enrollment key through a private file
or `TAILSCALE_AUTH_KEY`; do not put key contents in command arguments or commit them.

The E2E is a separate CLI, so ordinary `go test ./...` never runs this lifecycle
scenario. Running `run` explicitly provisions and initializes a fresh cluster;
invoking the CLI without a subcommand prints help.

From the repository root:

```sh
go run ./cmd/infestation-e2e run \
  --tailnet-domain quoll-duck.ts.net \
  --tailscale-auth-key-file secrets/auth \
  --ssh millerbase
```

Or build it first:

```sh
go build -o bin/infestation-e2e ./cmd/infestation-e2e
bin/infestation-e2e run --help
```

Omit `--ssh` to run all three VMs locally. The CLI prints progress and returns
zero only when the lifecycle assertions and final shutdown succeed. Interruption
or a failure still attempts shutdown using an independent cleanup deadline.

Options:

- `--tailnet-domain`: dedicated test tailnet; required.
- `--tailscale-auth-key-file`: private enrollment key file; otherwise use
  `TAILSCALE_AUTH_KEY`. Key contents are never command-line arguments.
- `--name`: unique group prefix; generated when omitted.
- `--state-dir`: new private test directory that must not exist; otherwise a
  fresh directory is created in `/tmp`.
- `--timeout`: lifecycle deadline, default 75 minutes; final shutdown has an
  additional eight-minute deadline.
- `--accel`: `kvm` (default) or `tcg` (slow).
- `--base-image`: trusted local standalone Flatcar qcow2 base; otherwise download
  the pinned image.
- `--helper-source-ref`: published branch/tag/commit, default `master`.
- `--ssh`: SSH alias or `user@host` for the third VM.
- `--ssh-state-dir`: absolute private remote state root.
- `--ssh-base-image`: trusted base-image path on that remote host; otherwise the
  remote host downloads the pinned image.

The test logs its private workstation state directory and shuts down every
attempted test group on success or failure, with a bounded cleanup deadline. It
retains all disks, credentials and state; it never deletes test resources. Cleanup
failures fail the test and leave state for diagnosis. Inspect any shutdown failure
before assuming all test VMs are stopped. This lifecycle only provisions through
the explicit CLI `run` subcommand.

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
and the [Go SQL client API](https://pkg.go.dev/github.com/jackc/pgx/v5/pgconn).

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

The recorded live run used the environment-gated Go test and external SQL CLI.
The subsequent standalone CLI extraction and Go-client
replacement has localhost PostgreSQL/TLS mock coverage, including client
certificate authentication, verified server TLS, multi-statement results and
sanitation of SQL failures; the extracted CLI has not yet been rerun against VMs.
