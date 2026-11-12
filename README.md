# Infestation

A kubernetes cluster that runs on any latency proximage (<30ms) compute using tailscale as the connective tissue.
Won't go down while you still have 2 nodes alive. 

Infestation provisions Flatcar VMs running host-level CockroachDB and K3s over
Tailscale. Nodes need outbound connectivity; cluster communication does not
require a shared underlay, public node addresses or an external ingress load
balancer. K3s uses its native Tailscale/Flannel integration, kube-proxy and CoreDNS.

QEMU and Azure are the current compute providers. The goal is to support any
compatible VM compute provider, preferably through Pulumi, with shared node
preparation and bootstrap. See [goals](docs/goals.md), [architecture](docs/architecture.md)
and the outstanding work in [TODO.md](TODO.md).

This is a prototype. The [mixed local/Azure test](docs/mixed-cluster.md) verified
three Ready nodes, three healthy database members and all six directed DNS/HTTP
checks after documented repairs. Unattended fresh bootstrap, replacement and
recovery still need verification. Test compute has been torn down; private local
state and reports remain preserved.

## Build and check

Requires Go 1.26.6 or newer:

```sh
go build -o bin/infestation ./cmd/infestation
go build -o bin/infestation-smoke ./cmd/infestation-smoke
go test ./...
go vet ./...
bin/infestation --help
```

Builds and unit tests do not provision resources. Running VM commands needs the
appropriate QEMU/KVM tools or authenticated Azure CLI. Connectivity checks need
kubectl and a reachable cluster.

## Create nodes

Use `infestation vm create` for local QEMU and `infestation azure create` for
Azure. Both create CockroachDB and K3s servers by default. Add `--agent` to omit
the Kubernetes API server while keeping CockroachDB; agents cannot use `--init`.
First apply the one-time [tailnet policy setup](docs/tailnet-policy.md), then see
[independent enrollment](docs/friend-vm.md).

[Three local VMs](docs/isolated-vm-cluster.md) and the
[mixed local/Azure runbook](docs/mixed-cluster.md) cover cluster creation. The CLI
records only that provider call’s nodes under `state/vms/<name>/nodes.json`.
Each call keeps its own private state; joining machines fetch credentials over
the tailnet rather than sharing a workstation directory. Keep generated state private.
[Local state](docs/local-state.md) lists its contents, secrets and lifecycle uses.

Helper source defaults to the published `master` branch. The creator resolves it
once to an immutable commit shared by every node in that call. Override with
`--helper-source-ref TAG-OR-COMMIT` when needed. Each Flatcar node builds the helper
in a pinned multiarchitecture Go container:

```sh
bin/infestation vm create --name lab --init --size 3 --state-dir state/vms \
  --tailnet-domain YOUR-TAILNET.ts.net \
  --tailscale-auth-key-file secrets/tsauth
```

Each command creates compute on one provider. For two local nodes and one
independently enrolled Azure node, use separate calls (which can run on different
workstations with different state directories):

```sh
bin/infestation vm create --name local --init --size 2 \
  --tailnet-domain YOUR-TAILNET.ts.net --tailscale-auth-key-file secrets/tsauth
bin/infestation azure create --name cloud --size 1 \
  --tailnet-domain YOUR-TAILNET.ts.net --tailscale-auth-key-file secrets/tsauth
```

The public CLI has the same lifecycle verbs for both providers: `create`,
`start`, `stop`, `status`, `wait` and `kubeconfig`. `vm status --netcheck`
includes guest network diagnostics. The separate `infestation-smoke` CLI runs
workload connectivity checks using Kubernetes credentials alone.
Every command has `--help`. Local VM control runs on its QEMU host; Azure control
requires saved provider state and Azure login. See [where commands run](docs/local-state.md#where-lifecycle-commands-run).

Only one founding node gets initialization authority. It waits for two database
peers. Other groups only join. Azure defaults to `westus2` and `Standard_B2s`;
override `--region` and `--sku` directly. Azure enrollment currently creates one
VM per call. Creation returns after provisioning by default; `--wait` also needs
tailnet access from the workstation. This independent flow has unit/protocol
coverage and still needs a fresh VM test. See [bootstrap](docs/bootstrap.md).

The renderer resolves a tag before provisioning. Credentials stay outside the
source/build container. Native arm64 execution remains unverified.

## Access and verification

```sh
bin/infestation vm kubeconfig --name lab --state-dir state/vms
export KUBECONFIG="$PWD/state/vms/lab/kubeconfig"
kubectl get nodes
bin/infestation-smoke --kubeconfig "$KUBECONFIG" \
  --out-dir smoke-results --tailnet-latency
```

The checker discovers all nodes through Kubernetes and tests every directed pair.
No provider inventory, local VM state or Azure login is needed. Optional host
Tailscale measurements use privileged Jobs; the caller needs the corresponding
Kubernetes permissions. It changes the test cluster and retains pods/services for
inspection. See [smoke checks](docs/smoke.md). See the
runbooks for route approvals, API Service setup and Tailscale SSH policy.

The tailnet is the administrator trust boundary. Routine SSH uses `core` with
sudo. Each API node publishes a root-only kubeconfig with Kubernetes CA/client
credentials and the shared API endpoint; normal TLS validation remains enabled.
See [bootstrap](docs/bootstrap.md) and [networking](docs/networking-plan.md).

## Repository layout

```text
cmd/                       CLI and node-helper entry points
internal/cluster/          inventory and validation
internal/pki/              certificates and token preparation
internal/ignition/         first-boot files and systemd configuration
internal/nodebootstrap/    node-local SQL setup and readiness
internal/tailnet/          host enrollment and runtime address configuration
internal/vm/               VM CLI and mixed-cluster prototype
internal/provider/         Pulumi provider implementations
internal/smoke/            workload connectivity and host latency checks
infra/                     Pulumi entry points
```

Never overwrite an existing CA/token or replace disks as a generic retry.
Infrastructure changes must preserve resource ownership and stack identity.
