# Flatcar + CockroachDB + K3s (three-node bootstrap)

This is a **starter profile**, not a tested production deployment. It generates **three separate Ignition v3.3 JSON files** for Flatcar Linux. There is **no Tailscale** and **no CNI** installed yet; K3s is configured for later Cilium installation.

For a disposable **QEMU lab**, including growing to three CockroachDB/K3s hosts and the
network layout, see [QEMU-LAB.md](QEMU-LAB.md).
That profile uses the same generator and TLS configuration, with a separate
ignored state directory. It starts in CockroachDB's `start-single-node` mode,
then migrates the existing store into a three-node replicated cluster.

## Resulting layout

On each node:

- Flatcar Linux, using existing host networking (e.g., DHCP reservations).
- The pinned community K3s **systemd-sysext** image, loaded at first boot.
- Docker-managed, pinned CockroachDB, supervised by `cockroachdb.service` and **not a Kubernetes pod**.
- K3s server with external SQL datastore pointed to **localhost:26257**, using TLS client certificates.
- A data directory `/var/lib/cockroach` on Flatcar's persistent `/var` (change this to a dedicated data disk before committing important data).
- `flannel-backend: none`, `disable-network-policy: true`, packaged Traefik/ServiceLB disabled. kube-proxy **remains enabled**.
- Flatcar unattended **reboots disabled** (`REBOOT_STRATEGY=off`) until you implement Cockroach-aware rolling maintenance. OS updates may still be staged.

## Prerequisites

1. Three x86_64 machines to boot **fresh** Flatcar images (Ignition is first-boot provisioning).
2. Your management Linux workstation has `python3` and Docker, and can reach node 1 on TCP 26257 for one-time initialization.
3. The Flatcar machines have network connectivity and unique stable IPv4 addresses. Configure DHCP reservations or host networking independently; the `node-ip` setting **does not configure host IP addresses**.
4. Allow node-to-node communication: TCP 26257 (CockroachDB), TCP 6443/9345 as needed (K3s), TCP 10250 (kubelet), plus the Cilium-specific traffic when you install it. Firewall the services to trusted networks.
5. Configure a real SSH public key in `cluster.json`.

## Generate profiles

Edit `cluster.json` to set your **actual** node names, IPs, and SSH public key. All three nodes must start with **empty CockroachDB data stores** when forming a brand-new cluster.

```bash
python3 generate.py pki
python3 generate.py ignition
ls output/*.ign
```

The `pki` command runs the CockroachDB CLI from the pinned Docker image to create a CA, a **unique node certificate for each server**, a unique Kine client certificate per server, and a separate root admin client certificate. It refuses to overwrite an existing PKI directory. If certificate generation fails partway through, inspect and back up existing keys before intentionally restarting.

The `ignition` command creates a random 256-bit K3s server token on first use (`pki/k3s-token`), then writes `output/kube1.ign`, `output/kube2.ign`, `output/kube3.ign` according to `cluster.json` names. **These files contain secrets** (K3s token, node private key, Kine private key). Store them securely, pass them over a trusted provisioning path, do not commit them to Git, and keep the `pki/ca.key` and root admin certificates on your admin machine only.

## Worker mode

Add worker entries to `nodes` in `cluster.json` alongside the three servers:

```json
{"name": "worker1", "ip": "192.168.1.104", "role": "worker"}
```

An omitted `role` still means `server`. Workers are additional nodes and do not
count toward the three-server requirement (one server with `single_node_lab`).
Include workers before running `python3 generate.py pki`, then run
`python3 generate.py ignition` to emit `output/worker1.ign`. When adding workers
to an existing cluster, provision their CockroachDB node certificates signed by
the existing CA before generating Ignition; `pki` refuses to overwrite existing PKI.
This generates first-boot profiles; it does not convert an existing running server.

