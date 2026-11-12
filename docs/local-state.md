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

- Local `start`, `stop` and `status` run on the original QEMU host:
  they need its saved state, local disks and QMP control sockets. Copying metadata
  alone does not let another machine operate those local VMs.
- Azure `start`, `stop` and `status` need saved provider state plus an
  authenticated Azure CLI. They can run from another machine if that state is
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
| `azure-scaleset.json` | Subscription, resource group, VMSS name, region, initial capacity and ownership identity. Start/stop/status target the entire owned set. Earlier single-VM state remains readable without resource migration. |
| `azure-template.json`, `azure-parameters.json` | Saved Azure deployment request, retained for inspection and diagnosis. Parameters contain private Ignition. |
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
