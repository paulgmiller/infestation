# Egress-only K3s networking checkpoint

Last updated: 2026-11-10 (local date). The isolated three-VM Tailscale cluster and
private API TailVIP are **deployed and verified** as described below. The static mixed local/Azure prototype also passed after repairs; see
[the mixed test record](mixed-cluster.md). The older Azure VMSS migration,
unattended fresh mixed bootstrap, and API failover drills remain unverified.

After merging origin/master's Go migration, `internal/cluster` accepts optional
`api_tailnet_hostname`. `internal/ignition` adds the shared SAN and a host-level
Serve unit on server profiles only, including API pools. Worker profiles never
advertise the API. This does not install/enroll Tailscale or define/approve a
Service in its control plane.

Existing Azure/libvirt provider defaults still use Cilium and disable
kube-proxy; Azure retains its load-balancer networking. The new direct-VM
cluster command uses native K3s Tailscale/Flannel, kube-proxy and CoreDNS without
Cilium. Azure migration remains separate. The original Python scaffold is retired;
its existing local state is preserved and ignored.

## VM enrollment secret handling

The in-progress static VM renderer accepts either an inventory
`tailscale.auth_key_file`, `ignition --tailscale-auth-key-file PATH`, or
`ignition --tailscale-auth-key KEY`. Supply exactly one source. The raw argument
is render-only: it is not saved in the cluster inventory or node bootstrap JSON.
Validation and PKI generation do not require the enrollment key.

Use a short-lived, reusable, preauthorized key whose tags identify lab nodes.
The key is embedded in private Ignition as `/etc/infestation/tailscale-authkey`
(mode `0600`). The host helper passes a file reference to `tailscale up` and
deletes that guest file after successful enrollment and identity validation.
Reboots reuse `/var/lib/tailscale`; they do not reset or reenroll a running node.
Deleting the guest file does not remove copies retained in Ignition, provisioning
storage or backups. Generated Ignition must remain private. Literal command-line
keys may also be retained in shell history or visible in process arguments.

Unit tests cover enrollment, identity mismatch, durable identity reuse and
render-only argument handling. After merging origin/master's `azurecheck` and
public smoke checks, the CLI builds and `go test ./...` passes. CLI tests exercise
both auth flags through private Ignition generation with disposable test keys.
The VM tests below establish enrollment and peer connectivity, but do not
establish Kubernetes cross-node pod connectivity.

### 2026-11-09 VM boot check

Added `infestation vm create/start/status/stop`; see the
[friend setup guide](friend-vm.md). Creation supports an auth-key argument,
`TAILSCALE_AUTH_KEY`, or a private key file, downloads a SHA512-verified Flatcar
image by default, and uses a separate QEMU user-mode NAT per VM with no inbound
port forwarding. It does not configure Kubernetes or CockroachDB yet.

