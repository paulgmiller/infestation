# Egress-only K3s networking checkpoint

Last updated: 2026-11-08. This records the agreed design and investigation;
replacement networking and the API TailVIP have **not been deployed or verified**.

After merging origin/master's Go migration, `internal/cluster` accepts optional
`api_tailnet_hostname`. `internal/ignition` adds the shared SAN and a host-level
Serve unit on server profiles only, including API pools. Worker profiles never
advertise the API. This does not install/enroll Tailscale or define/approve a
Service in its control plane.

Current provider profiles still use Cilium, disable kube-proxy, and retain Azure
load-balancer networking. The architecture below is the **target**, not the
current deployed configuration. Removing Cilium and changing provider defaults
remain separate implementation work. The original Python scaffold is retired;
its existing local state is preserved and ignored.

## Goal and component ownership

Three Flatcar nodes must be able to form a cluster without a shared private LAN,
public node IPs, inbound port forwarding, or a self-managed external load balancer.
Outbound connectivity, DNS, and time synchronization remain required.

| Component | Responsibility |
| --- | --- |
| Host-level Tailscale | Node connectivity and encrypted cross-node pod routing |
| K3s Tailscale/Flannel integration, under evaluation | PodCIDR assignment plumbing, local CNI setup, and subnet advertisements |
| kube-proxy and CoreDNS | Standard Kubernetes Services and discovery |
| kube-network-policies | Kubernetes pod ingress/egress policy |
| Tailscale Service/TailVIP | One private Kubernetes API address across three server hosts |
| Three Cloudflare Tunnel replicas | Public HTTP/HTTPS ingress through one named tunnel |
| Existing host-level CockroachDB | Shared K3s datastore through Kine, retaining TLS |

Remove Cilium tooling and active installation instructions. Preserve historical
Cilium test results as historical evidence only. Keep kube-proxy enabled and
K3s's built-in network-policy controller disabled. Do not use Funnel or TailVIPs
for ordinary Kubernetes Services. The API TailVIP is an explicit exception.

## Private API endpoint: decided

Create one Tailscale Service named `svc:k8s-api`, accepting `tcp:443`. All three
tagged server hosts advertise it, with each host forwarding raw TCP to its own
`127.0.0.1:6443`. **Do not forward to the Kubernetes ClusterIP.**
Worker-only nodes must not advertise the API Service. The Go generator enforces
this by role; servers without local SQL still advertise it.

```text
kubectl on tailnet -> k8s-api.<tailnet>.ts.net:443
                       -> selected server's tailscaled
                       -> that server's 127.0.0.1:6443
```

After defining the Service and its access/approval policy, run on each server:

```sh
sudo tailscale serve --service=svc:k8s-api --tcp=443 tcp://127.0.0.1:6443
```

Service hosts require tag-based identities. Approve all three advertisements or
configure scoped service auto-approval. Grant the intended administrative clients
access to `svc:k8s-api` on TCP 443. Do not replace unrelated tailnet policy or
Serve configurations.

Add the actual shared DNS name to every K3s server's `tls-san` before using it in
kubeconfig. Retain the Kubernetes CA and client credentials; do not terminate TLS
in Serve or disable certificate verification. The kubeconfig endpoint becomes
`https://k8s-api.<tailnet>.ts.net:443`.

Host-level Serve must remain independent of Kubernetes pods, DNS, and operators.
Keep direct per-server API endpoints for bootstrap and recovery.

Default Tailscale routing is active/passive failover. Regional routing can
distribute clients on eligible plans, but per-connection round-robin balancing is
not a v1 requirement. Connections can break during failover. Tailscale host
availability does not prove local API readiness: explicit `/readyz` monitoring
and advertisement withdrawal are follow-up work, not a property to claim for v1.

Prerequisites still outstanding: identify/enroll the three actual server hosts,
confirm local API listeners and certificate SANs, and define/approve the Service.
The management host is not an API backend.

## Reuse K3s before writing a custom networking agent

K3s has experimental Tailscale integration via `--vpn-auth`/`--vpn-auth-file`.
Our pinned K3s v1.35.8+k3s1 configures upstream Flannel's generic `extension`
backend to run `tailscale set --accept-routes --advertise-routes=<subnets>`.
This delegates transport to Tailscale rather than adding VXLAN over it.
Allowing Flannel for this orchestration role revisits the original no-Flannel
constraint; do not silently enable its default VXLAN backend.

Investigate this integration before implementing a custom PodCIDR reconciler.
If it cannot satisfy host-network independence, use bridge CNI plus host-local
IPAM and a small host agent that reads the node's assigned PodCIDR and reconciles
local CNI configuration and advertisements.

K3s emits `ShutdownCommand: tailscale down`, but its pinned upstream Flannel
v0.28.4 extension parser has no such field. Source inspection indicates that
field is ignored; shutdown behavior still needs a VM test. K3s can also run
`tailscale up --reset` when the client is not running. Verify that enrollment,
restarts, and configuration reconciliation preserve independently managed host
networking and preferences.

Each node advertises only its assigned PodCIDR, accepts peer routes, and preserves
pod source IPs (`--snat-subnet-routes=false`). Validate internet masquerading,
local-route precedence, MTU, and route approval. Start at pod MTU 1280. Use IPv4
initially and reject overlapping pod, service, underlay, or existing tailnet ranges.
Persist node identity and IPAM state; reconcile stale routes on replacement.

## Accepted tradeoffs and remaining risks

