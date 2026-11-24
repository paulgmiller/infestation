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
The retained directories are listed in the
[memory-reset runbook](cockroach-memory-rollout.md); staged extension images,
Docker/containerd caches, logs and the verified helper build cache are retained
as well. Database, Tailscale, K3s, machine-ID and SSH identities are preserved.

Progress and recovery files live privately inside the selected VM directory at
`upgrade-v1.36.5-k3s1/`. Rerun the same command after correcting a failure. The VM
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
They do not boot Flatcar. Live canary evidence will be recorded after verification.
