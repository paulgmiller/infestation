> Historical smoke-test record. The project is now the root Go module; see [migration notes](../../docs/migration.md) for current commands and preserved state.

# Local smoke deployment: repeat and troubleshoot

This records the successful 2026-10-25 deployment. Start a fresh deployment with
the [README procedure](README.md); the commands below also identify the existing
stack and the lessons from running it. These addresses belong to this lab, so
choose unused addresses and resource names for a second cluster.

## Existing stack

Run from `infra/qemu` on the VM host:

```bash
python3 state/run.py pulumi stack select smoke
python3 state/run.py pulumi stack output
python3 state/run.py pulumi preview --refresh --diff
export KUBECONFIG="$PWD/state/kubeconfig"
kubectl get nodes -o wide
python3 state/check.py db-status
```

The last verified refresh reported 14 unchanged resources. The private wrapper
selects `state/backend`, `state/pulumi-home`, `state/passphrase`, and the pinned
CLI in `state/tools`. An ordinary `pulumi` invocation may select another backend;
check the backend and stack before applying anything. These private helpers and
credentials are ignored local files, not part of a fresh checkout.

| Item | Smoke deployment |
| --- | --- |
| Stack / resource prefix | `smoke` / `flatcar-pulumi-test` |
| VM names | `flatcar-pulumi-test-kube1`, `-kube2`, `-kube3` |
| VM addresses | `192.168.124.11`, `.12`, `.13` |
| Network / bridge / gateway | `flatcar-pulumi-test` / `virbr1` / `192.168.124.1` |
| Pool directory | `/var/lib/libvirt/images/flatcar-pulumi-test` |
| QEMU service account | `libvirt-qemu`, UID/GID `954:954` on this host |
| VM sizing | 4 GiB RAM, 2 vCPUs, 40 GiB overlay each |
| Admin kubeconfig / SSH key | `state/kubeconfig` / `state/id_ed25519` |

Keep the local backend, encryption passphrase, cluster config, SSH key, and PKI
with private backups. Do not regenerate certificates to reconnect to this stack.

## Host setup lessons

Install QEMU/libvirt/dnsmasq and start libvirtd and Docker before provisioning.
Check `/dev/kvm` on the actual host: a restricted command sandbox can hide the
device and host networking even when both work outside it.

Use the account that actually runs QEMU for pool ownership. This machine has
both `qemu` and `libvirt-qemu`; they have different IDs. Do not copy the numeric
IDs above onto another machine without checking its account database.

On this Omarchy host, membership in `libvirt` avoids repeated management polkit
dialogs. After an administrator adds the user, log in again or use:

```bash
newgrp libvirt -c 'virsh -c qemu:///system list --all'
```

The old session does not acquire the new supplementary group automatically.
`sg` was unavailable; `newgrp -c` worked. When remoted in, run privileged setup
through a terminal with `sudo`; graphical authorization dialogs may be invisible.

UFW blocked DHCP and first-boot downloads despite the VMs showing as running.
These were the scoped rules used here, entered by the administrator. Inspect
the actual bridge, subnet, and uplink before adapting them:

```bash
sudo ufw allow in on virbr1 proto udp from any to any port 67 comment 'Pulumi test DHCP'
sudo ufw allow in on virbr1 proto udp from 192.168.124.0/24 to 192.168.124.1 port 53 comment 'Pulumi test DNS'
sudo ufw allow in on virbr1 proto tcp from 192.168.124.0/24 to 192.168.124.1 port 53 comment 'Pulumi test DNS'
sudo ufw route allow in on virbr1 out on enp9s0 from 192.168.124.0/24 to any comment 'Pulumi test egress'
sudo ufw route allow in on virbr1 out on virbr1 from 192.168.124.0/24 to 192.168.124.0/24 comment 'Pulumi test peers'
```

The firewall stayed enabled. Adding new rules is not necessary to repeat checks
on the existing stack. Libvirtd and Docker were started for the test; persistent
host service enablement was not configured, and VM autostart is false.

## Diagnose in dependency order

```bash
virsh --readonly -c qemu:///system list --all
virsh --readonly -c qemu:///system net-info flatcar-pulumi-test
virsh --readonly -c qemu:///system net-dhcp-leases flatcar-pulumi-test
virsh --readonly -c qemu:///system net-dumpxml flatcar-pulumi-test
python3 state/check.py ssh kube1 'sudo systemctl status cockroachdb k3s --no-pager'
python3 state/check.py ssh kube1 'sudo journalctl -b -u cockroachdb -u k3s --no-pager -n 100'
```