Booted a new disposable `enroll-smoke-1109` VM using TCG (the initial sandbox
device check missed the host's available KVM device),
the previously cached Flatcar 4757.2.2 base and an intentionally invalid auth key.
The cached archive matches its recorded vendor SHA512 digest. Ignition fetched
the Tailscale v1.104.2 sysext; the guest reported
`1.104.2-tc3430f0d2-g598b63867`, kernel TUN mode, and `NeedsLogin` over a private
virtio status channel. The CLI correctly refused to label this enrolled.
Graceful QMP shutdown completed and restart reused the existing overlay.
Existing lab disks were preserved.

With a supplied auth key, the opt-in `TestLiveEnrollmentAndRestart` then passed
in **201.123 seconds** under TCG. A fresh VM reported a running, tagged identity
in kernel TUN mode. After graceful shutdown and restart, the test verified the
same stable node ID, DNS name, tailnet IPs and VM disk. The host's Tailscale client
also saw the new peer. The test requested shutdown and retained private artifacts
under `/tmp/inf-vm-*`; no existing lab VM was changed. Normal tests skip this
provisioning test. A tailnet API management key is not required.

A further CLI start succeeded without an auth key. From the host, three
`tailscale ping` discovery probes received replies through Seattle DERP in
**17 ms, 10 ms, and 10 ms**. A direct path was not established; the default ping
command exited nonzero because its direct-path criterion was not satisfied.
These are peer discovery timings, not pod-to-pod or application latency tests.
Graceful shutdown was requested after the probes.

Host `tailscale netcheck` reported UDP available, endpoint-independent mapping
(`MappingVariesByDestIP: false`), IPv4 available and IPv6 unavailable. This does
not characterize the guest's additional SLIRP layer or prove why this host/guest
pair used DERP. The initial three-ping sample was too short to conclude that
SLIRP prevented a direct connection: the subsequent comparison below reached a
direct path. A private TAP/kernel-NAT network is another option at the cost of
privileged host setup.

Those initial checks covered standalone enrollment. The later three-VM cluster
test below verifies Cockroach and K3s cross-node connectivity.

### 2026-11-09 isolated three-VM cluster: verified

Created `tail3-1109-0`, `tail3-1109-1` and `tail3-1109-2`, each with its own
QEMU SLIRP backend and one NIC. There is no shared bridge, TAP, hub, socket
network or host port forward. Every VM has the same private underlay address
`10.0.2.15` inside its separate NAT, and a distinct Tailscale address. Node
diagnostics confirmed peer names resolve to tailnet IPs and peer routes use
`tailscale0`. The three VMs are left running; original lab storage was preserved.

| Node | Tailnet node IP | Assigned PodCIDR |
| --- | --- | --- |
| tail3-1109-0 | 100.82.228.1 | 10.42.0.0/24 |
| tail3-1109-1 | 100.122.250.100 | 10.42.1.0/24 |
| tail3-1109-2 | 100.65.35.80 | 10.42.2.0/24 |

Actual verification after the user approved the three pod routes:

- Three K3s **v1.35.8+k3s1** control-plane nodes Ready, each private API
  `/readyz` successful through Tailscale.
- Host CockroachDB **v26.2.7**: three live/available members advertising tailnet
  DNS endpoints, 95 ranges, **zero unavailable or under-replicated ranges**.
  A local client container used workstation admin credentials read-only; no
  long-lived admin key or CA signing key was delivered to a VM.
- Native Flannel backend annotations report **extension**. CoreDNS, local-path
  provisioner and metrics-server are running. No Cilium chart/agent is installed;
  kube-proxy is enabled.
- All **six directed DNS/ClusterIP HTTP smoke checks passed**, matching the
  expected remote node's HTTP response. The successful run ID is
  `6fab8ea6-8a60-41ff-b0a5-60dc7ed4dbc1` (generated manifests are in private state).
  The earlier attempt before route approval failed; fresh jobs were used for
  the successful rerun.
- `go test ./...` passed; source/build checks do not themselves provision VMs.

Private cluster state is `state/vms/tail3-1109`; its `kubeconfig` is mode `0600`.
See [the create/resume runbook](isolated-vm-cluster.md) for commands. Node Ready
alone is not a successful pod-network check: route approval and policy must
permit pod traffic as the smoke test established here.

Boot fixes made during this experiment: the initial device check ran inside
the tool sandbox and missed accessible host KVM. The first attempts used TCG;
the successful cluster runs with KVM, two vCPUs and 3072 MiB per VM. The original
14 MB firmware-fed Ignition documents also stalled in `ignition-fetch-offline`.
Moving only the 10 MB non-secret helper to a loopback HTTP server with a pinned
SHA512 reduced the profiles to 20–25 KB and cleared that wait. PKI, token and
the existing overlays were preserved. The temporary artifact server stopped
after API readiness; credentials/profiles were never served by it.

The owner added this auto-approval policy to the existing tailnet policy
(the auth key uses `tag:roach`):

```json
"autoApprovers": {
  "routes": {
    "10.42.0.0/16": ["tag:roach"]
  },
  "services": {"svc:k8s-api": ["tag:roach"]}
}
```

Tailscale's [auto-approver rules](https://tailscale.com/docs/reference/syntax/policy-file#auto-approvers)
permit contained subnets, covering each future node's `/24`. K3s assigns those
subnets; the tailnet access rules separately govern their traffic. New rules
apply to new advertisements; existing pending routes must be re-advertised or
approved manually. The current routes were approved manually; automatic approval
of a future node has not been exercised. Separate clusters need disjoint pod
address blocks.

Remaining work includes NetworkPolicy enforcement, public ingress,
failure/recovery drills, and Azure migration. This one-host test
establishes isolated-underlay connectivity, not WAN performance or production
reliability across sites.

### 2026-11-09 passt versus SLIRP comparison

Tested QEMU **10.2.1**, Flatcar **4757.2.2**, Tailscale **v1.104.2** and passt
**0.0~git20260120.386b5f5-1**, using TCG. The same VM disk and enrolled identity
were started first with `--network slirp`, then `--network passt`. No inbound
port forwards were configured; passt explicitly used
`tcp-ports=none,udp-ports=none`. QEMU's monitor confirmed the active passt helper.

New profiles run the actual guest `tailscale netcheck --format=json` after
enrollment and every five minutes. `infestation vm netcheck --name NAME`
displays the latest complete guest report through a private virtio channel,
without SSH. These were guest results, not the host's netcheck output.

| Check | SLIRP | passt |
| --- | --- | --- |
| Implementation | libslirp inside QEMU | Separate helper using native host sockets |
| Extra host prerequisite | None beyond QEMU | Installed passt package; namespace sandbox allowed |
| Guest UDP / IPv4 | Both available | Both available |
| Usable public IPv6 | Unavailable | Unavailable |
| `MappingVariesByDestIP` | `false` | `false` |
| Guest UPnP / NAT-PMP / PCP | All undetected | All undetected |
| WireGuard-level TSMP probe | Succeeded, initial probe 49 ms | Succeeded, initial probe 45 ms |
| Discovery route to VM | Direct, observed 1 ms | Seattle DERP after 10 + 20 probes |

SLIRP's guest report was timestamped `2026-11-09T22:56:23Z`; passt's was
`2026-11-09T23:00:57Z`. Passt discovery replies were typically 9–13 ms, with
outliers up to 78 ms. Its discovery commands exited nonzero solely because no
direct path was established. The private passt report was retained at
`state/vms/passt-check-1109/netcheck-passt.json`. Graceful shutdown was requested
after testing; existing lab VMs and disks were untouched.

No SLIRP networking option was changed to obtain the direct result: it remained
`-netdev user,id=egress` without host forwarding. The comparison used a fresh VM
session, waited for enrollment/guest netcheck, sent a TSMP probe first, and then
allowed up to ten discovery probes rather than the earlier three. Additional
time and traffic may have helped Tailscale discover a direct endpoint; that is
an inference, not an isolated causal result. The earlier short sample did not
justify blaming SLIRP for relaying.

**Decision: keep SLIRP as the default; retain passt as an opt-in backend.**
Passt did not improve direct connectivity in this run. These observations are
for one local host/VM pair, not different friends' networks, a throughput
benchmark, or proof of the cause of passt's relaying. Initial TSMP timings are
not steady-state latency comparisons. The upstream
[QEMU networking docs](https://www.qemu.org/docs/master/system/devices/net.html)
describe passt's performance and IPv6 advantages; neither was demonstrated here.

The locally unpacked passt binary initially failed before VM boot with
`Failed to detach isolating namespaces: Operation not permitted`. Installing
Ubuntu's package, including its AppArmor profile, let the same backend launch.
Use the packaged helper rather than recommending a global relaxation of host
namespace restrictions. The QEMU build must list `passt` in `-netdev help`.

#### MiniUPnP as a possible optimization

[MiniUPnP](https://github.com/miniupnp/miniupnp) supplies an IGD client
(`miniupnpc`) and a gateway daemon (`miniupnpd`). Tailscale already attempts
automatic port mapping where supported; see its
[NAT traversal explanation](https://tailscale.com/blog/nat-traversal-improvements-pt-1).
The host detected UPnP, NAT-PMP and PCP, while neither guest detected them.

A host-side MiniUPnP client could be an optional explicit mapping mechanism,
but would need to coordinate an upstream router mapping with the VM backend's
UDP endpoint/forwarding. Simply installing it in the guest would not guarantee
both layers are mapped. Running miniupnpd would add a gateway service and
backend integration. No MiniUPnP mappings or daemons were configured in this
experiment. Do not make them a requirement for the egress-only design; retain
Tailscale's relay fallback. SLIRP already obtained a direct path without a
guest-visible port-mapping service.

## Goal and component ownership

Every Flatcar node, both API servers and workers, must be able to join and run
with outbound connectivity only. Nodes need not share a VNet/VPC, private LAN,
or mutually routable underlay addresses. Do not require public node IPs, inbound
port forwarding, or a self-managed external load balancer. Responses to outbound
connections/NAT mappings are expected; this is not an air-gapped cluster.
Outbound connectivity, DNS, and time synchronization remain required.

| Component | Responsibility |
| --- | --- |
| Host-level Tailscale on every Flatcar server and worker | Node connectivity and encrypted cross-node pod routing |
| K3s's native Tailscale/Flannel extension integration, selected | PodCIDR assignment plumbing, local CNI setup, and subnet advertisements |
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

Live setup status after the VM cluster test: all three API hosts now have
persistent raw TCP forwarding for `svc:k8s-api` on port 443 to local port 6443.
The one-time setup Jobs completed. TLS verification using the common
`kubernetes.default.svc.cluster.local` certificate name succeeded against every
direct API server. A private `kubeconfig.tailvip` preserves the existing
Kubernetes CA and client credentials and sets that `tls-server-name` while
targeting `https://k8s-api.<tailnet>.ts.net:443`. This approach needs no serving
certificate rotation or TLS termination at Tailscale.

The owner defined the Service and added service auto-approvers. The working
`state/vms/tail3-1109/kubeconfig` now targets
`https://k8s-api.quoll-duck.ts.net:443` (VIP `100.88.130.231`), with normal CA
verification and the existing client credentials. The direct endpoint remains
backed up in `kubeconfig.direct`. Authenticated `/readyz` and `kubectl get nodes`
through the Service succeeded again on November 10; all three nodes were Ready.
The owner's console lists nodes 0 and 1 as online Service hosts. Node 2's local
Serve configuration and advertisement were verified and refreshed, but its
control-plane acceptance has not been confirmed. Three Ready Kubernetes nodes
does not establish three accepted Service hosts. API failover has not been
tested. See the [runbook](isolated-vm-cluster.md#stable-private-api-name-with-https)
for repeatable setup.

The direct certificate already covers the canonical Kubernetes name, and the
new candidate retains normal CA/hostname verification. For freshly generated
profiles, the existing `api_tailnet_hostname` option can additionally put the
TailVIP's own DNS name in the certificate SANs.

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

For new profiles, add the shared DNS name to every K3s server's `tls-san`.
Existing profiles can use the verified common certificate identity via
`tls-server-name: kubernetes.default.svc.cluster.local`, as tested above.
Retain the Kubernetes CA and client credentials; do not terminate TLS
in Serve or disable certificate verification. The kubeconfig endpoint becomes
`https://k8s-api.<tailnet>.ts.net:443`.

Host-level Serve must remain independent of Kubernetes pods, DNS, and operators.
Keep direct per-server API endpoints for bootstrap and recovery.

Default Tailscale routing is active/passive failover. Regional routing can
distribute clients on eligible plans, but per-connection round-robin balancing is
not a v1 requirement. Connections can break during failover. Tailscale host
availability does not prove local API readiness: explicit `/readyz` monitoring
and advertisement withdrawal are follow-up work, not a property to claim for v1.

The owner now confirms all three lab hosts are connected to the Service. The
management host is not an API backend.

### Portable kubeconfig retrieval: selected, not implemented

The tailnet is the chosen administrator trust boundary. Permit all tailnet
members to SSH to cluster nodes through Tailscale SSH and retrieve kubeconfig;
declare both network grants and SSH policy accordingly. This supersedes the
earlier no-SSH design for the portable path. It does not require public port 22,
Azure Run Command, or a cloud-specific remote execution service.

An Ignition-installed host service on every control-plane node will configure
the API Service forwarder and publish `/etc/infestation/kubeconfig` from K3s's
managed `/etc/rancher/k3s/k3s.yaml`. Leave the original unchanged. The published
copy will preserve the CA and client credentials, use the shared TailVIP URL,
and retain verified TLS (the common certificate identity is supported for
existing profiles). Write atomically with mode 0600 and refresh after source
credential changes, including renewal on subsequent K3s starts. Publication
must not wait for TailVIP reachability or Service approval.

The workstation CLI will retrieve this prepared file over Tailscale SSH from
any available server, save it privately, and verify API access. Root SSH access
is acceptable within the selected tailnet trust boundary. QEMU's existing
private virtio retrieval can remain a local bootstrap/debugging mechanism.

Remove Blob Storage from kubeconfig delivery. Build the helper on each Flatcar node in a versioned, digest-pinned official
Go container, using a GitHub source tag resolved to an immutable commit by the
CLI. This avoids per-architecture helper release assets, a workstation artifact
server, and Azure Blob Storage in the portable path. The builder runs before
Tailscale enrollment, so it uses ordinary outbound connectivity. It installs a
native static executable atomically and checks the cached identity/checksum on
later boots. No compiler is installed on the host. Container image/source/module
downloads, cold-build time and disk usage are accepted prototype costs. Revisit
this after the mixed-cloud acceptance test; do not treat guest builds as a
finished artifact/update strategy.
No image or artifact may contain enrolled Tailscale state or node credentials.

Removing the storage account entirely also requires replacing the durable
Cockroach initialization marker and any Azure discovery/status uses. That
recovery design is still open: do not replace the durable create-only init
guard with a disposable single-VM file or permit automatic reinitialization
after an ambiguous outcome. Current deployments still use their existing
storage/bootstrap paths until this replacement is implemented and tested.

## Pod networking: native K3s integration selected

Decision: use K3s's native Tailscale integration and retire Cilium. Restore
kube-proxy for Services and keep CoreDNS. Do not build a custom PodCIDR agent.
This replaces the original blanket no-Flannel constraint: Flannel supplies the
local CNI/subnet orchestration, while Tailscale supplies the encrypted transport.
Do not select Flannel VXLAN or its separate native WireGuard backend.

Install and run `tailscaled` as a host service on **every** Flatcar API server and
worker, with durable identity/state. Install/enroll it before starting services
that depend on tailnet addresses, including CockroachDB peer networking. Do not
implement the node network using Kubernetes pods or rely on an in-cluster operator
for bootstrap. Only API servers additionally advertise `svc:k8s-api`.

K3s has experimental Tailscale integration via `--vpn-auth`/`--vpn-auth-file`.
Our pinned K3s v1.35.8+k3s1 configures upstream Flannel's generic `extension`
backend to run `tailscale set --accept-routes --advertise-routes=<subnets>`.
This delegates transport to Tailscale rather than adding VXLAN over it.
Validate the selected integration's lifecycle in disposable VMs. If it cannot
preserve independently managed host connectivity, record the specific blocker
and revisit the design rather than silently substituting a custom controller.

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
2. Implement pinned host-level Tailscale installation/enrollment and durable
   state on all Flatcar node roles. Configure K3s's native Tailscale integration,
   retire the Go bootstrap Cilium chart, and restore kube-proxy together. Retain
   generic DNS, Service, cross-node, and datastore smoke checks. Label previous
   Cilium validation historical; do not claim replacement networking verified.
3. Validate native K3s/Tailscale restart and source-IP behavior in a disposable
   profile without a shared private node network. Separately replace Azure's
   inbound API/gateway LB dependence and verify outbound access remains possible;
   preserve existing infrastructure/state until an explicit deployment migration.
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


## Mixed acceptance test: selected scope and completed prototype

Create a fresh three-server cluster with two independent local QEMU NAT VMs
and one Azure VM, all running Flatcar, CockroachDB and K3s. Their only shared
network is Tailscale; Azure placement in a common VNet is optional and must not
be relied upon for cluster communication. Azure still needs its ordinary NIC,
VNet/subnet and outbound connectivity plumbing; the intended dependency budget
excludes Blob Storage, Key Vault, managed databases, external ingress/load
balancers and cloud-specific remote execution. Use persistent disks and compute,
plus the required networking and the existing tailnet/source/container services.
Preserve the existing local lab and its credentials.

Use the source-container helper build and tailnet SSH/local kubeconfig delivery
outlined above. The static test uses the existing persistent-disk guard on the local initializer;
this is not evidence of replacement safety. At the owner's request it reuses
`svc:k8s-api` while the old cluster remains stopped. Concurrent clusters require
separate Services and disjoint ranges; do not resume the old advertisements.

Acceptance evidence: three Ready API nodes, healthy Cockroach members and zero
unavailable/under-replicated ranges after convergence, all six directed pod DNS
and ClusterIP HTTP checks, and API access through the test's shared Service with
normal TLS validation. Save the inventory and infrastructure resource list to
show that no storage account or external managed bootstrap service was used.

Record ten Tailscale DISCO pings for every directed node pair (six pairs), with
`--until-direct=false`, retaining first-ping behavior and per-reply path strings
(direct endpoints, DERP or peer relay). Save raw output, timestamps, requested
versus received sample counts, min/median/p95/max RTT and incomplete measurements.
This measures tailscaled-to-tailscaled connectivity, not pod or SQL request
latency; keep it alongside the application connectivity checks. A relay path is
an observation, not a failed cluster test or an assumed direct connection.

After the mixed cluster test, stop feature work and review what can be simplified
and made more resilient. Do not automatically expand into further cloud support
or failure-recovery implementation before that review.


### 2026-11-10 implementation and local latency baseline

Added opt-in `--helper-source-ref` to VM cluster creation and standalone Ignition
rendering. It resolves GitHub tags/revisions to commits and generates a host
container-build unit before enrollment/bootstrap. A real official Go 1.26.10
Bookworm container fetched commit `138c5676d497f665517d6fc67c1d5a0712e7a15e`
from GitHub and built the helper successfully with native amd64 and CGO disabled.
The default builder uses its multiarch manifest digest. Generated amd64/arm64
Ignition and bootstrap ordering tests pass; a fresh Flatcar boot of this path
and arm64 execution have not yet been exercised. Existing VMs were not migrated.

Added `smoke --tailnet-latency` and exercised it on the running three-local-VM
lab before shutdown. All six DNS/ClusterIP checks passed, and all six directed
ping jobs returned 10/10 samples. Every sample used `DERP(sea)` in this run.

| Direction (tail3-1109 suffix) | Min ms | Median ms | p95/max ms |
| --- | ---: | ---: | ---: |
| 0 → 1 | 6 | 8.5 | 10 |
| 0 → 2 | 6 | 8 | 155 |
| 1 → 0 | 6 | 8 | 20 |
| 1 → 2 | 6 | 8 | 21 |
| 2 → 0 | 7 | 8.5 | 14 |
| 2 → 1 | 6 | 7 | 13 |

With ten samples, nearest-rank p95 equals the maximum; this is a short diagnostic
baseline, not a performance characterization. Private raw evidence is retained
in `state/vms/tail3-1109/tailnet-latency-b584d14c-5fab-407f-9ae4-5ee9561bf84b.json`.
The subsequent mixed Azure/local test is recorded in [mixed-cluster.md](mixed-cluster.md).

At the owner's request, the existing local lab and standalone enrollment test
VMs were shut down after these checks. Preserve their disks, PKI, kubeconfig and
reports. API reachability is not expected while the lab is stopped.


### Mixed test completion, 2026-11-10

`mix-1110` is running with two local KVM/SLIRP VMs and one Azure westus2 VM.
Three API endpoints and three Ready nodes passed verification. All six directed
DNS/ClusterIP checks and 60/60 Tailscale ping samples completed; cross-cloud pings
were direct, median 8 ms. CockroachDB has three live/available members with zero
unavailable or under-replicated ranges. Kubeconfig copied over Tailscale SSH from
Azure works through the existing `k8s-api` Service. The resource group has only a
VM, disk, NIC, VNet/subnet and NSG; no storage account or public IP assignment.

This required live DNS and publisher-path repairs, plus Azure API/profile and
local retry fixes. The Azure VM reboot was premature; logs later proved join
retries were working and container DNS was wrong. The generator now sets
Cockroach container DNS to Quad100 in Tailscale profiles. This is a repaired
prototype, not a clean unattended bootstrap or recovery proof. The former lab
remains stopped. See [the full runbook and findings](mixed-cluster.md) before
reusing either cluster. Stop feature expansion here for the requested review.


The consolidated follow-up list is [networking-todo.md](networking-todo.md),
including replacement of the prototype Azure default outbound access with an
explicit egress design. Do not interpret the mixed-test success as selecting
default outbound access for future deployments.


Teardown completed after the mixed test: both local QEMU processes stopped and
Azure group `mix-1110-rg` no longer exists. The Azure managed disk was deleted
with the group; local disks, credentials and test evidence remain preserved.
See the consolidated TODO list for explicit egress and resilience follow-up.
