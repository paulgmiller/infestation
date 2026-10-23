# Disposable single-node QEMU lab

This lab provisions Flatcar through Ignition, runs secure CockroachDB in a
host Docker container, and runs K3s against that SQL datastore through Kine.
Cilium is installed after the API starts. It uses no embedded etcd, SQLite, or
Flannel. Kube-proxy remains enabled.

The host needs x86-64 Linux, access to `/dev/kvm`, Python 3.12+, `curl`,
`ssh-keygen`, SSH, Docker, and `qemu-system-x86_64` / `qemu-img`:

```bash
sudo apt-get update
sudo apt-get install -y --no-install-recommends qemu-system-x86 qemu-utils
```

Run the following from this directory. The VM uses 4 vCPUs, 6 GiB RAM,
and a sparse 40 GiB overlay over an unmodified Flatcar base image.
All VM state, SSH credentials, PKI, Ignition, and kubeconfig live in ignored
`lab/`. The lab creates its own SSH key; no change to `cluster.json` is needed.

```bash
python3 qemu-lab.py prepare
python3 qemu-lab.py start
# First boot downloads the K3s extension and CockroachDB image. Allow a few minutes.
python3 qemu-lab.py ssh 'systemctl is-active cockroachdb; sudo journalctl -u k3s -n 15 --no-pager'
# Retry this SQL command once CockroachDB is listening. It is idempotent.
python3 qemu-lab.py bootstrap-db
python3 qemu-lab.py ssh 'sudo k3s kubectl get nodes'
python3 qemu-lab.py kubeconfig
python3 qemu-lab.py cilium install --version 1.20.4 \
  --set operator.replicas=1 --set kubeProxyReplacement=false \
  --set ipam.operator.clusterPoolIPv4PodCIDRList=10.42.0.0/16 \
  --set cni.binPath=/opt/cni/bin --set cni.confPath=/etc/cni/net.d
python3 qemu-lab.py cilium status --wait --wait-duration 45s --interactive=false
python3 qemu-lab.py smoke
```

The smoke test deploys a small HTTP server and a job in `lab-smoke`. The job
checks Kubernetes DNS and fetches the server through its DNS name and ClusterIP.
Startup retries can leave a failed job pod beside the successful pod; the job
must have condition `Complete`, and successful logs include
`PASS: pod DNS and HTTP through ClusterIP service`.
To run a fresh job after a previous success:

```bash
python3 qemu-lab.py ssh 'sudo k3s kubectl delete job -n lab-smoke dns-http'
python3 qemu-lab.py smoke
```

When upgrading Cilium, repeat `--set kubeProxyReplacement=false`; CLI detection
looks for a kube-proxy DaemonSet and can miss K3s's embedded kube-proxy.
With `flannel-backend: none`, this K3s release leaves CNI directories at
containerd defaults: `/opt/cni/bin` and `/etc/cni/net.d`.

Only localhost ports are forwarded, using QEMU user networking; no host bridge
or firewall changes are needed:

| Host | Guest | Purpose |
| --- | --- | --- |
| 127.0.0.1:2222 | 22 | SSH as core |
| 127.0.0.1:6443 | 6443 | Kubernetes API |
| 127.0.0.1:26257 | 26257 | TLS SQL with lab admin certificate |

For host kubectl, set `KUBECONFIG=$PWD/lab/kubeconfig`.
For guest kubectl, use `python3 qemu-lab.py ssh 'sudo k3s kubectl get pods -A'`.
For SQL, use `python3 qemu-lab.py sql 'SHOW DATABASES;'`.
The CA private key and root client key stay on the management host.

Stop gracefully with `python3 qemu-lab.py stop`; wait for `lab/qemu.pid` to
disappear. `start` then reuses the existing disk. Ignition runs only on first
boot; regenerating Ignition does not modify an already provisioned guest.
For a fresh lab, stop it, rename `lab/` to a private backup directory, then run
`prepare` and `start` again. Never reuse these lab credentials for real nodes.

## Observed validation, 2026-10-23

- Flatcar **4757.2.2**, image verified against vendor SHA-512 digest.
- CockroachDB **v26.2.7**, TLS server verification and certificate-authenticated
  Kine connection; no insecure mode.
- K3s **v1.35.8+k3s1**, community Flatcar sysext loaded on first boot;
  Kine dependency **v0.16.4**.
- Cilium **1.20.4**, CLI **v0.20.1**, CLI archive verified by SHA-256.
- Node `kube1` reached `Ready`; API `/readyz?verbose` passed every check;
  Cilium, Envoy, operator, CoreDNS, metrics-server, and local-path provisioner ran.
- Pod DNS and service HTTP smoke job passed. Kine's initial 495 revisions were
  observed as IDs 1 through 495, backed by `nextval(kine_id_seq)`.
- Reboot recovery passed: CockroachDB and K3s restarted automatically, API
  readiness passed, Cilium reported healthy, and a newly created DNS/HTTP job
  passed again. The persisted Kine table had 1716 revisions with IDs 1–1716
  when checked after reboot. Guest shutdown took a few minutes while systemd
  waited for a remaining workload process; allow for this when stopping the VM.
- Python generator tests passed; shell syntax and Python compilation passed.
  Ignition was validated by actual first-boot execution, not a separate schema CLI.

First boot exposed an existing `/etc/flatcar/update.conf`: generated Ignition
now explicitly overwrites managed files. Also moved systemd's
`StartLimitIntervalSec` setting into `[Unit]`.

## Limits

This is a single-node integration smoke test, not an HA or durability guarantee.
The three-node profile remains untested. CockroachDB is not on K3s's official
supported datastore list. Kine's PostgreSQL adapter calls
`pg_total_relation_size()`, which CockroachDB lacks: recurring storage-size/status
warnings remain, despite successful API readiness and workload tests. No fake
compatibility function was installed to suppress those warnings.

`bootstrap.sql` configures `serial_normalization=sql_sequence` before table
creation. It cannot fix a pre-existing Kine table with a `unique_rowid()` default.
The lab's CockroachDB initializes itself through `start-single-node`; do not run
`bootstrap-db.sh` / `cockroach init` against this lab.

## Three-node smoke procedure (not run)

For three disposable VMs, generate separate SSH credentials, certificates, and
Ignition from the normal three-node profile with three unique reachable guest
addresses. Use an isolated guest network that permits node-to-node traffic;
three separate QEMU user networks sharing `10.0.2.15` do not provide that.
Boot each fresh overlay with its corresponding Ignition, wait for all three
CockroachDB containers, and run `bootstrap-db.sh` exactly once from a management
host that can reach node 1. Check all three K3s APIs, install Cilium once with
the matching pod CIDR and default CNI paths, wait for three Ready nodes, and
run pod/service connectivity tests across nodes before any failover tests.

## Vendor references

- [Flatcar QEMU quickstart](https://www.flatcar.org/docs/latest/getting-started/quickstart/)
- [Flatcar community K3s sysext](https://flatcar.github.io/sysext-bakery/k3s/)
- [K3s datastore support and sequential revision requirements](https://docs.k3s.io/datastore)
- [Cilium on K3s](https://docs.cilium.io/en/stable/installation/k3s/)
- [K3s v1.35.8 containerd config template](https://github.com/k3s-io/k3s/blob/v1.35.8%2Bk3s1/pkg/agent/templates/templates.go)
- [Kine PostgreSQL adapter](https://github.com/k3s-io/kine/blob/v0.16.4/pkg/drivers/pgsql/pgsql.go)
- [CockroachDB SERIAL behavior](https://www.cockroachlabs.com/docs/v26.2/serial)
