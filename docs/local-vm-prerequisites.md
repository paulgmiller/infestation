# Local VM prerequisites

`infestation vm` currently requires a Linux x86-64 host. It runs QEMU directly
as your normal user, using SLIRP networking. Libvirt, virt-manager, a bridge and
a running libvirtd service are not required for these CLI commands.

## Install QEMU

Ubuntu/Debian:

```sh
sudo apt-get update
sudo apt-get install qemu-system-x86 qemu-utils
```

Arch Linux (including Omarchy):

```sh
sudo pacman -Syu qemu-base
```

These supply `qemu-system-x86_64` and `qemu-img`. See
[Ubuntu's QEMU guide](https://ubuntu.com/server/docs/how-to/virtualisation/qemu/)
and the [Arch qemu-base package](https://archlinux.org/packages/extra/x86_64/qemu-base/).
Build the CLI as described in the [README](../README.md#build-and-check), then
check the tools without creating a VM:

```sh
qemu-system-x86_64 --version
qemu-img --version
```

## Enable hardware virtualization

The default accelerator is KVM. QEMU packages alone are insufficient: the
running kernel must provide KVM and your user must be able to open `/dev/kvm`
for reading and writing. KVM support comes from the host kernel.

```sh
lscpu
ls -l /dev/kvm
id -nG
test -r /dev/kvm && test -w /dev/kvm && echo 'KVM device accessible'
```

If creation reports `KVM unavailable`, use the checks above to distinguish:

- **Missing `/dev/kvm`:** enable Intel VT-x (sometimes called Intel Virtualization
  Technology) or AMD-V/SVM in BIOS/UEFI. Load the module matching your CPU:
  `sudo modprobe kvm_intel` for Intel or `sudo modprobe kvm_amd` for AMD.
  If loading fails, check `sudo dmesg` for KVM errors. A missing module can mean
  the installed kernel/modules do not match the running kernel; after a kernel
  upgrade, reboot into the installed kernel and check again.
- **Device exists but access check fails:** inspect its group with
  `ls -l /dev/kvm`. If it is owned by group `kvm`, add your normal user:

  ```sh
  sudo usermod -aG kvm "$(id -un)"
  ```

  Log out of the desktop/session completely and log back in, then repeat the
  access check. Opening another terminal in the same desktop session does not
  refresh its supplementary groups. Some desktops grant access through session
  ACLs, so group membership is only needed when the access check fails. Run
  Infestation as your normal user after fixing access.
- **Host is itself a VM:** its outer hypervisor must expose virtualization
  extensions and support nested virtualization. Installing packages inside the
  guest cannot enable this on the outer host. See the
  [Linux kernel nested KVM guide](https://docs.kernel.org/virt/kvm/x86/running-nested-guests.html).
  Containers likewise need the host to provide and permit access to `/dev/kvm`.

The access check matches the permission requirement; it does not prove KVM can
successfully start a guest. Retry creation after resolving the host issue.

## Software emulation fallback

If KVM is unavailable, append `--accel tcg` to your creation command. This avoids
the `/dev/kvm` requirement but still needs QEMU and Linux x86-64. Software
emulation is much slower, especially while building the node helper and starting
CockroachDB/K3s; prefer KVM for ordinary use.

## Before creating nodes

Apply the [tailnet policy](tailnet-policy.md) and obtain an enrollment key as
described in [independent enrollment](friend-vm.md). The workstation needs
internet access to resolve the helper source and download the Flatcar image;
guests need outbound internet access for bootstrap. Workstation tailnet access
is needed for `wait` and kubeconfig retrieval. A single `--init` VM waits for two
database peers; use the [three-node runbook](isolated-vm-cluster.md) for a local
founding group.
