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

The current target is K3s `v1.36.5+k3s1`, Flatcar `4757.2.4` and Tailscale
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
Kubernetes drain respects disruption budgets and refuses unmanaged pods and
emptyDir data loss. Unknown persistent hostPath/local-volume paths block reset.
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
`upgrade-v1.36.5-k3s1/`. Rerun the same command after correcting a failure. The VM
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
