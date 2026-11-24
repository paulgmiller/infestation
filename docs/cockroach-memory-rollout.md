# Immutable node configuration upgrades

New profiles use `--max-go-memory=768MiB` and `--max-sql-memory=256MiB`.
The JSON settings `cockroach_go_memory` and `cockroach_sql_memory` override
these values. Go memory is a soft runtime limit, not a total-process cap.

Existing nodes require an immutable upgrade workflow; manually editing installed
systemd units is not the deployment model. This workflow is not implemented or
live-verified. Updating source or replacing an attached Ignition file does not
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

Future implementation and resource sizing remain tracked in [TODO](../TODO.md).
No live node has been changed by the memory-default source update.

References: [Flatcar first-boot configuration](https://www.flatcar.org/docs/latest/getting-started/quickstart/),
[Flatcar reprovisioning](https://www.flatcar.org/docs/latest/fb-provision/infrastructure/),
[CockroachDB memory flags](https://docs.cockroachlabs.com/docs/v26.2/cockroach-start),
and [database draining](https://docs.cockroachlabs.com/docs/v26.2/node-shutdown).
