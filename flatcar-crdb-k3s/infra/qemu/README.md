# Local QEMU/KVM with Pulumi Go and libvirt

For the tested host setup, existing-stack commands, and troubleshooting lessons,
see the [smoke deployment runbook](RUNBOOK.md).

This target creates a fresh local cluster through `qemu:///system`: a directory
storage pool, an IPv4 NAT network, fixed DHCP reservations, a read-only base image,
one persistent qcow2 overlay per node, and a private Ignition volume per node.
The same `cluster.json` and `generate.py` used for Azure supply certificates and
host configuration. Workers run the K3s agent and CockroachDB by default.

It is separate from `qemu-lab.py` and its `lab/` directory. It does not adopt,
modify, stop, or delete that lab's VMs, disks, or credentials.

## Prerequisites

- Linux on x86-64 with KVM available and a working local libvirt/QEMU installation.
- Permission to manage `qemu:///system`, including storage pools and NAT networks.
- Pulumi CLI **3.272.0**, Go **1.26.6 or later**, Python 3, curl, and Docker for PKI generation.
- Enough RAM for all nodes; the default is 3 GiB and two vCPUs per VM.
- A dedicated pool directory and a private subnet that do not overlap existing
  libvirt networks, your LAN/VPN, or Kubernetes pod/service networks.

Check existing host resources without changing them:

```bash
virsh --readonly -c qemu:///system list --all
virsh --readonly -c qemu:///system net-list --all
virsh --readonly -c qemu:///system pool-list --all
```

Identify the numeric UID/GID of the account that actually runs QEMU. Common
account names are `qemu` and `libvirt-qemu`; consult your host's libvirt setup.
For example, inspect `getent passwd qemu` and `getent group qemu`. Do not use your
own UID merely because you invoke Pulumi. The pool uses mode `0700` and volumes
use `0600`, owned by the configured QEMU account.

On Omarchy, the packaged polkit rule grants members of the `libvirt` group
management access without repeated admin dialogs. Have an administrator add
your user to that group, then start a new login session or use
`newgrp libvirt -c 'pulumi preview --diff'` to pick up the new group immediately.
Read-only virsh commands above do not require management authorization.

If the host firewall defaults to dropping incoming and forwarded traffic,
allow DHCP requests on the test bridge (UDP 67), DNS from the test subnet to
its gateway (TCP/UDP 53), and forwarded traffic for subnet egress and peers.
Scope rules to the actual bridge/subnet, found with `virsh net-info` and
`virsh net-dumpxml`; keep the host firewall enabled. Without these rules,
the domains can be running while first-boot DHCP or downloads are blocked.

## Prepare the project

Run from `flatcar-crdb-k3s/infra/qemu`:

```bash
mkdir -m 700 state
pulumi login --local
pulumi stack init local
pulumi install
```

Choose a strong state-encryption passphrase when prompted. Login explicitly
before installing packages so the CLI uses your chosen backend. Pulumi Cloud
is optional; an existing object-storage backend can also be used.

`Pulumi.yaml` pins the Terraform-provider bridge to **1.5.0** and libvirt to
**0.9.9**. `pulumi install` generates the current Go SDK under ignored `sdks/`
and installs dependencies. The generated SDK is required before `go test` or
`go build` on a fresh checkout. No Terraform/OpenTofu CLI is needed. The Go
module uses a local `replace` directive for that SDK.

Merge settings from `Pulumi.stack.example.yaml` into the newly created
`Pulumi.local.yaml`, preserving its generated `encryptionsalt`. Set the actual
QEMU service UID/GID and choose an unused stack name, pool directory, and subnet.
Example defaults use `192.168.124.0/24`, with gateway `192.168.124.1`.

Copy the shared configuration:

```bash
cp ../../cluster.json cluster.local.json
```

Edit `cluster.local.json` with your SSH public key and private node IPs in the
chosen subnet. For example, the `nodes` array can be:

```json
[
  {"name": "kube1", "ip": "192.168.124.11"},
  {"name": "kube2", "ip": "192.168.124.12"},
  {"name": "kube3", "ip": "192.168.124.13"},
  {"name": "worker1", "ip": "192.168.124.14", "role": "worker"}
]
```