- Same-node pod traffic may be plaintext within the host. Cross-node encryption
  also terminates at the hosts; use application TLS/mTLS where required. Hosts
  and privileged workloads remain trusted infrastructure.
- NAT traversal may fall back to DERP, reducing throughput and increasing latency.
  Cloudflare needs outbound TCP or UDP 7844; HTTPS-only egress is insufficient.
- Keep the initial control-plane/database nodes in a low-latency region. A mesh
  does not remove CockroachDB quorum latency or make WAN partitions harmless.
- The existing unsupported CockroachDB/Kine compatibility, local SQL endpoint,
  certificate renewal, storage, and backup limitations remain.
- Verify Flatcar's netfilter/NFQUEUE support, same-node bridge filtering, and
  K3s/containerd NRI integration. `kube-network-policies` defaults to fail-open;
  setting fail-open false does not establish continuous fail-closed behavior,
  especially during graceful shutdown, initial pod setup, and agent upgrades.
- Keep Tailscale grants/host firewall rules for infrastructure separate from pod
  NetworkPolicy. Test DNS, API, and Cloudflare connector exceptions explicitly.
- A Cloudflare connector can retain internet access while losing its backend
  path. Tunnel availability is not application readiness or capacity balancing.
- Public HTTPS terminates at Cloudflare. V1 public exposure covers web apps, not
  arbitrary TCP/UDP, and application identity/source-IP semantics change at proxies.
- Vendor control planes, relays, credentials, key expiry, and certificate renewal
  remain dependencies. Preserve host identities and test recovery after outages.

## Implementation sequence and boundaries

1. Save this checkpoint; update README and agent instructions to reflect it.
   Completed, including migration of the optional API TailVIP configuration to Go.
2. Retire the Go bootstrap Cilium chart and related active setup instructions as
   part of the networking transition. Re-enable kube-proxy in coordination with
   installing replacement networking; retain generic DNS, Service, cross-node,
   and datastore smoke checks. Label prior Cilium validation historical. Until
   a replacement CNI is installed, new nodes may remain NotReady.
3. Validate the native K3s/Tailscale path in a disposable profile and choose it
   over custom glue if restart and source-IP behavior meet the requirements.
4. Configure and verify the host-level API TailVIP described above.
5. Add standard namespace NetworkPolicy enforcement, then Cloudflare replicas
   spread across nodes. Pin component versions/digests after compatibility checks.

Do not delete lab state or uninstall software from running guests as part of the
repository cleanup. Provisioning, live migration, and datastore initialization
are separate operations. Do not run `cockroach init` on existing stores.

## Latency and acceptance tests

There is no dedicated latency report in the existing lab tooling. Measure from
every node to every other node (six directed pairs for three nodes):

- `ping -n -c 30 PEER_IP`: packet loss and min/average/max network RTT.
- `tailscale ping PEER_IP`: tunnel latency and direct/relay path once installed.
- Timed HTTP requests to direct pods and ClusterIPs: application-path overhead.

Record cold-start and warmed-up measurements separately; paths can transition
from relayed to direct. Use RTT initially; accurate one-way measurements require
synchronized clocks. VMs on one workstation do not model real WAN latency.

Before claiming the design reliable:

1. Cold-start nodes without a shared node LAN or inbound reachability; verify
   API, datastore, PodCIDRs/routes, Services, and DNS.
2. Check direct pod and ClusterIP TCP/UDP across all pairs, same-node and hairpin
   paths, large packets, source-IP preservation, direct/relay performance, CPU,
   connection rate, and policy convergence.
3. Verify default-deny and selective allow for same-node/cross-node traffic,
   DNS, internet egress, and connector-to-application traffic.
4. Crash, gracefully stop, and restart networking/policy agents; create pods
   during outages and document any isolation gaps.
5. Access the API TailVIP with verified Kubernetes TLS; test reads, writes,
   watches, exec/log streaming, per-host failure, and a local SQL/API failure
   while the host remains connected to Tailscale. Record failover limitations.
6. Remove each Cloudflare connector and reboot nodes individually while probing
   the public URL. Test a connector whose backend path fails independently.
7. Test coordination loss, credential renewal, node replacement, and datastore
   quorum loss. Distinguish observed behavior from planned tests.

## References

- [K3s distributed clusters and Tailscale integration](https://docs.k3s.io/networking/distributed-multicloud)
- [Pinned K3s Flannel/Tailscale setup](https://github.com/k3s-io/k3s/blob/v1.35.8%2Bk3s1/pkg/agent/flannel/setup.go)
- [Pinned K3s VPN lifecycle](https://github.com/k3s-io/k3s/blob/v1.35.8%2Bk3s1/pkg/vpn/vpn.go)
- [Flannel v0.28.4 extension parser](https://github.com/flannel-io/flannel/blob/v0.28.4/pkg/backend/extension/extension.go)
- [Tailscale Services](https://tailscale.com/docs/features/tailscale-services)
- [Serve TCP forwarding](https://tailscale.com/docs/reference/tailscale-cli/serve#use-a-tcp-forwarder)
- [Tailscale route selection](https://tailscale.com/docs/features/route-selection)
- [Tailscale site-to-site routing](https://tailscale.com/docs/features/site-to-site)
- [Network policy implementation](https://github.com/kubernetes-sigs/kube-network-policies)
- [Cloudflare Tunnel redundancy](https://developers.cloudflare.com/cloudflare-one/networks/connectors/cloudflare-tunnel/configure-tunnels/tunnel-availability/)
