# One-instance QEMU upgrade

`infestation vm upgrade` applies this CLI's pinned release to one existing,
owned QEMU database server. It requires an explicit instance index:

```sh
infestation vm upgrade --name beeback --instance 0
```

Use the same name and state directory as create. SSH-backed groups automatically
run on their recorded compute host, which needs the same CLI binary. Run the
command separately for each instance. Finish the 1.36 rollout across all servers
before changing the compiled target to 1.37, as required by the
[K3s upgrade policy](https://docs.k3s.io/upgrades/manual).

The current target is K3s `v1.37.1+k3s1`, Flatcar `4757.2.4` and Tailscale
`v1.104.2`. CockroachDB stays at `v26.2.7`: database version transitions are not
implemented. The helper's configured `master` source reference is resolved once
to an immutable commit and recorded for the operation. Reruns use that same
commit. This is not a release planner, automatic cluster rollout or rollback.

The command stages extension images and builds the helper, then drains Kubernetes
and CockroachDB, stages a Flatcar retained-state reset, and shuts down the selected
VM. It checks the reset profile, retention marker, machine-ID setting and first
boot flag before shutdown. It takes and checks a standalone offline qcow2 copy,
replaces the selected node's saved profile, starts it and verifies its retained
identities, software versions and Kubernetes readiness before uncordoning it.
No administrator certificate or CA signing key is copied to the guest.

A successful CockroachDB drain is required. The command does not audit surviving
voting quorum; the operator must select a node whose temporary absence the cluster
can tolerate. It never decommissions the database node or lowers replication.
Kubernetes drain respects disruption budgets and refuses unmanaged pods. It uses
`--delete-emptydir-data` to permit eviction of pods with temporary `emptyDir`
storage, including metrics-server. Unknown persistent hostPath/local-volume paths
block reset.
Kubernetes drain output is reported on success; failures include kubectl's exit
status and error output, including pod eviction blockers and timeouts. SSH
authentication diagnostics remain private.
The retained directories are listed in the
[memory-reset runbook](cockroach-memory-rollout.md); staged extension images,
Docker/containerd caches, logs and the verified helper build cache are retained
as well. Database, Tailscale, K3s, machine-ID and SSH identities are preserved.

During a partial group rollout, each saved per-node profile records its own pins.
The group inventory pins advance after every instance profile has converged.

Progress and recovery files live privately inside the selected VM directory at
`upgrade-v1.37.1-k3s1/`. Rerun the same command after correcting a failure. The VM
may already have the target Flatcar update staged by update-engine; the command
reuses a matching pending payload instead of calling `flatcar-update` again.
A different pending version blocks the upgrade without discarding that payload.
An update in progress must finish before retrying.
The VM
lifecycle lock prevents concurrent start/stop/autostart while the command runs.
A failure can leave the node cordoned, CockroachDB drained, reset pending or the
VM stopped. The command resumes from its saved stage rather than operating on
other instances. Do not manually start a VM whose upgrade is paused in the
stopped/copy stage.

The offline recovery copy contains the pending reset markers and staged retained
join bundle; it is not an automatic rollback image. Never restore an old database
store into a running cluster without a separate reviewed recovery procedure.
Clear the pending reset in an offline recovery workflow if necessary. The tool
never restores snapshots automatically. Founding nodes, workers, Azure instances,
arbitrary disk layouts and database version transitions are outside this first
implementation.

## Validation

Local mock tests cover instance selection, lifecycle exclusion, live-disk copy
refusal, resuming after start succeeded but checkpoint persistence was interrupted,
state/SSH-key retention, staged extension delivery and supported K3s transitions.
They do not boot Flatcar. Live evidence is recorded below.

### Live canary, 2026-11-24

`bin/infestation vm upgrade --name beeback --instance 0` completed a retained-state
reset on beeback-0. K3s changed from `v1.35.8+k3s1` to `v1.36.5+k3s1`; Flatcar
remained `4757.2.4`, Tailscale remained `v1.104.2`, and CockroachDB remained
`v26.2.7`. The helper build was pinned to source commit
`d8e8343c25400b42f360bdd787c5a9526892869e`. The command confirmed the retained
machine, SSH host-key, database CA, Kubernetes node-password, Tailscale device and
database cluster/node identities. It verified the target versions and Ready
condition before uncordoning the node. Its standalone offline qcow2 copy passed
`qemu-img check`.

A repeated completed operation verified successfully without changing QEMU's
PID or rebooting the VM. The final CLI also reconciled the completed group's
saved software pins. Kubernetes reported beeback-0 Ready on 1.36.5 and both live
basementquiet nodes Ready on 1.35.8. Old stopped nodes remained NotReady entries;
no node removal was performed. The CLI was installed on millerbase and its
upgrade help was checked; its VMs were not upgraded in this canary.

The initial read-only attempt stopped before drain/reset because the identity
query lacked `allow_unsafe_internals`; the command now sets that SQL session
option using the existing limited Kine identity. No administrator credentials
were deployed. `go test ./...` and `go vet ./...` passed with the final code.

The command deliberately uses host shutdown/start rather than just an in-guest
reboot to provide an offline disk-copy interval. Both trigger the staged Flatcar
reset at the next boot.

### K3s 1.36 checkpoint, 2026-11-24

All three live servers reported Ready on `v1.36.5+k3s1` before preparing the
1.37 target. CockroachDB remained `v26.2.7`. Post-boot K3s journals showed a
fatal serialization commit failure (`SQLSTATE 40001`) on beeback-0 at
23:41:41 UTC. It had about 33.5 minutes of observation and two service restarts;
only one fatal commit was found. Basementquiet-0 and -1 had no fatal commits or
service restarts in approximately 14.9 and 9.5 minutes respectively, too short
to establish a compaction fix. Two successful compaction batches on beeback-0
deleted 956 and 963 rows in about 614 and 602 ms. Recent slow SQL logs still
contained SELECT/count operations taking roughly 1–3 seconds.

Ten persistent-connection samples per SQL gateway produced SELECT 1 medians
0.57–0.63 ms without errors. Kine maximum/compact revision reads had medians
about 8.3–8.7 ms through beeback-0/basementquiet-0, and 0.73–0.88 ms through
basementquiet-1. Earlier medians were about 0.8–2.0 ms; these samples do not
control workload, lease placement or network conditions and cannot establish
an upgrade-caused regression. A subsequent revision snapshot was 2,073,791
latest versus 767,031 compacted, a gap of 1,306,760 revisions, not a row count.
The compaction crash and slow operations remained present.

K3s 1.36.5 bundles Kine 0.16.5; the published 1.37.1 Flatcar extension bundles
Kine 0.17.1. That Kine version still has fatal commit handling. SQLSTATE 40001
requests a transaction retry; serializable transactions require retrying the
complete transaction when the server cannot transparently retry it. Raising
the Kubernetes version alone is not evidence that this error handling is fixed.
Sources: [K3s 1.36 component versions](https://docs.k3s.io/release-notes/v1.36.X),
[K3s 1.37 component versions](https://docs.k3s.io/release-notes/v1.37.X),
[Kine 0.17.1 commit handling](https://github.com/k3s-io/kine/blob/v0.17.1/pkg/drivers/generic/tx.go),
and [CockroachDB retry errors](https://docs.cockroachlabs.com/docs/v26.2/transaction-retry-error-reference).

[Kine issue #87](https://github.com/k3s-io/kine/issues/87) describes an older
unsupported compaction SQL syntax problem. Its October 2026 maintainer response
confirms CockroachDB is tested and documents `serial_normalization=sql_sequence`;
bootstrap already sets that default for the Kine role. Successful compaction
batches in this cluster establish that the old syntax failure is not the observed
commit failure. The remaining SQLSTATE 40001 failure needs complete-transaction
retry handling rather than the historical SQL syntax workaround.

### K3s 1.37 canary, 2026-11-24

The same one-instance command upgraded beeback-0 from `v1.36.5+k3s1` to
`v1.37.1+k3s1`, using helper commit
`b3643a48bf147b8b360d1c415739353cd6991319`. Its first attempt stopped before
database drain or reset because metrics-server used an `emptyDir` mounted at
`/tmp`. At the operator's request, Kubernetes drain now passes
`--delete-emptydir-data`. The retry reported the metrics-server and goldpinger
evictions, completed the database drain and offline-copy/reset workflow, and
verified retained identities, target versions and Ready status before uncordoning.
Immediate follow-up showed K3s active/running with zero restarts since boot.
This brief startup check does not establish compaction reliability; the previous
failure recurred over a longer observation window. CockroachDB stayed at 26.2.7.