Do not set `cluster_mac`: this target supplies one virtio NIC and a DHCP
reservation matching each configured IP. MACs are derived from the stack name
and node name, so inventory reordering does not change them. The subnet's
network address, gateway, and broadcast address cannot be assigned to nodes.

For a smaller first experiment, `single_node_lab: true` with one server is
supported. It must have exactly one CockroachDB host. Any workers added in that
mode must explicitly set `cockroachdb: false`, otherwise they would each start
an independent single-node database. Use the three-server configuration for a
shared multi-node database.

Download and verify a pinned Flatcar image, then generate fresh private PKI and
Ignition:

```bash
python3 prepare-image.py --version 4757.2.2 --state-dir state
python3 ../../generate.py pki --config cluster.local.json --state-dir state
python3 ../../generate.py ignition --config cluster.local.json --state-dir state
```

The image helper verifies the archive's SHA512 against the vendor's HTTPS
`.DIGESTS` file, decompresses it, checks qcow2 magic, and makes the base read-only.
Repeated runs verify an existing identical image; a differing destination is
never overwritten. This checks download integrity, not a separate GPG signature.
If you choose another exact Flatcar version, update `baseImagePath` accordingly.
An already verified, uncompressed Flatcar QEMU image can also be used directly.

The PKI command refuses to overwrite existing certificates. Keep CA private
keys and root admin credentials on the workstation, outside the VM storage pool.

## Review and create

```bash
go test ./...
python3 -m unittest discover -v
pulumi preview --refresh --diff --save-plan=state/deployment-plan.json
```

Preview validates configuration and checks current resource state; it does not
create VMs. Only the following step provisions the network, pool, volumes, and
domains, and starts VMs when `running` is true:

```bash
pulumi up --plan=state/deployment-plan.json
pulumi stack output domainNames
pulumi stack output sshCommands
```

The host can connect directly to guest IPs on the libvirt NAT bridge; no public
IPs or SSH/API port forwarding are used. NAT and libvirt DNS provide outbound
access for the first-boot K3s sysext download and container pulls. Other LAN
machines do not automatically have a route into this network.

Inspect a node using its configured IP or exported domain name:

```bash
ssh core@192.168.124.11 'sudo systemctl status cockroachdb k3s'
ssh core@192.168.124.14 'sudo systemctl status cockroachdb k3s-agent'
virsh -c qemu:///system console flatcar-local-kube1
```

