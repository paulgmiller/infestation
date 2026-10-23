# Flatcar + CockroachDB + K3s (three-node bootstrap)

This is a **starter profile**, not a tested production deployment. It generates **three separate Ignition v3.3 JSON files** for Flatcar Linux. There is **no Tailscale** and **no CNI** installed yet; K3s is configured for later Cilium installation.

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
- **Verification:** Generated JSON is syntax-checked by the generator, but hasn't been validated with a live Flatcar first boot or Cockroach/K3s integration test in this environment. Test on disposable nodes before using real data.

## References

- [Flatcar systemd-sysext / community K3s sysext](https://flatcar.github.io/sysext-bakery/k3s/)
- [Flatcar Ignition/Butane](https://www.flatcar.org/docs/latest/getting-started/quickstart/)
- [K3s external datastore](https://docs.k3s.io/datastore)
- [K3s custom CNI](https://docs.k3s.io/networking/basic-network-options)
- [CockroachDB node/client certificate requirements](https://www.cockroachlabs.com/docs/v26.2/create-security-certificates-custom-ca)
- [CockroachDB first initialization](https://www.cockroachlabs.com/docs/stable/start-a-local-cluster.html)
