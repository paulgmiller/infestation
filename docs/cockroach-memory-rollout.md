# Immutable node configuration upgrades

New profiles use `--max-go-memory=768MiB` and `--max-sql-memory=256MiB`.
The JSON settings `cockroach_go_memory` and `cockroach_sql_memory` override
these values. Go memory is a soft runtime limit, not a total-process cap.

Existing nodes require an immutable upgrade workflow; manually editing installed
systemd units is not the deployment model. A memory-only reset for joining QEMU
servers has been tested below; automated rolling and component-version upgrades
remain outstanding. Updating source or replacing an attached Ignition file does not
by itself update a previously booted node.

## Storage and provisioning boundary

QEMU currently attaches a persistent `disk.qcow2` backed by a Flatcar image and
passes `config.ign` through firmware configuration. The guest OS and default
CockroachDB store (`/var/lib/cockroach`) share that disk. Replacing the disk would
therefore also replace database data and other durable guest state. Changing its
backing image is not a safe OS upgrade procedure.

Ignition normally runs only on first boot. Flatcar documents `flatcar-reset` for
reprovisioning with explicit retained root-filesystem paths. Before using it,
identify and test preservation of database stores, Tailscale identity, K3s state,
credentials and bootstrap/recovery markers. Never replay initializer authority
or format a retained store. An alternative design is a fresh OS disk with
separate retained data storage and a tested identity/credential handoff.

Flatcar OS updates are separate from applying changed Ignition configuration.
CockroachDB and K3s version transitions also need their respective supported
upgrade paths, recovery plans and per-component health checks.

## Required rollout behavior

- Establish actual database membership and per-range quorum before disruption;
  Kubernetes Ready alone is insufficient.
- Prepare and validate the new profile and a recovery path before touching a node.
- Drain Kubernetes workloads, withdraw its API forwarder as appropriate, and
  gracefully drain the database before stopping or reprovisioning a whole node.
- Upgrade only one member at a time, retaining disks, ownership and identity.
- Require recovered database quorum/replication, K3s/API and tailnet health before
  proceeding; compare CPU, GC and memory headroom under representative workload.
- Test rollback limits separately for OS, configuration, database and K3s versions;
  restoring an old boot image is not a database downgrade procedure.

Remaining upgrade work and resource sizing are tracked in [TODO](../TODO.md).
The source defaults alone do not update existing nodes; the canary below applied
a replacement profile explicitly.