Exit a virsh console with Ctrl+]. Hosts using SELinux/AppArmor may need a scoped
allow rule for QEMU to read the Ignition volumes in the pool directory; follow
the [Flatcar libvirt guidance](https://www.flatcar.org/docs/latest/deploy/virt-options/libvirt/)
for the actual path. The program does not disable host confinement.

## Complete Kubernetes bootstrap

For a brand-new three-node CockroachDB cluster, initialize it exactly once and
apply `../../bootstrap.sql` using `state/pki/admin` client credentials. Use the
same Docker CLI steps as the [Azure initialization guide](../azure/README.md#initialize-a-brand-new-cluster),
but connect directly to the first database node's private IP on port 26257
instead of creating an SSH tunnel. The certificates include that private IP.
Do not run `cockroach init` in `single_node_lab` mode: `start-single-node`
initializes itself, so only the bootstrap SQL is needed in that mode.

Once the API is ready, obtain a private kubeconfig and replace its loopback
endpoint with the reachable server IP:

```bash
umask 077
ssh core@192.168.124.11 'sudo cat /etc/rancher/k3s/k3s.yaml' > state/kubeconfig
sed -i 's/127.0.0.1:6443/192.168.124.11:6443/' state/kubeconfig
export KUBECONFIG="$PWD/state/kubeconfig"
kubectl get --raw=/readyz
kubectl get nodes -o wide
```

Use your actual server IP in these examples. Install Cilium with the
[existing lab settings](../../QEMU-LAB.md): kube-proxy enabled, pod CIDR
`10.42.0.0/16`, CNI binaries `/opt/cni/bin`, and config `/etc/cni/net.d`.
Then wait for Ready nodes and exercise pod/DNS/service connectivity.

## Persistence and current limits

All VMs share an immutable base and have independent persistent qcow2 overlays.
CockroachDB data is on each overlay's `/var/lib/cockroach`, outside Kubernetes.
Domains, storage pool, network, and volumes are protected against deletion or
replacement through Pulumi. Disabling protection and destroying disk volumes
would delete their data; use a deliberate backup/teardown procedure.

Volume inputs are treated as immutable after creation. This avoids the pinned
provider's unsupported in-place volume updates and preserves running nodes when
Ignition is regenerated. Changing image, disk size, certificates, roles, or
service settings needs an explicit update/replacement procedure. Ignition runs
only on first boot; editing the `.ign` file does not reconfigure a running node.

**The pinned provider replaces networks when their DHCP inventory changes.**
Network protection blocks that operation on an existing stack. Choose the full
initial node inventory before the first apply; live expansion needs a deliberate
network update workflow that is not implemented yet. CPU/memory and running-state
changes also require reviewing the provider's update plan.

Ignition is uploaded directly from private generator output into `0600` libvirt
volumes. The provider's separate Ignition helper is deliberately unused because
it writes `0644` temporary files. Pulumi inputs contain file paths, not the
embedded private keys, but local files, volumes, disks, backups, and state must
still be kept private. The pool and VMs use the host's normal QEMU service account.

## Verification

Tests use Pulumi mocks and temporary fake credentials, calling the actual Python
generator. They cover node/DHCP identity, NAT, per-node disks and Ignition,
Flatcar `fw_cfg` with ACPI, credential-volume permissions, protected lifecycle,
worker database defaults, stale profiles, subnet validation, and the single-node
database guard. The image helper's checksum and overwrite behavior is also tested.

The provider SDK has been generated and the Go program compiled and tested.

On 2026-10-25, a native Omarchy smoke deployment used Pulumi 3.272.0, libvirt
12.8.0, QEMU 11.1.2, and Flatcar 4757.2.2. Preview and apply succeeded, creating
14 resources including three running VMs, each with 4 GiB RAM, two vCPUs, and a
40 GiB overlay, on the `192.168.124.0/24` NAT network. Real provider validation
required explicit empty PTY paths for libvirt to allocate the serial consoles;
the program now supplies those values and ignores subsequent runtime PTY-path
changes. A final preview with refresh reported **14 unchanged resources**.

The fresh three-node integration smoke test passed:

- All three DHCP reservations were leased; SSH and outbound HTTPS worked.
- CockroachDB 26.2.7 was initialized once with TLS, then the Kine bootstrap SQL
  was applied. All three database nodes reported live, with no unavailable or
  under-replicated ranges at the end of bootstrap.
- All three K3s 1.35.8+k3s1 APIs passed `/readyz`, and all nodes became Ready.
- Cilium 1.20.4 and Envoy were ready on 3/3 nodes; both operator replicas were
  ready. The effective `kube-proxy-replacement` setting remained `false`.
- All six directed node-pair jobs passed Kubernetes DNS and cross-node service
  HTTP checks. CoreDNS, metrics-server, and local-path provisioner were healthy.
- The Go mock tests and image-helper tests passed after the console fixes.

Host UFW initially blocked first-boot networking. Rules scoped to `virbr1` and
`192.168.124.0/24` enabled DHCP, DNS, peer forwarding, and egress via `enp9s0`.
The firewall stayed enabled. Two VMs were rebooted during this early bootstrap;
post-bootstrap reboot recovery, failover, replacement recovery, and durability
under faults were not exercised in this test.

For the running smoke stack, the private admin kubeconfig is
`state/kubeconfig` (mode `0600`). Its endpoint is `https://192.168.124.11:6443`.
The fresh VM SSH key is `state/id_ed25519`. The helper `state/run.py` selects
the private local Pulumi backend and uses the libvirt group for management.

References: [current Pulumi libvirt package](https://www.pulumi.com/registry/packages/libvirt/),
[pinned libvirt provider](https://github.com/dmacvicar/terraform-provider-libvirt/tree/v0.9.9),
[Pulumi package generation](https://www.pulumi.com/registry/packages/terraform-provider/),
[Flatcar libvirt boot configuration](https://www.flatcar.org/docs/latest/deploy/virt-options/libvirt/).
