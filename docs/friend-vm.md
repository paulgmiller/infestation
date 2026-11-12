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
