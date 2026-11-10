# Infestation

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
go test ./...
go vet ./...
bin/infestation --help
```

Builds and unit tests do not provision resources. Running VM commands needs the
appropriate QEMU/KVM tools or authenticated Azure CLI. Connectivity checks need
kubectl and a reachable cluster.

## Create a VM or cluster

[Create a VM on a friend's tailnet](docs/friend-vm.md) covers standalone enrollment.
That VM does not automatically join Kubernetes or run CockroachDB.

[Three local VMs](docs/isolated-vm-cluster.md) and the
[mixed local/Azure runbook](docs/mixed-cluster.md) cover cluster creation. The CLI
generates a private inventory under `state/vms/<name>/cluster.json`; it also
preserves PKI, token, profiles and reports there. Keep that directory private.

The helper can be built on each Flatcar node in a pinned multiarchitecture Go
container from an immutable source commit:

```sh
bin/infestation vm cluster create --name lab --state-dir state/vms \
  --tailnet-domain YOUR-TAILNET.ts.net \
  --tailscale-auth-key-file secrets/tsauth \
  --helper-source-ref YOUR-PUBLISHED-TAG-OR-COMMIT
```

The renderer resolves a tag before provisioning. Credentials stay outside the
source/build container. Native arm64 execution remains unverified.

## Access and verification

```sh
export KUBECONFIG="$PWD/state/vms/lab/kubeconfig"
kubectl get nodes
bin/infestation smoke --config state/vms/lab/cluster.json \
  --state-dir state/vms/lab --kubeconfig "$KUBECONFIG" --tailnet-latency
```

The smoke command creates workload checks for every directed node pair and can
record host Tailscale latency/path samples. It changes the test cluster. See the
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
