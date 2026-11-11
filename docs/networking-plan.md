# Egress-only cluster networking

The goal is a cluster whose nodes need only outbound connectivity and do not
rely on a shared or connected underlay. Current implementations target local
QEMU and Azure; the architecture should work with any compatible VM compute
provider, preferably provisioned through Pulumi. See [project goals](goals.md).

## Component ownership

| Function | Component |
| --- | --- |
| Host enrollment, identity and encrypted peer transport | Tailscale on Flatcar |
| PodCIDR allocation and pod networking integration | K3s native Tailscale/Flannel |
| Ordinary Kubernetes Service routing | kube-proxy |
| Kubernetes service discovery | CoreDNS |
| Host database and Kine datastore | CockroachDB over TLS |
| Private API endpoint | Tailscale Service forwarding TCP to each local API |
| Operator access and kubeconfig retrieval | Tailscale SSH, `core` + sudo |
| Public application ingress | Outbound tunnel design still to be selected |

Each node advertises the PodCIDR assigned by Kubernetes. Nodes can be behind
independent NATs, with overlapping private underlay addresses. They communicate
using their tailnet identities, not underlay addresses. Same-node pod traffic is
not encrypted by the host overlay. NetworkPolicy enforcement remains undecided.

## Enrollment and approvals

Use a reusable, preauthorized tagged auth key for enrollment. The CLI accepts an
argument, environment variable or private file. Private Ignition contains the
key; guest enrollment removes the guest copy after success. Copies in local
profiles/provider state still need private handling.

Configure tag-scoped auto-approvers for pod routes and API Service hosts. Example
for the tested tag and default pod range, merged into existing policy:

```json
"autoApprovers": {
  "routes": {"10.42.0.0/16": ["tag:roach"]},
  "services": {"svc:k8s-api": ["tag:roach"]}
}
```

K3s allocates the per-node subnets. Approval does not allocate routes or grant
traffic access. The network policy must separately allow the required traffic.
Do not run concurrent clusters with overlapping pod ranges or accidentally share
an API Service between different clusters.

## Pod access from the tailnet

Ordinary tailnet clients can reach pod IPs through approved advertisements when
client route acceptance and traffic policy permit it. Linux clients explicitly
accept subnet routes with `tailscale set --accept-routes`. Verify the route and
actual pod address before testing; a pod address can change on rescheduling.
Pod CIDR advertisements do not advertise the Kubernetes Service CIDR.

## Private API and kubeconfig

Every server advertises `svc:k8s-api` on TCP 443, forwarding raw TCP to its own
`127.0.0.1:6443`. Worker-only nodes must not advertise the API Service. The shared
DNS name is configured through `api_tailnet_hostname`.

Kubeconfig retains the Kubernetes CA and client credentials. The tested endpoint
uses the shared Service URL and `tls-server-name: kubernetes.default.svc.cluster.local`,
which the API serving certificates cover. New profiles also add the shared
hostname SAN. Do not terminate Kubernetes TLS at the forwarder or bypass
certificate verification.

Each server writes a separate `/etc/infestation/kubeconfig` atomically with mode
0600 and refreshes it from the K3s-managed source. Publication does not wait for
Service approval or reachability. The tailnet is the administrator trust boundary;
SSH policy must allow members to reach the tagged nodes. Routine access uses
`core` with sudo.

A live Tailscale host is not evidence of a ready local API. Readiness-aware
withdrawal and client failover still need implementation and tests.

## Provider egress

There is no cluster requirement for a shared VNet, public node IP, inbound
firewall exception or ingress load balancer. A provider still needs to give each
VM a NIC and usable outbound access. Direct Tailscale paths can use NAT traversal;
relay paths must remain usable when direct connections cannot form.

The Azure prototype explicitly enabled default outbound access, with no assigned
public IP or load balancer. Replace that shortcut with an explicit egress design.
New Azure subnet defaults changed; this is not a blanket shutdown of existing
networks. The choices and verification work are in [networking TODOs](networking-todo.md).

Local QEMU supports SLIRP and passt for standalone enrollment. SLIRP requires no
host network setup. passt offers a different NAT/forwarding implementation but
requires the installed helper and working namespace/AppArmor permissions. Guest
netcheck and directed ping paths, rather than the backend name alone, determine
whether a particular pair establishes a direct connection. The mixed test used
independent SLIRP NATs; public ingress or host port forwarding was not required.

## Evidence and limits

The [mixed test record](mixed-cluster.md) preserves three-node database/API health,
six directed pod DNS/HTTP checks, all 60 ping replies, direct/relay paths and the
repairs needed during bootstrap. Cross-cloud median host RTT was 8 ms in that
short run. This is not pod/SQL latency, WAN performance or failure-recovery proof.

See [the local cluster runbook](isolated-vm-cluster.md) for commands and
[TODO.md](../TODO.md) for outstanding work.

## Independent enrollment

Each provider call creates only its own machines. After host enrollment, our Go
helper discovers `tag:roach` peers and exchanges join material on TCP 7444, bound
to a Tailscale IPv4 address. This helper endpoint is separate from K3s/Cockroach
and requires tailnet grants between cluster peers. It carries shared leaf keys
and the K3s token within WireGuard; CA signing and SQL root keys are excluded.
The same helper can publish kubeconfig to tailnet administrators. See
[bootstrap](bootstrap.md) for pinning, initialization guards and verification limits.
