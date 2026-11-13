# Local provider state

Each `infestation <provider> create --name NAME` saves private state for the
machines that call owns. It does not enumerate the whole tailnet's cluster.
With `--size 2`, one provider-state directory describes both owned nodes.

The default location is `$XDG_DATA_HOME/infestation/vms/NAME`, or
`~/.local/share/infestation/vms/NAME`. With `--state-dir DIR`, it is `DIR/NAME`.
Use the same name/state directory for subsequent lifecycle commands.

## Where lifecycle commands run

Every public command has `--help`, for example `infestation vm start --help` or
`infestation azure stop --help`. Help describes the state/host requirements.

- Local `start`, `stop`, `remove` and `status` run on the original QEMU host:
  they need its saved state, local disks and QMP control sockets. Copying metadata
  alone does not let another machine operate those local VMs.
- Azure `start`, `stop`, `remove` and `status` need saved provider state plus an
  Azure SDK credentials (`DefaultAzureCredential`). They can run from another machine if that state is
  available there; the current CLI does not discover resources from the name or
  ownership tags alone. Tailnet membership does not grant Azure compute control.
- `wait` and `kubeconfig` need the saved node inventory and tailnet connectivity
  from the caller. They can run elsewhere with the inventory; Azure login and
  QEMU host control are not needed for these operations. `wait` also uses kubectl.

Keep private material protected when moving any state. No state needs sharing
merely to enroll a friend's node. Azure `start --wait` currently ignores the
wait flag; use `azure wait` separately to check cluster readiness.

## Saved files

| File or directory | Contents and later use |
| --- | --- |
| `nodes.json` | Owned node names, roles, tailnet, pinned versions and shared settings. Lifecycle commands identify and validate their targets from this file. |
| `provider.json` | Provider ownership, so a local command cannot operate Azure resources. |
| `output/*.ign` | Original first-boot configuration, retained for inspection and diagnosis. Profiles contain the enrollment key and, for the initializer, leaf/join material and the short-lived SQL root credential. Keep them private even after guest copies are removed. |
| `<node>/disk.qcow2` | Local VM's persistent disk, including its database data, Tailscale identity and node-local bootstrap state. Start reuses it; stop preserves it. |
| `<node>/machine.json`, `<node>/config.ign` | Saved QEMU launch settings and the VM's original first-boot profile. |
| `<node>/qmp.sock`, PID/lock files | Local process control used by start, stop and status. These are not backups. |
| `<node>/*.log`, diagnostic JSON and access reports | Boot/network troubleshooting and private VM-to-host reports. Access reports may contain Kubernetes admin credentials. |
| `azure-scaleset.json` | Subscription, resource group, VMSS name, region, initial capacity and ownership identity. Start/stop/status target the entire owned set. |
| `azure-request.json` | Private typed Azure SDK objects saved before provisioning, including Ignition customData. |
| `azure-progress.json` | Current/last provisioning stage, retained when an operation fails or is canceled. |
| `azure-resources.json` | Recorded resource inventory for diagnosis. |
| `pki/` (initializer only) | Workstation CA signing key, SQL admin credentials, node/client leaves and K3s token. Only permitted leaves and a separate one-hour SQL bootstrap credential enter the selected VM. Signing/admin keys remain here. |
| `kubeconfig` | Admin kubeconfig retrieved by `wait` or `kubeconfig`, mode 0600. It can be retrieved again from an enrolled node. |

New groups use `nodes.json`. Existing directories with `cluster.json` remain
readable without an automatic rename. If both exist, `nodes.json` takes
precedence; invalid new state is an error, not permission to use older state.
Static test fixtures elsewhere in the repository have separate lifecycles.

Running nodes do not require this workstation to remain online. Joining nodes
fetch join material from enrolled peers over Tailscale; participants do not copy
workstation state between themselves. The running database's UUID/init marker
and pinned enrollment bundle are stored inside the VM, not solely in `nodes.json`.

Preserve the directory. Losing local QEMU disks loses those nodes' stored data;
losing Azure bookkeeping does not stop the Azure VM, but these lifecycle commands
currently need that bookkeeping to manage it. Losing initializer workstation PKI
also loses the signing/admin material needed for deliberate credential recovery.
Node data and provisioning files are not a coordinated database backup.

Creation refuses existing provider state. Partial provisioning repair is not
supported. A failed create preserves state/resources for inspection; cleanup
and a fresh create must be explicit and preserve any data or identity needed
for recovery. Expired bootstrap credentials require deliberate recovery.
Keep state ignored by Git and private;
never print profiles, kubeconfigs or keys into logs or support messages.

## Remove owned compute

```sh
infestation vm remove --name lab --state-dir state/vms
infestation vm remove --name lab --state-dir state/vms --delete-disks
infestation azure remove --name cloud --state-dir state/vms
```

QEMU removal terminates each owned emulator through its private QMP monitor,
waits for the verified process to exit, and removes its socket and PID file.
This is a forced guest power-off; use `stop` first when a graceful shutdown is
needed. Disks are preserved by default and `start` can reuse them. The explicit
`--delete-disks` option permanently removes only each owned `disk.qcow2` overlay,
including its database data and enrolled identity. Shared base images, profiles,
reports, credentials and inventory remain. An unavailable monitor with a live
process, mismatched identity, or an active start lock prevents cleanup.
[QEMU documents `quit`](https://www.qemu.org/docs/master/interop/qemu-qmp-ref.html#command-quit)
as terminating the emulator.

Azure removal verifies the saved provider/VMSS identity and the resource group's
`infestation-mixed-id` tag, then deletes the **entire dedicated resource group**.
This includes the VMSS, its disks, network resources and anything subsequently
added to that group. It waits for the Azure SDK deletion poller;
[SDK long-running operations](https://learn.microsoft.com/en-us/azure/developer/go/control-plane#long-running-operations)
describe this pattern. This is permanent data deletion; use `stop` when disks
must survive. Missing groups are already removed. Ownership failures and deletion
errors retain all local evidence. Destructive removal requires current
`provider.json` and `azure-scaleset.json`; older mixed/single-VM records are not
accepted as authority to delete a resource group.

Both commands retain the private provider directory, including initializer
CA/token material. Removal can be retried after partial failure. A fresh create
still refuses that existing directory: preserve it for recovery and use a new
name, or deliberately archive it before reusing the name. Neither removal drains
CockroachDB/Kubernetes members nor deregisters tailnet devices, routes or Services.
These commands are compute teardown, not a cluster member replacement protocol.

### Local validation

Removal is covered by `go test ./...` and `go vet ./...`. QEMU tests simulate a
QMP monitor and a disposable local process; they cover exit before disk deletion,
stale control files, repeated removal, interrupted creation, identity/provider
mismatches, reused PIDs and start locks. Azure tests use a mocked CLI runner to
check subscription/group scope, ownership rejection, absent groups and deletion
failure with retained state. These checks do not provision or delete real QEMU
VMs or Azure resources; live teardown remains unverified.