No DHCP lease: inspect bridge/firewall first. Lease but downloads fail: check DNS
and forwarded egress. Database ready but Kubernetes nodes NotReady: check Cilium
installation and image pulls. The first Cilium status wait timed out during
image pulls; a later status check succeeded.

For a serial console, use `virsh -c qemu:///system console
flatcar-pulumi-test-kube1` with management group access. Exit with Ctrl+].
Read-only commands work without a management authorization prompt.

## Bootstrap and acceptance checks

For a **fresh** three-node database, initialize CockroachDB exactly once, then
apply `../../bootstrap.sql` with the admin certificates. The existing stack is
already initialized; do not repeat initialization. K3s needs the Kine database,
grants, and `serial_normalization='sql_sequence'` setting from that SQL file.

The tested Cilium installation used CLI 0.20.1:

```bash
state/tools/cilium --kubeconfig state/kubeconfig install --version 1.20.4 \
  --set operator.replicas=2 --set kubeProxyReplacement=false \
  --set ipam.operator.clusterPoolIPv4PodCIDRList=10.42.0.0/16 \
  --set cni.binPath=/opt/cni/bin --set cni.confPath=/etc/cni/net.d
```

Use `install` only for a fresh cluster. To inspect the running cluster:

```bash
state/tools/cilium --kubeconfig state/kubeconfig status --wait
kubectl -n kube-system get configmap cilium-config -o jsonpath='{.data.kube-proxy-replacement}'
kubectl get nodes -o wide
kubectl get pods -A
kubectl get --raw=/readyz
python3 state/check.py db-status
```

Acceptance results: three Ready nodes, all three APIs ready, three live TLS
database nodes with no unavailable or under-replicated ranges, Cilium/Envoy 3/3,
operators 2/2, and effective kube-proxy replacement `false`. Six directed
node-pair DNS and service HTTP checks passed in namespace `lab-cross`. The local
`state/check.py cross-smoke` helper recreates that test's resources, so run it
deliberately rather than treating it as a read-only status command.

## Host-to-service test

The existing `lab-cross/web-kube1` service was changed to NodePort:

```bash
kubectl -n lab-cross patch service web-kube1 --type=merge -p '{"spec":{"type":"NodePort"}}'
kubectl -n lab-cross get service web-kube1
```

Kubernetes allocated TCP port `32188`. Host requests to each of
`http://192.168.124.11:32188/`, `http://192.168.124.12:32188/`, and
`http://192.168.124.13:32188/` all returned `kube1`. This exercises both local and
cross-node forwarding to the pod selected by that service. A recreated service
may receive a different port; inspect it before querying. No new firewall rules
were needed for these host-originated requests.

## Stopping and resuming

The local stack's `running` setting was changed to `false` before switching to
the Azure experiment. Shut guests down gracefully through libvirt and wait for
`shut off` in `virsh list --all`; preserve their disks and credentials. After a
refresh, the stopped state should match Pulumi configuration. To resume later:

```bash
python3 state/run.py pulumi config set running true --stack smoke
python3 state/run.py pulumi preview --refresh --diff --stack smoke
# Review that only the intended domains start before applying:
python3 state/run.py pulumi up --stack smoke
```

Starting existing domains is different from creating a fresh database. Do not
repeat PKI generation or CockroachDB initialization when resuming.

## Provider and lifecycle lessons

- Generate the pinned libvirt SDK with `pulumi install` before Go builds/tests.
- Serial/console PTY sources need explicit empty paths when creating domains.
  Ignore the runtime-assigned PTY paths to avoid spurious VM updates on refresh.
- Use ACPI and Flatcar's `fw_cfg` Ignition entry. Upload private Ignition directly
  as mode `0600`; the provider's separate Ignition helper created `0644` files.
- Ignition runs on first boot. Regenerating it does not update existing nodes.
  Protected, immutable volumes prevent accidental replacement of database disks.
- The pinned provider replaces networks for DHCP inventory changes. Plan the
  initial inventory; adding nodes to a live protected network needs more work.
- Keep Go module caches outside packages traversed by `go test ./...`, or give
  the cache directory a separate `go.mod` (as in this local `state/` directory).

The kubeconfig currently targets kube1 directly. A host API load balancer was
discussed but has not been deployed. LAN clients need routing or an SSH tunnel
to reach this private subnet. Post-bootstrap reboot recovery, API failover, and
durability under faults remain untested.
