# Local QEMU/KVM through Pulumi

The root Go module creates a private NAT network, stable DHCP reservations,
QEMU-owned storage pool, immutable base image, private Ignition volumes, and
persistent copy-on-write VM disks through `qemu:///system`. Servers and workers
use the same shared Go configuration, PKI and Ignition packages as Azure.

The Pulumi project ID remains `flatcar-k3s-qemu` to preserve existing stack URNs.
There is no generated SDK or Python prerequisite: the Go adapter registers the
pinned terraform-provider bridge 1.5.0 with libvirt 0.9.9 directly. No Terraform
or OpenTofu executable is required. If automatic plugin downloads are disabled,
install the bridge with `pulumi plugin install resource terraform-provider 1.5.0`.

## Prepare

Requires x86-64 Linux/KVM, libvirt, QEMU, dnsmasq and management permissions.
Build from the repository root with `make build`. Check current resources first:

```sh
virsh --readonly -c qemu:///system list --all
virsh --readonly -c qemu:///system net-list --all
virsh --readonly -c qemu:///system pool-list --all
```

Find the account that actually runs QEMU (`libvirt-qemu` or `qemu`) and its
numeric UID/GID. The pool uses mode `0700`, volumes `0600`, owned by that account.
Do not use your own UID. On this host, `libvirt-qemu` is `954:954`.

Members of the host's `libvirt` group can manage libvirt without repeated polkit
prompts. A new login picks up group changes; `newgrp libvirt -c 'COMMAND'` can be
used immediately. When remoted in, run privileged host setup through terminal
sudo rather than relying on graphical authorization dialogs.

If the firewall drops incoming/forwarded traffic, permit DHCP on the actual
bridge, DNS from the test subnet to its gateway, and forwarding for egress and
peers. Keep rules scoped to the chosen bridge/subnet/uplink. The historical
[runbook](RUNBOOK.md) records the UFW rules used on this host; never disable the
whole firewall to make a lab work.

Use a fresh private state directory and an inventory with unused guest IPs, for
example `192.168.124.11`, `.12`, `.13`. Do not set `cluster_mac`; this provider
assigns stable MACs from stack/node names and matching DHCP reservations.

```sh
mkdir -p state/qemu
chmod 700 state state/qemu
cp examples/cluster.json state/qemu/cluster.json
# Set the SSH key and guest IPs in state/qemu/cluster.json.
bin/infestation pki --config state/qemu/cluster.json --state-dir state/qemu
bin/infestation ignition --config state/qemu/cluster.json --state-dir state/qemu
bin/infestation image --version 4757.2.2 --state-dir state/qemu
```

The image command verifies SHA512 against the vendor HTTPS DIGESTS, checks qcow2,
and publishes a read-only base. Repeated runs accept an identical image and
refuse to replace a different existing disk. This is download integrity checking,
not a separate GPG-signature verification.

## Configure and apply

Run from the root; choose a private backend/passphrase and a fresh stack name:

```sh
pulumi --cwd infra/qemu login --local
pulumi --cwd infra/qemu stack init local
```

Merge [Pulumi.stack.example.yaml](Pulumi.stack.example.yaml) into the generated
`infra/qemu/Pulumi.local.yaml`, preserving `encryptionsalt`. Set the QEMU UID/GID,
dedicated pool path, VM sizing and subnet. Prefer absolute input paths:

```sh
pulumi --cwd infra/qemu config set clusterPath "$PWD/state/qemu/cluster.json"
pulumi --cwd infra/qemu config set ignitionDir "$PWD/state/qemu/output"
pulumi --cwd infra/qemu config set baseImagePath "$PWD/state/qemu/flatcar-4757.2.2.qcow2"
pulumi --cwd infra/qemu preview --refresh --diff
pulumi --cwd infra/qemu up --refresh
```

The host can reach the guests directly; other LAN clients need a route or tunnel.
NAT/DNS support extension downloads and container pulls. All VMs share an
immutable base and have independent persistent overlays. Cockroach stores data
under `/var/lib/cockroach` on those overlays. The Azure temporary-disk profile is
explicitly rejected here because QEMU does not provide Azure's resource disk.

## Complete bootstrap

With a private SSH key matching the inventory:

```sh
bin/infestation bootstrap --config state/qemu/cluster.json --state-dir state/qemu \
  --ssh-private-key /PATH/TO/KEY --initialize-new-cluster
export KUBECONFIG="$PWD/state/qemu/kubeconfig"
kubectl get nodes
bin/infestation smoke --config state/qemu/cluster.json --state-dir state/qemu
```

The native Go bootstrap handles SSH/SQL forwarding, guarded init, Kine SQL,
Cilium and admin kubeconfig. For an already initialized cluster, omit the init
flag. For `single_node_lab`, Cockroach initializes itself; do not request init.
Workers in that mode must opt out of Cockroach so they do not form independent
databases. A public API load balancer is not required when the host can directly
reach the first server; `--endpoint` can override the kubeconfig address.

For retrieval alone, `infestation kubeconfig` accepts the same config/state/key
flags. It preserves CA/client authentication and writes a mode `0600` kubeconfig.
The API endpoint here targets one server; API failover needs another endpoint.

## Lifecycle and verification

Pool, network, volumes and domains are protected against deletion/replacement.
Volume inputs are immutable after creation. Changing Ignition does not alter a
running VM: first-boot data applies once, and live updates need another procedure.
The pinned provider replaces networks when DHCP inventory changes, so choose
the full initial inventory; expansion needs a reviewed network update workflow.
Runtime console PTY paths are ignored to avoid spurious domain updates.

To stop gracefully, request `virsh shutdown` for the desired domains, wait for
`shut off`, set the stack's `running` config to `false`, and refresh state. To
resume, set `running` to `true`, review a refreshed preview, then apply. Do not
regenerate PKI or initialize the database when resuming existing disks.

The earlier three-node smoke test passed actual DHCP/SSH/egress, TLS database
initialization, three Ready nodes, Cilium and all six directed DNS/service checks.
After the root Go migration, a real refreshed preview still reported **14
unchanged resources**, with the stopped VMs/disks preserved. The new native PKI
and bootstrap have unit/mock coverage but have not been boot-tested on fresh VMs.

See [RUNBOOK.md](RUNBOOK.md) for historical host setup and
[migration notes](../../docs/migration.md) for state paths.
