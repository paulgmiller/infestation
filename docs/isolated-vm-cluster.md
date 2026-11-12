# Three local nodes with independent VM networks

Each local QEMU VM has its own SLIRP NAT and uses host Tailscale for cluster
traffic. No shared bridge, host port forwarding or public node IP is required.
The independent enrollment flow has protocol/unit coverage; fresh VM verification
is still pending. See the [mixed test record](mixed-cluster.md) for earlier live
evidence and [bootstrap](bootstrap.md) for the current trust/recovery contract.

## Create

Follow the [local VM prerequisites](local-vm-prerequisites.md) to install
QEMU and the CLI and check KVM access on Linux x86-64. The published
`master` branch must contain the updated helper. The tailnet key must enroll `tag:roach`
nodes, with grants for helper TCP 7444 and cluster traffic. Configure pod-route
and API-Service approval as described in [networking](networking-plan.md).

```sh
infestation vm create --name lab --init --size 3 --state-dir state/vms \
  --tailnet-domain YOUR-TAILNET.ts.net \
  --tailscale-auth-key-file secrets/tsauth
```

This creates `lab-0/1/2` with 3072 MiB and two virtual CPUs each. Only the first
node has initialization authority. `--size` otherwise defaults to one, and
`--memory-mib` changes per-VM memory. `--accel tcg` permits slow software emulation.
`--base-image` uses an existing trusted Flatcar image rather than downloading it.

Every node hosts CockroachDB and K3s. `--agent` selects K3s agents without API
servers while keeping CockroachDB. It cannot be combined with `--init`. Create
agent groups independently after designating one founding server.

Source defaults to `master`; optional `--helper-source-ref` selects another
branch, tag or commit. The renderer resolves it once before provisioning. Each node
builds the helper in the official multiarchitecture Go container. The build
receives no credentials or host Docker socket. Cluster joiners fetch credentials
from enrolled peers over the tailnet.

## Access and lifecycle

Creation returns after provisioning. A tailnet-connected workstation can wait
for readiness and retrieve the private kubeconfig:

```sh
infestation vm wait --name lab --state-dir state/vms
infestation vm kubeconfig --name lab --state-dir state/vms
export KUBECONFIG="$PWD/state/vms/lab/kubeconfig"
kubectl get nodes

infestation vm status --name lab --state-dir state/vms
infestation vm status --name lab --state-dir state/vms --netcheck
infestation vm stop --name lab --state-dir state/vms
infestation vm start --name lab --state-dir state/vms
```

`wait` also saves kubeconfig, so subsequent explicit retrieval may need `--force`.
Node readiness does not prove that pod routes are approved. Run workload checks:

```sh
infestation-smoke --kubeconfig "$KUBECONFIG" \
  --out-dir smoke-results --tailnet-latency
```

The API Service is declared by node configuration and forwarded by server hosts
to localhost:6443 with TLS passthrough. Define/approve `svc:k8s-api` in Tailscale
separately; do not disable Kubernetes certificate validation.

Each provider group owns only its local nodes and private state. Start/stop
preserve disks. Failed creation preserves state/resources for inspection. Partial
provisioning repair is not supported, and expired SQL credentials need deliberate
recovery. Keep partial failure evidence instead
of deleting disks or initializing another database.