A worker enables the sysext's `k3s-agent.service`, which runs the K3s agent
including kubelet, containerd, and kube-proxy, without running an API server or
other control-plane components. The K3s agent has no SQL connection or Kine credentials.
CockroachDB defaults to on for both servers and workers, as a separate host
database process with its own node certificates. Set `cockroachdb: false` on
a worker to omit that process and its certificates. Database-enabled workers
participate in the same CockroachDB join list as database-enabled servers.
See the [Flatcar agent service configuration](https://flatcar.github.io/sysext-bakery/k3s/)
and [K3s agent options](https://docs.k3s.io/cli/agent).

By default, a worker joins `https://<first-server-ip>:6443` using the token in
`pki/k3s-token`. Set `server_url` to override the join endpoint:

```json
{"name": "worker1", "ip": "192.168.1.104", "role": "worker", "server_url": "https://192.168.1.101:6443"}
```

For workers with `cockroachdb: false`, you can also use a configuration containing
only workers, each with an explicit `server_url`. Keep database-enabled workers
in the full cluster configuration so their SQL join list includes existing hosts.
Before generating worker-only Ignition, create a private `pki/` directory and
put the existing cluster's join token in `pki/k3s-token` with mode `0600`.
The generator refuses to invent a new token for a workers-only configuration.
Use an HTTPS endpoint reachable from the worker and covered by the server's TLS
certificate. Workers inherit the server's CNI settings; the existing cluster
must use the intended Cilium configuration.

After provisioning a fresh worker, inspect `sudo systemctl status k3s-agent`
and `sudo journalctl -u k3s-agent` on it. From a server or admin workstation,
check `kubectl get nodes -o wide` and wait for the worker to become Ready after
Cilium starts on it. Worker generation is unit-tested; a worker VM join has
not yet been verified.

## Install and boot

Deliver each `.ign` file as the first-boot Ignition configuration for the corresponding Flatcar machine. The installer command/path depends on your boot/install mechanism; for example the Flatcar installer accepts `-i` to refer to the local Ignition config. Select your disks carefully and consult the [Flatcar bare-metal documentation](https://www.flatcar.org/docs/latest/installing/bare-metal/).

Boot all three machines. Check on each:

```bash
sudo systemctl status docker cockroachdb k3s
sudo journalctl -fu cockroachdb
sudo journalctl -fu k3s
```

CockroachDB will start and wait for its cluster initialization; K3s might retry its SQL connection until the initialization and SQL account creation are complete.

## Initialize CockroachDB exactly once

**Only for a brand-new CockroachDB cluster**, once all nodes are running their CockroachDB processes, from the Linux management workstation:

```bash
./bootstrap-db.sh
```

This runs CockroachDB's **one-time** `cockroach init`, then creates the `kubernetes` database, certificate-authenticated `kine` SQL user, and permissions for the public schema. **Do not run the `init` command on an already initialized cluster.** If the initial `init` succeeded but subsequent SQL failed, run only the SQL portion of `bootstrap-db.sh` (the `cockroach sql` invocation), not the init again.

A K3s service that previously failed to start can be restarted on each node:

```bash
sudo systemctl restart k3s
sudo k3s kubectl get nodes -o wide
```

Nodes should be registered, but likely **NotReady until you install Cilium**. Use [Cilium's K3s instructions](https://docs.cilium.io/en/stable/installation/k3s/) after the Kubernetes API is available.

## Design cautions

- **Version pins:** The examples pin CockroachDB `v26.2.7` and K3s sysext `v1.35.8+k3s1` for **x86-64**, versions listed in published image/release indexes when generated. Update only with a planned, tested rolling procedure. The K3s sysext is community-supported by Flatcar's bakery, not a Flatcar core tested extension.
- **Security:** CockroachDB uses certificate-based TLS even though it is on your LAN. Keep the Ignition configs and CA private key private. Restrict TCP 26257 to trusted peers.
- **Single local SQL endpoint:** K3s connects to `127.0.0.1:26257`. If a machine's CockroachDB process fails but peers remain healthy, **its local API server still loses its datastore**. Add a host-level HAProxy/fallback later if you need local-process-level failover.
- **Reboot discipline:** At RF=3, a three-node CockroachDB cluster can normally lose one replica-host; don't reboot two hosts together. Independently assess the OS update/reboot process.
- **Disk:** `/var/lib/cockroach` is on the host persistent filesystem, not a dedicated disk. Mount a dedicated persistent filesystem there before a real deployment, and set up backups.
- **No stable Kubernetes API address:** Additional servers join via the external SQL datastore; agents and external users should eventually use a stable HA API address/LB. `tls-san` currently includes only the node IP.
- **Verification:** The QEMU lab grew to three hosts, each running CockroachDB and K3s with local TLS SQL. All three nodes reached Ready, all APIs passed readiness, all six directed DNS/service HTTP tests passed, and CockroachDB reported three live nodes with RF=3 and no under-replicated ranges. A Kubernetes write/read passed through the surviving nodes while kube1's SQL process was stopped, then that service was restored. Single-node reboot recovery also passed earlier. See [QEMU-LAB.md](QEMU-LAB.md) for versions and limits. Direct fresh-three-node initialization remains untested.
- **CockroachDB compatibility:** Bootstrap SQL sets the Kine user's `serial_normalization=sql_sequence` before Kine creates its table. Existing tables created with `unique_rowid()` are not repaired by that setting. Kine also issues a PostgreSQL `pg_total_relation_size()` query that CockroachDB does not implement; the lab logs datastore-size/status warnings even while API readiness passes. CockroachDB is not among K3s's officially supported external datastores.

## References

- [Flatcar systemd-sysext / community K3s sysext](https://flatcar.github.io/sysext-bakery/k3s/)
- [Flatcar Ignition/Butane](https://www.flatcar.org/docs/latest/getting-started/quickstart/)
- [K3s external datastore](https://docs.k3s.io/datastore)
- [K3s custom CNI](https://docs.k3s.io/networking/basic-network-options)
- [CockroachDB node/client certificate requirements](https://www.cockroachlabs.com/docs/v26.2/create-security-certificates-custom-ca)
- [CockroachDB first initialization](https://www.cockroachlabs.com/docs/stable/start-a-local-cluster.html)
