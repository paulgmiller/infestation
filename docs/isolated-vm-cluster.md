# Three local nodes with independent VM networks

Each local QEMU VM has its own SLIRP NAT and uses host Tailscale for cluster
traffic. No shared bridge, host port forwarding or public node IP is required.
The independent enrollment flow has protocol/unit coverage; fresh VM verification
is still pending. See the [mixed test record](mixed-cluster.md) for earlier live
evidence and [bootstrap](bootstrap.md) for the current trust/recovery contract.

## Create

Install QEMU/KVM, `qemu-img` and the updated CLI on Linux x86-64. Use a published
source ref containing the updated helper. The tailnet key must enroll `tag:roach`
nodes, with grants for helper TCP 7444 and cluster traffic. Configure pod-route
and API-Service approval as described in [networking](networking-plan.md).

```sh
infestation vm create --name lab --init --size 3 --state-dir state/vms \
  --tailnet-domain YOUR-TAILNET.ts.net \
  --tailscale-auth-key-file secrets/tsauth \
  --helper-source-ref YOUR-PUBLISHED-TAG-OR-COMMIT
```

This creates `lab-0/1/2` with 3072 MiB and two virtual CPUs each. Only the first
node has initialization authority. `--size` otherwise defaults to one, and
`--memory-mib` changes per-VM memory. `--accel tcg` permits slow software emulation.
`--base-image` uses an existing trusted Flatcar image rather than downloading it.

Every node hosts CockroachDB and K3s. `--agent` selects K3s agents without API
servers while keeping CockroachDB. It cannot be combined with `--init`. Create
agent groups independently after designating one founding server.

The official multiarchitecture Go container builds the helper from the supplied
source ref. The build receives no credentials or host Docker socket. Alternatively,
local creation accepts an already built `--node-helper`; a loopback artifact
server supplies only that executable with a pinned checksum. Cluster credentials
are always private and joiners fetch them over the tailnet.

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
infestation smoke --config state/vms/lab/nodes.json \
  --state-dir state/vms/lab --kubeconfig "$KUBECONFIG" --tailnet-latency
```

The API Service is declared by node configuration and forwarded by server hosts
to localhost:6443 with TLS passthrough. Define/approve `svc:k8s-api` in Tailscale
separately; do not disable Kubernetes certificate validation.

Each provider group owns only its local nodes and private state. Start/stop
preserve disks. Resume uses original profiles and never regenerates CA/token or
renews an expired SQL bootstrap credential. Keep partial failure evidence instead
of deleting disks or initializing another database.
