# A Linux VM on your friend's tailnet

This creates a **standalone Flatcar VM enrolled in Tailscale**. It does not yet
join Kubernetes or run CockroachDB. It is the first test of the egress-only node
setup in [the networking plan](networking-plan.md).

Use Linux x86-64 with hardware virtualization enabled and access to `/dev/kvm`.
Install QEMU once (Ubuntu/Debian):

```sh
sudo apt-get install qemu-system-x86 qemu-utils
```

Install the binary from this checkout with Go 1.26.6 or newer:

```sh
go install ./cmd/infestation
export PATH="$(go env GOPATH)/bin:$PATH"
```

With the supplied key in `TAILSCALE_AUTH_KEY`, create your VM:

```sh
infestation vm create --name alice-node
```

Choose a distinct name per person/VM. The command downloads pinned Flatcar
4757.2.2, verifies its archive against the vendor SHA512 digest, creates a private
persistent disk, boots the VM, installs Tailscale v1.104.2 through Flatcar's
community sysext, and waits up to ten minutes for a running, tagged identity.
On success it prints the VM's tailnet name and IPs. No inventory, static IP,
SSH, libvirt, root VM launcher, LAN bridge or inbound port forwarding is needed.
Each VM gets its own QEMU user-mode NAT. The laptop does not need Tailscale.

Alternatively, pass `--tailscale-auth-key "$KEY"` or
`--tailscale-auth-key-file /private/path/key`. Explicit arguments/files take
precedence over `TAILSCALE_AUTH_KEY`. Literal secrets in command lines may remain
in shell history or process listings. Do not paste real keys into Git issues.

For the person supplying the key: create a short-lived reusable auth key, assign
the intended VM tag, and preauthorize it if device approval is enabled. Grant
that tag only the connectivity intended for these VMs. Subnet routing and
Kubernetes policy are not configured by this enrollment-only command. See
[Tailscale server enrollment](https://tailscale.com/docs/how-to/set-up-servers).

Manage the VM with the same installed binary:

```sh
infestation vm status --name alice-node
infestation vm netcheck --name alice-node
infestation vm stop --name alice-node
# After graceful shutdown completes:
infestation vm start --name alice-node
```

`start` reuses the disk and enrolled Tailscale identity; it needs no auth-key
argument. Creation refuses an existing name. `stop` requests graceful shutdown
and preserves storage. There is intentionally no destructive delete command.
`status` checks QEMU and displays the latest guest enrollment report; it does
not establish current connectivity from another tailnet peer. Verify peer access
with `tailscale ping --until-direct=false --c 3 alice-node` from a tailnet client.
That checks Tailscale peer discovery and reports its route; application traffic
and OS firewall behavior need their own checks.

New VM profiles run the real guest `tailscale netcheck --format=json` after
enrollment and every five minutes. `vm netcheck` displays the latest complete
guest report over a private virtio channel, including its `Now` timestamp. It
does not require SSH and does not substitute the host's netcheck results.

SLIRP is the default backend. To try passt, install your distribution's `passt`
package and add `--network passt` to `vm create`. Passt still runs without root;
Ubuntu's package also supplies the AppArmor profile needed for its namespace
sandbox. Both backends disable inbound port forwarding. `vm start --network
slirp` or `--network passt` overrides the stored backend for that start only,
allowing a comparison on the same enrolled disk. Older profiles created before
guest diagnostics were added do not have the netcheck reporter.

State defaults to `$XDG_DATA_HOME/infestation/vms`, or
`~/.local/share/infestation/vms`. Override it with `--state-dir` on every command.
State directories are private and contain the disk, Ignition, console log and
QEMU monitor. The guest deletes its enrollment key file after enrollment;
generated Ignition retains a copy. Protect these files as credentials.

If KVM is unavailable, use `--accel tcg` when creating the VM. This uses slow
software emulation. `--wait 0` starts in the background; a nonzero timeout leaves
the VM running for inspection. Do not create a replacement VM simply because
enrollment timed out. Check the private `console.log` and `vm status` first.
An existing QMP socket prevents duplicate starts. If a host crash leaves a stale
socket or `start.lock`, verify that its VM process has exited before removing it.

`--base-image PATH` skips the automatic download and trusts an existing Flatcar
qcow2. This is intended for local tests with a previously verified image. It
creates an overlay and never boots/writes that base directly.

## Verification

Unit tests check private key rendering, argument/environment/file precedence,
identity validation, partial status reports, and refusal to overwrite existing VM
state. The live boot/enrollment test record is in the networking checkpoint.
Kubernetes joining, Cockroach quorum over Tailscale, public ingress, and latency
measurement between enrolled nodes remain separate integration tests.

To run the opt-in live enrollment and restart test from a checkout, with a real
key already exported:

```sh
INFESTATION_VM_TEST=1 go test ./internal/vm -run TestLiveEnrollmentAndRestart -count=1 -v -timeout 25m
```

It creates a real tagged tailnet device and verifies that a graceful VM restart
preserves its node ID, DNS name, IPs and disk. It requests shutdown at the end
and retains private VM artifacts for inspection. It does not delete the
tailnet device. Set `INFESTATION_VM_ACCEL=tcg` for software emulation or
`INFESTATION_VM_BASE_IMAGE=/trusted/flatcar.qcow2` to use a previously verified
base image. Normal `go test ./...` skips provisioning.
