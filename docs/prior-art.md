# Prior art

Comparison reviewed on 2026-11-12. These are architectural references, not tested
replacements or claims of equivalent availability. Infestation currently combines
Flatcar VM preparation, independent tailnet enrollment, K3s native Tailscale
networking and host-level CockroachDB backing Kine. Contributor-aware placement
and verified failure survival remain outstanding work in [TODO.md](../TODO.md).

## Closest related projects

The closest overall architectural relative in this comparison is **Talos Linux
with KubeSpan**. **Liqo** is especially relevant to independently contributed
capacity, but uses separate Kubernetes clusters rather than one shared cluster.
These rankings are an assessment of fit to Infestation's [goals](goals.md).

| Project | Shared ideas | Important boundary |
| --- | --- | --- |
| [Talos Linux](https://github.com/siderolabs/talos) with [KubeSpan](https://docs.siderolabs.com/talos/v1.12/learn-more/kubespan) | Kubernetes-focused OS, declarative node configuration, peer discovery and automated WireGuard networking across locations. | KubeSpan is integrated into Talos. Talos uses an etcd control plane; Infestation uses Flatcar, Tailscale and CockroachDB/Kine. |
| [Liqo](https://github.com/liqotech/liqo) | Resource sharing across independently managed on-premises, cloud and edge infrastructure. | Peering links independent Kubernetes clusters and exposes remote capacity through virtual nodes. Infestation enrolls individual VMs into one cluster. |
| [Kilo](https://github.com/squat/kilo) | WireGuard networking for Kubernetes nodes across clouds and locations. | A network component rather than the full VM/bootstrap/database lifecycle. Its installation instructions require opening the WireGuard port. |
| [K3s distributed networking](https://docs.k3s.io/networking/distributed-multicloud) | Native Tailscale VPN integration for nodes without a common private network. | This is already Infestation's network foundation. Infestation adds provider lifecycle, Flatcar preparation, independent credential enrollment and CockroachDB/Kine bootstrap. |

Talos is useful prior art for node lifecycle and discovery. Its
[edge reference architecture](https://www.siderolabs.com/edge-kubernetes-cluster-reference-architecture-for-omni-and-talos-linux/)
describes the etcd-based control plane. Talos Linux is MPL-2.0; the related Omni
management product is separately source-available under BSL 1.1, according to
[Sidero's licensing overview](https://www.siderolabs.com/talos-linux).

Liqo's [peering model](https://docs.liqo.io/en/stable/features/peering.html)
establishes a directional resource-consumption relationship between clusters.
Using one cluster per contributor would change the control-plane failure and
administrative boundaries: a contributor could withdraw its cluster without
being a member of another cluster's datastore quorum. That is an architectural
alternative to evaluate, not a change to Infestation's current design or a
guarantee of application availability after withdrawal.

## Tailscale alternatives

These candidates address mesh networking and coordination rather than the whole
Infestation stack. The descriptions below reflect their repository documentation
on the review date; none has been integrated or live-tested with Infestation.

| Project | Discovery and membership | Relevance and limits |
| --- | --- | --- |
| [wgmesh](https://github.com/atvirokodosprendimai/wgmesh) | Shared-secret mesh enrollment with decentralized DHT discovery; also offers a centralized SSH deployment mode. | Automates WireGuard peers and routes, and supports subnet advertisements that could carry pod CIDRs. Its README advertises NAT traversal and exposes relay/NAT metrics; multiple-home-NAT connectivity and relay fallback still need independent verification. |
| [Bramble](https://github.com/WhyAsh5114/bramble) | WireGuard peer authorization through an ENSv2 registry, with device identity, revocation and scoped service grants; peers independently read the registry. | Explores an alternative to coordinator-authoritative membership. Its current design uses Sepolia RPC, Tor for control-plane reads, and relay discovery/payments involving ENS and Hedera/x402. Those are different external dependencies, not elimination of coordination dependencies. |
| [Webmesh](https://github.com/webmeshproj/webmesh) | Distributed WireGuard mesh management with extensible storage and authentication providers, designed without a single controller or database. | Its README describes direct links, ICE/WebRTC connections and libp2p circuit relays. Offers a host daemon, embeddable Go APIs and a separate Kubernetes CNI project; outbound-only home-NAT connectivity and Infestation integration remain unverified. |

wgmesh is relevant to simple independent enrollment without a dedicated
coordination server. Its shared-secret enrollment model does not by itself
establish the administrator-controlled contributor identity proposed for
Infestation. Membership revocation, contributor identity and replacement of
Tailscale tags would need explicit design. See its
[README](https://github.com/atvirokodosprendimai/wgmesh#readme).

Bramble is MIT-licensed. Its README distinguishes implemented behavior from
planned features and open acceptance checks: fresh-device connectivity and the
STUN/hole-punching/automatic-fallback workstream are not presented as fully
accepted. It also documents that prolonged RPC unavailability removes admitted
peers after a stale-data timeout. Treat it as exploratory prior art for identity,
authorization and relay design, rather than a verified replacement. See its
[status and dependency discussion](https://github.com/WhyAsh5114/bramble#readme).

Webmesh is Apache-2.0 licensed and supports integration through `webmeshd` or
embedding its Go package. Its distributed control plane is relevant to reducing
dependence on a hosted coordinator, but still needs assessment of membership,
storage consistency and failure behavior. Its separate CNI does not establish
compatibility with Infestation's existing native K3s/Flannel setup. See the
[Webmesh README](https://github.com/webmeshproj/webmesh#readme).

For each candidate, a replacement assessment must cover outbound-only
connectivity across independent home NATs, stable node addressing and DNS,
pod-route distribution, API endpoint failover, contributor identity/revocation,
and control-plane outages. K3s's native Tailscale integration would also need an
alternative integration; a working WireGuard mesh alone does not establish
equivalent cluster behavior.

## Is KubeSpan independently reusable?

**The node-side implementation is open source, but KubeSpan is a Talos feature,
not a documented standalone agent for arbitrary Linux distributions.**

The [Talos v1.12 KubeSpan design](https://docs.siderolabs.com/talos/v1.12/learn-more/kubespan)
describes peer discovery, endpoint rotation, and coordinated WireGuard,
nftables and routing configuration. In Talos v1.14.5 its controllers live under
`internal/app/machined/pkg/controllers/kubespan`; the
[package documentation](https://pkg.go.dev/github.com/siderolabs/talos@v1.14.5/internal/app/machined/pkg/controllers/kubespan)
identifies them as controllers for the Talos feature. Talos's
[source license](https://github.com/siderolabs/talos/blob/main/LICENSE) is MPL-2.0.
The internal package location and integration imply that reuse outside Talos
would require extraction/adaptation, not simply importing a supported public
library or installing a KubeSpan service on Flatcar. No standalone deployment
path was established in this review.

Discovery is separately packaged:

- [Discovery Service](https://github.com/siderolabs/discovery-service) is a
  separately deployable server for exchanging encrypted membership/peer data.
  Its current [license](https://github.com/siderolabs/discovery-service/blob/main/LICENSE)
  is BSL 1.1, with no additional use grant and a stated MPL-2.0 change date of
  2030-10-05. This current server version is source-available, not open source.
- [Discovery Client](https://github.com/siderolabs/discovery-client) and
  [Discovery API](https://github.com/siderolabs/discovery-api) are separate
  MPL-2.0 projects. They supply discovery interfaces, not the complete node-side
  WireGuard mesh agent.

These license observations refer to the reviewed versions; do not assume all
historical server releases have the same terms.

KubeSpan also should not be assumed to provide Tailscale-equivalent traversal
and relay behavior. In a
[2024 maintainer explanation](https://github.com/siderolabs/talos/discussions/9237),
each node pair needed direct one-way reachability, and two separate networks
behind NAT were identified as a limitation. That historical explanation does
not establish current behavior; any replacement evaluation must verify the
exact version against Infestation's outbound-only, multiple-home-NAT requirement.

For Infestation, KubeSpan is useful design prior art. A switch would require
separate portability, connectivity and lifecycle evaluation; this comparison
does not propose replacing the current host Tailscale/native K3s integration.
