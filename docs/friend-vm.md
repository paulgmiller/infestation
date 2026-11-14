# A Linux VM on your friend's tailnet

Create an independently enrolled cluster node using only the supplied tailnet
auth key/domain and a distinct name. No other person’s workstation state, CA
signing key, token file or global inventory is required. The flow has unit and
protocol tests; a fresh independent VM test is still pending.

```sh
infestation vm create --name alice --size 1 \
  --tailnet-domain YOUR-TAILNET.ts.net \
  --tailscale-auth-key-file /private/path/key
```

If the creator workstation is already connected to the intended tailnet, omit
`--tailnet-domain`: the CLI reads `CurrentTailnet.MagicDNSSuffix` from
`tailscale status --json` (with the legacy `MagicDNSSuffix` field as a fallback)
and prints the selected domain before profile preparation. It requires a running
client and a valid `<tailnet>.ts.net` suffix; it does not guess from peer names,
the tailnet display name or the enrollment key. If discovery is unavailable,
supply the flag explicitly. An explicit value takes precedence, allowing creation
from outside the tailnet or while connected to another one. Check that the auth
key belongs to the selected tailnet. See the
[Tailscale status definitions](https://github.com/tailscale/tailscale/blob/main/ipn/ipnstate/ipnstate.go).

Omit `--init` when joining. Exactly one founding group uses it; that group’s first
node waits for two database peers before initialization. The published `master`
branch must contain the enrollment protocol; override `--helper-source-ref` to
select another published revision. Nodes discover `tag:roach` participants,
fetch leaf/join credentials over the tailnet on helper port 7444, and start
CockroachDB and K3s. Every enrolled member can supply join material afterward.
The creator workstation does not need to be on the tailnet unless it requests
`--wait` readiness checks. See [bootstrap](bootstrap.md) for policy and recovery.

Use `vm status|stop|start --name alice` for this provider group.
`azure create --name alice-cloud --size 1` accepts the same enrollment details;
Azure creates a VMSS with capacity from `--size`; region/SKU default to
`westus2`/`Standard_B2s`. See [VMSS behavior](azure-vmss.md). Each command creates compute
on only one provider. Each machine retains its own private provider state.

The person supplying the key first applies the [tailnet setup](tailnet-policy.md),
including tagged-peer traffic, SSH and route/Service auto-approvals.

## Install and manage

First follow the [local VM prerequisites](local-vm-prerequisites.md) for
Ubuntu/Debian or Arch Linux package installation, KVM kernel support and
`/dev/kvm` permissions. Then install the CLI from this repository:

```sh
go install ./cmd/infestation
```

With the updated CLI on PATH, use the creation command above. `--size` defaults
to one. Add `--agent` if this machine should run CockroachDB and a K3s agent
without hosting the API server. An agent cannot use `--init`.

Manage only the nodes owned by this provider group:

```sh
infestation vm status --name alice
infestation vm status --name alice --netcheck
infestation vm stop --name alice
infestation vm start --name alice
infestation vm wait --name alice
infestation vm kubeconfig --name alice --out kubeconfig
```

Azure uses the same verbs; `status --netcheck` is specific to the local guest
channel. Start/stop preserve VM disks and identity. Failed creation retains
state/resources for inspection; partial provisioning repair is not supported. `kubeconfig`
requires workstation tailnet access and refuses an existing output unless
`--force` is supplied. API TLS/client authentication remain enabled.

State defaults to `$XDG_DATA_HOME/infestation/vms`, or
`~/.local/share/infestation/vms`. Set `--state-dir` consistently when overriding it.
Keep provider state private: it contains disks/profiles and, for an initializer,
workstation PKI. No directory needs copying to another participant.

## Proxmox

To manage the VM yourself in Proxmox, use Infestation only to export its disk and
enrollment profile. Install `qemu-img` (`qemu-utils` on Debian/Ubuntu); KVM access
and a local QEMU daemon are not required for export. Supply a fresh Tailscale
auth key carrying `tag:roach` and a unique node name:

```sh
infestation proxmox export --name alice-proxmox-0 \
  --out-dir state/exports/alice-proxmox-0 \
  --tailnet-domain YOUR-TAILNET.ts.net \
  --host alice-proxmox --site alice-home \
  --tailscale-auth-key-file /private/path/key
```

`TAILSCALE_AUTH_KEY` can supply the key instead of a file. The command downloads
and verifies the pinned Flatcar QEMU base, converts it to a standalone dynamic
**VHD** (`qemu-img` format `vpc`), and renders a joining node's private Ignition
profile. `--base-image` can reuse a trusted never-booted standalone QEMU qcow2.
The export refuses existing output directories and never boots or provisions a
VM. `--agent` selects K3s agent plus CockroachDB; this export cannot initialize a
database. Helper source defaults to published `master`, resolved to a commit.

The output contains only `flatcar.vhd`, `node.ign`, `image.sha256` and
`manifest.json`. The Tailscale auth key is in **`node.ign`**, supplied separately
to first boot; it is not baked into the VHD. No Python or shell helper scripts
are generated. Keep the directory private and transfer both files securely to
the Proxmox host. Repository `state/` and disk/profile export patterns are ignored;
do not commit or publish generated exports. Generate a new profile/name for each
VM rather than cloning an enrolled disk.

On the Proxmox host, place the files at root-only paths, for example
`/root/alice-proxmox-0/flatcar.vhd` and `/root/alice-proxmox-0/node.ign`.
Choose an unused VMID (123 below), disk storage (`local-lvm`) and a bridge
(`vmbr0`) providing DHCP and outbound connectivity. Run these individual Proxmox
commands manually, substituting those values:

```sh
qm create 123 --name alice-proxmox-0 --arch x86_64 --ostype l26 \
  --machine q35 --bios seabios --cores 2 --cpu host --memory 4096 \
  --net0 virtio,bridge=vmbr0 --serial0 socket --vga serial0 --protection 1
qm disk import 123 /root/alice-proxmox-0/flatcar.vhd local-lvm
```

The imported disk appears as an **Unused Disk** in the VM's Hardware tab. Attach
it using the VirtIO Block bus (`virtio0`), increase it to at least 20 GiB, and set
it first in Options → Boot Order. Set the first-boot Ignition file before starting:

```sh
qm set 123 --args '-fw_cfg name=opt/org.flatcar-linux/config,file=/root/alice-proxmox-0/node.ign'
qm start 123
```

The `--args` command replaces any existing custom QEMU arguments; use a newly
created VM as above. Keep its absolute path free of spaces and commas. Preserve
the private profile on the host and copy it securely before migrating the VM to
another host. This uses Flatcar's generic QEMU image and fw_cfg, so no cloud-init
disk or `--cicustom` setting is needed. Two cores and 4 GiB are starting settings,
not a database performance guarantee. Follow
[Flatcar's QEMU Ignition guidance](https://www.flatcar.org/docs/latest/deploy/virt-options/qemu/),
[QEMU's VHD format reference](https://www.qemu.org/docs/master/system/qemu-block-drivers.html#vpc)
and [Proxmox's import/VM command reference](https://github.com/proxmox/pve-docs/blob/master/generated/qm.1-synopsis.adoc).

On first boot the node joins Tailscale and obtains cluster credentials from
existing `tag:roach` peers. Apply the [tailnet policy](tailnet-policy.md) first;
finish any old-cluster teardown before starting against a replacement cluster.
Use `tailscale ssh core@alice-proxmox-0` for access. Infestation does not register
ownership or manage this Proxmox VM's start/stop/removal. Protection helps prevent
accidental deletion, but an administrator can disable it. Decommission the
database and verify replica evacuation before deleting a contributing VM;
offline timeouts alone cannot restore a lost voting majority. This manual
Proxmox boot/enrollment path still needs live verification.
