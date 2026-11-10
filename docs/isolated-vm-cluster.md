# Three-node cluster with isolated VM networks

The VM cluster command creates three Flatcar control-plane nodes, each hosting
CockroachDB. Each QEMU process has its own SLIRP NAT and one virtio NIC. There is
no common bridge, TAP, virtual switch, host port forward, or public node IP.
Nodes use host Tailscale for peer communication. They still need internet egress.

This is a local proof of the topology; it does not simulate WAN latency between
friends' houses or clouds. Azure migration remains separate.

## Create

On Linux x86-64, install QEMU/KVM, qemu-img and kubectl. Build the CLI and static
Linux helper from the checkout:

```sh
go build -o bin/infestation ./cmd/infestation
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o bin/infestation-node ./cmd/infestation-node

bin/infestation vm cluster create --name lab \
  --tailnet-domain YOUR-TAILNET.ts.net \
  --tailscale-auth-key-file secrets/tsauth \
  --state-dir state/vms
```

Alternatively supply `TAILSCALE_AUTH_KEY` or `--tailscale-auth-key`. The command
requires a tagged enrollment key and refuses an existing state directory. It
creates `lab-0`, `lab-1` and `lab-2`, using 3072 MiB and two virtual CPUs per VM.
`--memory-mib` sets memory per VM. KVM is the default; `--accel tcg` permits slow
software emulation. `--base-image` accepts a previously verified Flatcar qcow2;
otherwise the pinned image is downloaded and SHA512-verified.

The initial bootstrap renders private PKI and a persistent K3s token once. The
CA signing key and long-lived SQL admin key remain on the workstation. Only
node `-0` receives the existing one-hour SQL bootstrap credential. It initializes
Cockroach once, configures Kine SQL, then removes that credential. Resume commands
do not regenerate the CA, token or database identity.

Only a small Ignition document travels through QEMU firmware. A temporary HTTP
server bound to host loopback serves **only the non-secret node executable**;
Ignition pins its SHA512 digest. Guest requests use QEMU's local gateway. The
server does not expose PKI, profiles or credentials and shuts down after API
readiness. The helper is installed persistently in each VM. No remote service
or inbound VM port is required for this provisioning transport.

## Approve pod routes and verify

K3s allocates one PodCIDR per node and its native Tailscale/Flannel extension
backend advertises that route. Approve the advertised routes in the Tailscale
console. For the default allocation this is:

| Node | Advertised route |
| --- | --- |
| lab-0 | 10.42.0.0/24 |
| lab-1 | 10.42.1.0/24 |
| lab-2 | 10.42.2.0/24 |

For unattended enrollment, configure scoped route auto-approval for the key's
tag. With the current `tag:roach` identity, merge this section into the existing
tailnet policy once:

```json
"autoApprovers": {
  "routes": {
    "10.42.0.0/16": ["tag:roach"]
  }
}
```

This authorizes that tag to advertise the `/16` or its contained subnets, so new
nodes' `/24` advertisements can be approved automatically. Keep using auth keys
assigned that tag. K3s assigns the individual node subnets; give separate
clusters disjoint PodCIDRs. Existing unapproved advertisements need to be
withdrawn/re-advertised for a newly added auto-approver rule to apply. The current
three routes were approved manually. See
[Tailscale auto approvers](https://tailscale.com/docs/reference/syntax/policy-file#auto-approvers).

Also allow the necessary pod-CIDR traffic in the tailnet policy; source
pod IPs are preserved. See the official
[K3s Tailscale configuration](https://docs.k3s.io/networking/distributed-multicloud#integration-with-the-tailscale-vpn-provider-experimental).
An enrollment auth key cannot approve routes or change the policy.

`create` waits for all three private API endpoints and Ready nodes, then writes
`state/vms/lab/kubeconfig` with mode `0600`. **Node readiness does not prove that
pod routes are approved.** Run the directed DNS/Service workload check:

```sh
bin/infestation smoke --config state/vms/lab/cluster.json \
  --state-dir state/vms/lab --kubeconfig state/vms/lab/kubeconfig
```

This creates test pods and fresh jobs in `infestation-smoke`. It checks DNS and
ClusterIP HTTP across every directed pair (six for three nodes). Completed jobs
expire after an hour. After approving routes, rerun to create fresh jobs rather
than relying on results from a failed attempt.

## Inspect and resume

```sh
bin/infestation vm cluster status --name lab --state-dir state/vms
bin/infestation vm cluster wait --name lab --state-dir state/vms
bin/infestation vm cluster stop --name lab --state-dir state/vms
# Once all VMs have shut down:
bin/infestation vm cluster start --name lab --state-dir state/vms
```

`start --accel kvm` or `--accel tcg` overrides the stored accelerator for that
start. State, data and identity are preserved. `status` reads private guest
reports containing service state, sanitized logs, peer resolution, TCP checks and
routes. Kubeconfigs arrive on a separate private virtio channel, never the
console. There is no SSH requirement and no automatic disk deletion command.

CoreDNS and kube-proxy remain enabled, and Cilium is absent from these profiles.
NetworkPolicy enforcement, public ingress, API TailVIP provisioning and
coordinated maintenance are not established by this test. Same-node pod traffic
is not encrypted. Read the actual test evidence and pending checks in the
[networking checkpoint](networking-plan.md).