References: [Flatcar first-boot configuration](https://www.flatcar.org/docs/latest/getting-started/quickstart/),
[Flatcar reprovisioning](https://www.flatcar.org/docs/latest/fb-provision/infrastructure/),
[CockroachDB memory flags](https://docs.cockroachlabs.com/docs/v26.2/cockroach-start),
and [database draining](https://docs.cockroachlabs.com/docs/v26.2/node-shutdown).

## Preparing a memory-only reset

`infestation reprovision-profile` prepares a private, configuration-only profile
from a joining QEMU database server's original Ignition:

```sh
infestation reprovision-profile --source ORIGINAL.ign --out REPLACEMENT.ign \
  --max-go-memory 768MiB --max-sql-memory 256MiB
```

It keeps software/helper pins, omits fresh Tailscale auth keys and temporary SQL
root credentials, removes writes to retained state, and adds a required boot
check before database, K3s, enrollment and Tailscale startup. It currently refuses
founders, workers, VMSS and profiles that partition or format disks. This is a
profile preparation command, not an automated rolling upgrade or power operation.

Retain `/var/lib/cockroach`, `/var/lib/tailscale`, `/var/lib/rancher/k3s`,
`/var/lib/kubelet`, `/etc/rancher/node`, `/var/lib/infestation` and
`/etc/cockroach/certs` and the host-key pattern `/etc/ssh/ssh_host_.*`; use
`--keep-machine-id`. Tailscale SSH reuses readable system SSH host keys, so its
state directory alone is insufficient to preserve the SSH fingerprint. See
[the pinned host-key loader](https://github.com/tailscale/tailscale/blob/v1.104.2/ssh/tailssh/hostkeys.go).
Inventory any additional workload
local storage. Docker/containerd caches and `/var/log` can also be retained to
reduce cold-start work and preserve diagnostics. The verified helper cache
(`/opt/infestation/infestation-node`, `helper-build.identity`, `helper-build.sha256`)
can be retained when its source/build pins are unchanged; the build script checks
the desired identity and checksum before reuse. Never retain the whole `/etc`
in place of regenerating configuration.

Review a deletion preview and take a checked, offline standalone disk copy
before staging `flatcar-reset`. Flatcar 4757.2.2's confext adds a nested
`/etc/.systemd-confext` mount; the installed `flatcar-reset --preview-delete`
can report `umount: /etc: target is busy` yet exit successfully. An empty preview
is not evidence of safety. For inspection only, recursively unmount `/etc` in
an explicitly private mount namespace and apply the tool's exact retention regex
to `find / -xdev`. Do not unmount the live host's `/etc`.

A normal Flatcar OS A/B update preserves root state and is separate from this
selective root reset. `/oem/config.ign` supplied by `flatcar-reset --ignition-file`
is preferred over QEMU fw_cfg on this release, provided no kernel config override
is present; also update the private host profile for future cold starts. See the
[release-pinned initrd configuration](https://github.com/flatcar/bootengine/blob/b92a1ca9f698bb155ffb7e9cccf83e27e0727817/dracut/53ignition/module-setup.sh)
and [Ignition fetch order](https://github.com/coreos/ignition/blob/v2.24.0/internal/exec/engine.go).


## Live canary, 2026-11-24

The authorized target was `beebud-0`, with `basementbaddy-0/1` kept online.
All 95 observed range voter sets retained a majority on stores 3 and 4 without
store 5; 54 ranges still referenced an old unavailable voter. This verified only
the selected canary stop, not the safety of stopping either basement node.
The Kubernetes inventory contained no PVs or custom hostPath volumes on the
canary. Non-DaemonSet pods were drained and its API forwarder withdrawn.

Merged master stop code initially failed closed: the v26.2.7 drain RPC ignored
`--user=node` and requested a root-named client certificate. The corrected
workflow uses temporary client-filename aliases to the existing node leaf,
retains normal TLS verification, and requires the exact `drain ok` receipt.
Both subsequent stop attempts confirmed successful drain and retained the disk.
No administrator key was copied to a guest.

A standalone offline qcow2 rollback copy passed `qemu-img check`. The first
normal restart activated the already-staged Flatcar 4757.2.3 OS update from
4757.2.2. The configuration reset then used the reviewed keep paths, machine-ID
preservation and `/oem/config.ign`, with matching saved host profiles. Docker and
containerd caches, logs and the checksum-verified helper cache were also retained.
Private profiles, inventory, previews and the rollback disk are retained under
ignored `state/upgrades/beebud-0-20261124/`.

After reset, the retained-state guard passed; CockroachDB returned as the same
node 5/store 5. Tailscale device ID, IP, machine ID, cluster CA and K3s registration
password hash matched the pre-reset observations. K3s and the API forwarder
recovered and the node was uncordoned; all three live nodes reported Ready.
CockroachDB stayed on v26.2.7 and K3s on v1.35.8+k3s1. Effective flags were
`--max-go-memory=768MiB --max-sql-memory=256MiB`; the Go limit metric reported
805306368 bytes.

The initial retention list missed `/etc/ssh/ssh_host_*`. Those keys regenerated,
changing the SSH fingerprint despite preserving the Tailscale device identity.
The new ED25519 fingerprint was independently verified through Tailscale SSH.
The renderer's retention list and state-write exclusions now include host keys;
that correction has local tests and still needs a subsequent live reset test.
The original disk rollback copy retains the pre-reset state. Do not change
known-host trust solely on the basis of an SSH warning.

A post-recovery 30-second sample counted one GC and about 0.043 seconds of GC CPU
out of 4.63 seconds total process CPU (about 0.9%). An earlier licensed sample
under the old 576 MiB limit counted 154 collections in 15 seconds with about 44%
of process CPU in GC. These are observations under changing workload, not a
controlled benchmark. Post-reset RSS was about 1.0 GiB; one host sample had
982 MiB available. Broader workload/memory tuning remains outstanding.

## Manual basement rollout: instance 0, then instance 1

The actual node names are `basementbaddy-0` and `basementbaddy-1`. Their QEMU
compute host is `millerbase`, with group state at
`~/.local/share/infestation/vms/basementbaddy`.

**Current blocker, checked November 24:** stopping either basement node would
lose quorum for 54 ranges whose voters still include dead store 1. Both basement
stores are currently required for those ranges. Do not drain/stop/reset either
node until replacement replicas have converged and the gate below is clear.
The canary's successful stop established safety only for store 5, not stores 3/4.

### 1. Check database quorum from the operator workstation

Confirm all three live database members are healthy and that the current mapping
is store 3 = basementbaddy-1, store 4 = basementbaddy-0, store 5 = beebud-0.
The following read-only query uses the matching client image and workstation
admin certificates; keys stay on the workstation. It assumes that mapping.

```sh
DB_CERTS="$HOME/.local/share/infestation/vms/beebuddy/pki/admin"
docker run --rm -i --network host --dns=100.100.100.100 --user 0 \
  --mount "type=bind,src=$DB_CERTS,dst=/certs,readonly" \
  --entrypoint /cockroach/cockroach cockroachdb/cockroach:v26.2.7 \
  sql --host=beebud-0.quoll-duck.ts.net:26257 --certs-dir=/certs <<'SQL'
SET statement_timeout = '15s';
SET allow_unsafe_internals = true;
SELECT
 count(*) FILTER (WHERE 2 * (SELECT count(*) FROM unnest(r.voting_replicas) AS v(store_id) WHERE v.store_id IN (3,5)) <= cardinality(r.voting_replicas)) AS unsafe_for_basementbaddy_0,
 count(*) FILTER (WHERE 2 * (SELECT count(*) FROM unnest(r.voting_replicas) AS v(store_id) WHERE v.store_id IN (4,5)) <= cardinality(r.voting_replicas)) AS unsafe_for_basementbaddy_1,
 count(*) FILTER (WHERE cardinality(r.learner_replicas)>0 OR cardinality(r.non_voting_replicas)>0) AS transitional_ranges
FROM crdb_internal.ranges_no_leases AS r;
SQL
```

The selected node's unsafe count and the transitional count must be **zero**.
Recheck immediately before its stop, and do not perform other node lifecycle
operations concurrently. The observed result when these instructions were
written was `54, 54, 0`: neither stop is currently permitted. Resolve replica
placement first; do not lower replication or use `--allow-outage` to bypass this.

### 2. Build the current CLI and prepare one private profile

The profile command and drain certificate fix are in the current checkout;
do not assume an older installed/published binary contains them. On the operator
workstation:

```sh
cd /home/pmiller/infestation
go build -o bin/infestation ./cmd/infestation
scp bin/infestation millerbase:/tmp/infestation-memory-upgrade
ssh millerbase
bash
```

Run the remaining compute commands in that Bash session on `millerbase`.
Start with index **0**. After its entire recovery and observation period, rerun
the procedure with index **1** and a fresh backup directory; never stop the whole
`basementbaddy` group or both instances together.

```sh
set -euo pipefail
umask 077
UPGRADE_INDEX=0
UPGRADE_NODE="basementbaddy-$UPGRADE_INDEX"
UPGRADE_BIN=/tmp/infestation-memory-upgrade
UPGRADE_GROUP="$HOME/.local/share/infestation/vms/basementbaddy"
UPGRADE_VM="$UPGRADE_GROUP/$UPGRADE_NODE"
UPGRADE_BACKUP="$HOME/.local/share/infestation/upgrade-backups/$UPGRADE_NODE-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$UPGRADE_BACKUP"
cp "$UPGRADE_VM/config.ign" "$UPGRADE_BACKUP/original.ign"
cp "$UPGRADE_GROUP/nodes.json" "$UPGRADE_BACKUP/original-nodes.json"
"$UPGRADE_BIN" reprovision-profile \
  --source "$UPGRADE_BACKUP/original.ign" \
  --out "$UPGRADE_BACKUP/replacement.ign" \
  --max-go-memory 768MiB --max-sql-memory 256MiB
```

Before disruption, record the Tailscale device ID/IP, machine ID, database
node/store IDs, CA fingerprint and SSH public-key fingerprints for comparison.
Inventory the selected node's pods/PVs for custom local storage: add every
required local path to the retention list below. Review the deletion preview as
described above, including the confext workaround if needed. Profile preparation
does not by itself establish that the retention list covers workload data.

### 3. Drain and take the offline backup

Proceed only after step 1 permits this specific node. These Kubernetes commands
use the healthy `beebud-0` API; they do not require copying kubeconfig to millerbase.

```sh
tailscale ssh core@beebud-0 sudo -n /usr/local/bin/k3s kubectl \
  drain "$UPGRADE_NODE" --ignore-daemonsets --timeout=2m
tailscale ssh "core@$UPGRADE_NODE" \
  sudo -n tailscale serve --service=svc:k8s-api --tcp=443 off
"$UPGRADE_BIN" vm stop --name basementbaddy --instance "$UPGRADE_INDEX"
test ! -e "$UPGRADE_VM/qmp.sock"
test ! -e "$UPGRADE_BACKUP/before-reset.qcow2"
qemu-img convert -O qcow2 "$UPGRADE_VM/disk.qcow2" \
  "$UPGRADE_BACKUP/before-reset.qcow2"
qemu-img check "$UPGRADE_BACKUP/before-reset.qcow2"
"$UPGRADE_BIN" vm start --name basementbaddy --instance "$UPGRADE_INDEX"
```

Every command must succeed, including confirmed database drain and image check.
If Kubernetes drain refuses a disruption or local data, investigate it rather
than adding force flags. Leave the node cordoned during the whole procedure.
Wait for Tailscale access and both CockroachDB and K3s to recover before staging
anything; inspect with:

```sh
tailscale ssh "core@$UPGRADE_NODE" \
  sudo -n systemctl is-active cockroachdb.service k3s.service
```

### 4. Stage the reviewed reset, stop, update saved profiles, start

Upload without printing profile contents. Compare the local and guest SHA256
outputs before running `flatcar-reset`.

```sh
tailscale ssh "core@$UPGRADE_NODE" \
  'sudo -n sh -c "umask 077; cat > /run/infestation/replacement.ign"' \
  < "$UPGRADE_BACKUP/replacement.ign"
sha256sum "$UPGRADE_BACKUP/replacement.ign"
tailscale ssh "core@$UPGRADE_NODE" \
  sudo -n sha256sum /run/infestation/replacement.ign
```

The following list includes the SSH host-key fix and the caches used by the
canary. Extend it for any custom local volumes found above. Quotes around the
SSH pattern prevent shell wildcard expansion.

```sh
tailscale ssh "core@$UPGRADE_NODE" 'sudo -n flatcar-reset \
  --ignition-file /run/infestation/replacement.ign --keep-machine-id \
  --keep-paths \
  /var/lib/cockroach /var/lib/tailscale /var/lib/rancher/k3s \
  /var/lib/kubelet /etc/rancher/node /var/lib/infestation \
  /etc/cockroach/certs "/etc/ssh/ssh_host_.*" \
  /var/lib/docker /var/lib/containerd /var/log \
  /opt/infestation/infestation-node \
  /opt/infestation/helper-build.identity /opt/infestation/helper-build.sha256'
```

This stages a reset for the next boot. Its no-`--backup` warning refers to the
in-guest backup mechanism; the checked, offline standalone disk copy above is
our separate recovery artifact. If aborting before boot, use
`sudo flatcar-reset --stop` on the guest to clear the pending reset.

Refresh the quorum gate, then stop through the same drain workflow. The initial
restart may have re-advertised the API, so withdraw it again.

```sh
tailscale ssh "core@$UPGRADE_NODE" \
  sudo -n tailscale serve --service=svc:k8s-api --tcp=443 off
"$UPGRADE_BIN" vm stop --name basementbaddy --instance "$UPGRADE_INDEX"
test ! -e "$UPGRADE_VM/qmp.sock"
install -m 600 "$UPGRADE_BACKUP/replacement.ign" "$UPGRADE_VM/config.ign"
install -m 600 "$UPGRADE_BACKUP/replacement.ign" \
  "$UPGRADE_GROUP/output/$UPGRADE_NODE.ign"
"$UPGRADE_BIN" vm start --name basementbaddy --instance "$UPGRADE_INDEX"
```

### 5. Verify before uncordoning or selecting instance 1

Wait for access, then check the guard and services. Compare identity observations
with step 2; the SSH fingerprint should now stay unchanged.

```sh
tailscale ssh "core@$UPGRADE_NODE" 'sudo -n systemctl is-active \
  infestation-retained-state.service cockroachdb.service k3s.service tailscale-api.service'
tailscale ssh "core@$UPGRADE_NODE" 'sudo -n curl -fsS \
  --cacert /etc/cockroach/certs/ca.crt https://localhost:8080/_status/vars' \
  | grep '^sys_go_limitbytes{'
tailscale ssh "core@$UPGRADE_NODE" \
  sudo -n ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
tailscale ssh core@beebud-0 sudo -n /usr/local/bin/k3s kubectl get nodes
```

Require `sys_go_limitbytes` = **805306368**, SQL budget **256MiB**, the same
CockroachDB node/store IDs, healthy SQL/range availability, and recovered K3s/API.
Inspect the container arguments with `sudo docker inspect --format '{{json .Args}}'
cockroachdb` on the guest. When those checks pass:

```sh
tailscale ssh core@beebud-0 sudo -n /usr/local/bin/k3s kubectl \
  uncordon "$UPGRADE_NODE"
```

Observe GC CPU, total CPU, RSS, host memory and SQL/API errors for at least
5–10 minutes under representative workload. Rerun step 1 before considering
index 1; all three live nodes must have recovered. Do not treat Kubernetes Ready
alone as permission to stop another database member.

After both nodes succeed, update the saved group's desired memory settings
without changing identity/ownership fields:

```sh
jq '.cockroach_go_memory="768MiB" | .cockroach_sql_memory="256MiB"' \
  "$UPGRADE_GROUP/nodes.json" > "$UPGRADE_BACKUP/nodes-updated.json"
install -m 600 "$UPGRADE_BACKUP/nodes-updated.json" "$UPGRADE_GROUP/nodes.json"
```

On a failure, halt the rollout and retain all artifacts. Do not initialize or
format anything, and do not blindly restore an old CockroachDB disk snapshot
into the running cluster; database recovery needs its own reviewed procedure.
This procedure changes memory configuration only; CockroachDB/K3s version
changes still require supported mixed-version enrollment handling.
