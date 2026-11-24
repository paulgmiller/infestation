# QEMU compute-host autostart

`vm create` enables autostart by default, under the account running QEMU on the
compute host. Use `--autostart=false` to create without changing user services or
enabling the new group's boot policy. This flag applies to QEMU, including
`vm create --ssh`; Azure has no host user-service option.

Before creating any VM disk, create writes and enables a systemd user service
for the selected host state root. Service setup errors stop creation before any
VM is created. Prepared private profiles/state remain available for inspection.
Create does not run the boot hook or restart existing guests. A failed setup may
leave the unit file behind; a subsequent create can update this managed file.
Units belonging to other software, including symlinked units, are retained and
cause setup to fail.

Create checks `loginctl show-user UID --property=Linger --value` on the compute
host. If lingering is disabled or cannot be confirmed, it warns with the command
to enable it and continues creation. It never runs sudo or changes lingering:

```sh
sudo loginctl enable-linger YOUR_UID
```

With lingering, the user service manager starts at boot without an interactive
login and survives logout. See [loginctl](https://www.freedesktop.org/software/systemd/man/latest/loginctl.html).
The binary, disks, backing images and state must be available before login;
home directories unlocked only during login cannot provide unattended startup.

The private `autostart.json` records both policy (`enabled`) and desired power
state (`resume`) for each owned machine.
Enabling policy on a running VM arms it for the next host boot. Enabling policy
on a stopped VM leaves it stopped; an explicit `vm start` arms it. Shutdown
initiated inside a guest does not change host policy. An intentional
`vm stop` disarms it before requesting powerdown, including when waiting for
shutdown later times out. Removal disarms retained disks before terminating
compute. A refused identity/ownership check does not change that intent.

On the actual Linux QEMU host, using the same account and state root as creation:

```sh
infestation vm autostart --name lab --state-dir /absolute/private/vms
infestation vm autostart --name lab --state-dir /absolute/private/vms --instance 1 --enable=false
```

These commands only save policy. They do not install services or start guests.
For SSH-backed groups, run them on the SSH compute host against its saved state;
the workstation's routing records are rejected. Each compute host has its own
boot hook and saved disks. No workstation CA, admin key or enrollment key is
needed at boot. Existing guest disk identity and enrollment are reused.

## User boot service

Create installs `infestation-autostart-HASH.service` in
`$XDG_CONFIG_HOME/systemd/user` (normally `~/.config/systemd/user`). The hash
identifies the absolute state root, so separate roots get separate services and
groups sharing a root use the same hook. It runs `systemctl --user daemon-reload`
and `systemctl --user enable UNIT`, with no `--now`. For SSH creates, setup and
the lingering check happen on the remote compute host under the SSH account.

The generated unit stores the absolute path to the running Infestation binary
and its current PATH. Keep that binary installed at the saved path and keep QEMU
tools available on that PATH. The account needs its usual `/dev/kvm` access for
KVM. Unit values are quoted for systemd; no shell or enrollment credentials are
involved.

For an existing group without a boot service, an equivalent manually installed
`~/.config/systemd/user/infestation-autostart.service` is:

```ini
[Unit]
Description=Resume saved Infestation QEMU guests

[Service]
Type=oneshot
Environment=PATH=/usr/local/bin:/usr/bin:/bin
ExecStart=/absolute/path/infestation vm autostart-run --state-dir /absolute/private/vms
RemainAfterExit=yes
KillMode=process
TimeoutStartSec=5min

[Install]
WantedBy=default.target
```

After replacing the example paths, enable this manual unit with
`systemctl --user daemon-reload` and
`systemctl --user enable infestation-autostart.service`. For automatically
installed units, create prints the actual name. Inspect a unit with
`systemctl --user status UNIT` and `journalctl --user -u UNIT`.
Starting the boot hook explicitly resumes armed guests; do that only when intended.

This is a startup hook, not a QEMU supervisor or a coordinated shutdown service.
`KillMode=process` prevents stopping/restarting the hook from killing its daemonized
guests. Systemd discourages this mode for ordinary supervised services because
children outlive the unit; QEMU lifecycle here remains with the Infestation CLI.
Use `vm stop` for intentional guest shutdown. Host reboot/power loss may interrupt
guest writes; autostart does not establish database recovery or quorum safety.
See systemd's [kill policy](https://www.freedesktop.org/software/systemd/man/latest/systemd.kill.html) and
[oneshot services](https://www.freedesktop.org/software/systemd/man/latest/systemd.service.html).

`vm autostart-run` scans the selected root for saved local QEMU ownership.
It skips Azure groups, SSH routing records, disabled policy and intentional
stops. It never provisions or recreates missing disks/profiles, and it refuses
symlinked VM directories or disk/profile files. Already running, verified QEMU
processes are left running. Launch failures are reported while other eligible
machines are still attempted. Intent is rechecked under the lifecycle lock so an
intervening stop/removal wins over earlier discovery. Kernel locks release on
process exit or reboot; the remaining `start.lock` file is harmless.

Disabling the service prevents future automatic boot runs but does not stop
guests or clear saved intent. Disable individual policy with `--enable=false`.
After boot, check `vm status --name lab --state-dir ...`, tailnet reachability,
database health and directed workload connectivity. Emulator launch alone is
not evidence of cluster recovery.

## Local verification

On 2026-11-22, `go test ./...` and `go vet ./...` passed with default-on create
integration and user-service setup. Read-only `systemd-analyze --user verify`
checks confirmed supported executable/path quoting and identified unsupported
quote/backslash characters in executable paths, which setup explicitly rejects.

The autostart tests use fake QEMU executables, saved disk sentinels, mock QMP
sockets and disposable test processes. They cover stopped/running policy,
private state, explicit-start rearming, stop cancellation, identity refusal,
removal disarming, stale and active locks, selection across provider groups,
per-machine failures and rechecking intent at launch. User-service tests use a
fake runner and fake executables to verify default-on creation and opt-out,
host/SSH policy propagation, lingering warnings, setup failure before disk
creation, unit ownership and path quoting. They do not boot Flatcar,
install a host service, reboot a host or prove restored tailnet/database health.

Live verification remains outstanding: explicitly authorize a test host reboot,
arm one running VM and intentionally stop another, record disk and enrolled
identities, reboot, then verify that only the armed guest resumes and retains its
identity. Check tailnet connectivity, database health and workload DNS/HTTP before
claiming successful recovery. Keep all saved state and disks for diagnosis.
