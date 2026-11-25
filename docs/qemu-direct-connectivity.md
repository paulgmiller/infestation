# Improving direct Tailscale connections for QEMU VMs

## Recommendation

Add an opt-in QEMU mode that forwards one host UDP port per VM to the guest's
Tailscale listener and advertises the corresponding host LAN endpoint from that
guest. Keep SLIRP isolation, ordinary Tailscale NAT traversal and relay fallback.
This targets direct connections between VMs on reachable LAN hosts without
requiring changes to the home router.

Treat bridged networking as a separate optional backend. It simplifies local
endpoint discovery and removes QEMU's user-mode NAT from the packet path, but
needs host network setup and a guest firewall to preserve Tailscale as the only
way into applications. It should not become the default for this requirement.

These are proposed changes, not implemented behavior. Creating this document
does not change or restart any VM. The cluster must continue to work across
independent networks; a local direct path is an optimization, not a new shared
underlay requirement.

## Current behavior and observed limits

Cluster QEMU creation selects SLIRP in
[`internal/vm/provider.go`](../internal/vm/provider.go). QEMU arguments in
[`internal/vm/vm.go`](../internal/vm/vm.go) use `user,id=egress`, without host
forwards. The experimental passt path explicitly disables inbound TCP and UDP
forwards; merely changing the backend name does not establish reachability.

[`internal/ignition/tailnet.go`](../internal/ignition/tailnet.go) configures a
guest Tailscale listener on UDP 41641. Current host metadata records the network
backend, but has no saved bind address, forwarded port or advertised endpoint.
The tests currently require the absence of host forwards and TAP interfaces.

In the preceding read-only investigation, beeback-0 had guest address
`10.0.2.15/24` and gateway `10.0.2.2`. Its Tailscale netcheck reported UDP working,
`MappingVariesByDestIP: false` and no detected port-mapping service. Three probes
to each basementquiet peer remained on Seattle DERP. This demonstrates failure
to establish those direct paths at that time; it does not identify which NAT or
firewall behavior prevented them. Stable STUN mappings alone do not prove peer
reachability.

The two NAT layers serve different purposes:

```text
Internet peer → home router NAT → host LAN address → QEMU NAT → guest
LAN peer ──────────────────────→ host LAN address → QEMU NAT → guest
```

For hosts on the same reachable LAN, a host LAN endpoint avoids the router's NAT
entirely. Using the router's public endpoint from inside the home instead needs
NAT hairpinning, whose availability cannot be assumed. Tailscale already tries
NAT traversal and periodically retries direct connectivity. See
[Tailscale connection establishment](https://tailscale.com/docs/reference/connection-types)
and [NAT traversal and hairpinning](https://tailscale.com/blog/how-nat-traversal-works).

## Changes for isolated guests with UDP forwarding

### Persist a narrow host forwarding configuration

Extend the QEMU configuration and saved per-instance machine metadata with an
explicit bind address, an allocated host UDP port and the fixed guest transport
port. Illustrative mappings on one host are:

```text
192.168.1.20:41642/udp → VM A 10.0.2.15:41641/udp
192.168.1.20:41643/udp → VM B 10.0.2.15:41641/udp
```

Each guest can retain the same private address because its SLIRP network is
separate. Each host address and UDP port combination must be unique, including
avoiding the host's own tailscaled listener. QEMU supports the following form:

```text
-netdev user,id=egress,hostfwd=udp:192.168.1.20:41642-10.0.2.15:41641
```

Bind to the selected LAN address rather than all interfaces. Open that UDP port
in the host firewall for the intended source networks. Do not forward SSH, SQL,
Kubernetes API, bootstrap or pod application ports. The exposed listener handles
Tailscale's encrypted transport and discovery; application access still depends
on Tailscale identity and policy. QEMU documents host address binding and UDP
forwarding in its [network options](https://www.qemu.org/docs/master/system/qemu-manpage.html).

Allocate and save ports under the host lifecycle lock. A preliminary availability
check does not eliminate a bind race: QEMU's successful bind is authoritative.
Report a collision without stealing a port or silently changing an existing
VM's saved endpoint. Stop releases the socket but retains the assignment; remove
releases the owned assignment. Remote QEMU calls must select addresses and check
ports on the saved SSH compute host, not on the creator workstation.

### Advertise the forwarded endpoint from the guest

Deliver the host endpoint into a private guest configuration and add it to
Tailscale's advertised transport endpoints. For the first example, the relevant
configuration field is:

```json
"StaticEndpoints": ["192.168.1.20:41642"]
```

This is a fragment, not a complete replacement configuration. Infestation's
current Tailscale pin is v1.104.2. That version defines `StaticEndpoints` in its
`alpha0` configuration format, separately from peer-relay endpoints; the
transport implementation adds these to discovered endpoints. Use this pinned
mechanism rather than relying on historical debug environment variables. See
[v1.104.2 configuration types](https://github.com/tailscale/tailscale/blob/v1.104.2/ipn/conf.go)
and [endpoint advertisement](https://github.com/tailscale/tailscale/blob/v1.104.2/wgengine/magicsock/magicsock.go).

Integrate configuration-file loading with the existing enrollment helper before
enabling this mode. The format defaults to locking configuration changes, while
[`internal/tailnet/prepare.go`](../internal/tailnet/prepare.go) calls `tailscale up`
and `tailscale set`; K3s also manages pod-route advertisement. Explicitly allow
the required mutable settings and test that loading or reloading endpoints
preserves hostname, tags, DNS, SSH, pod routes, SNAT settings and API Service
configuration. Preserve the existing Tailscale state and node identity. Do not
copy raw daemon preferences, auth keys or CA signing keys into host metadata.

Keep host endpoints in QEMU provisioning/runtime data. Shared node preparation
may accept an optional list of transport endpoints, but must not require QEMU,
LAN connectivity or host forwarding for Azure or other providers.

### Handle address changes without router administration

Use an explicitly selected host address initially. Automatic selection should
require an unambiguous interface and exclude loopback, container bridges and
tailnet addresses. Do not assume a DHCP reservation can be configured: router
administration is outside our control.

Check the address at start and report when it changes. A later host reconciler
can rebind the forward and update the guest's endpoint through an authenticated
channel. That delivery must also work during loss of the old direct path, using
relay access or a private host-to-guest channel. Publish only endpoints backed by
a working listener and remove stale hints after replacement. Initial support
can require an explicit refresh rather than pretending a fixed address follows
DHCP automatically.

Check that the guest is actually listening on the configured transport port.
An added endpoint is a candidate, not an instruction to bypass Tailscale's path
selection or disable relay fallback.

## Improving connections to peers outside the LAN

The local forward handles the inner NAT. It does not create a mapping on the home
router. For outside peers, retain Tailscale's automatic discovery and hole
punching; a LAN endpoint is simply another candidate for peers that can reach it.

A possible second phase is a host-side NAT-PMP, PCP or UPnP client, if the router
already offers one. It would request an outer UDP mapping to the allocated host
port, renew the lease, discover changes to the public address and port, and
deliver that additional endpoint to the guest. It cannot enable a disabled
router service, bypass router policy or guarantee traversal through upstream
carrier NAT. Port-mapping protocols generally do not configure both NAT layers
automatically. See [Tailscale on double NAT](https://tailscale.com/docs/reference/faq/ipv6).

This is more work than enabling a guest setting. The full chain must be:

```text
public address:router port → host LAN address:host port → guest:41641
```

Verify replies as well as inbound delivery. Do not assume QEMU's `hostfwd` makes
every outbound packet use the forwarded host source port, or that STUN will
discover the manually mapped endpoint. A canary must establish the actual
bidirectional behavior before building automatic router-mapping management.

Keep this optional and disabled by default. The first implementation should
solve local reachability without adding a router dependency. A dynamic public
IP is manageable when discovered endpoints and mapping leases are refreshed;
hardcoding it is not. For same-LAN peers, prefer the LAN candidate so public
hairpin support remains unnecessary.

## Comparison with a bridge onto the LAN

Here, bridge means a TAP connected to the host's physical LAN bridge. A private
host-only bridge behind host NAT is a different design and still needs NAT
traversal or forwarding for connections from other hosts.

| Consideration | SLIRP with UDP forward and endpoint | Bridge onto the LAN |
| --- | --- | --- |
| Local direct path | Host LAN endpoint forwards into each guest | Guest advertises its own reachable LAN endpoint |
| Router changes | None for local peers | None for local peers |
| Outside peers | Still cross two NAT layers; automatic traversal remains conditional | Removes host NAT; home-router behavior still matters |
| Endpoint management | Host address, unique ports and guest hints must stay synchronized | Guest address discovery is automatic; DHCP and LAN reachability still matter |
| Application isolation | Only explicitly forwarded Tailscale UDP enters through SLIRP | Must enforce guest firewall rules before attaching to LAN |
| Host setup | Unprivileged QEMU port bind; firewall permission may be needed | TAP/bridge setup and narrowly scoped privileged helper or preconfigured interfaces |
| LAN requirements | Hosts need usable unicast reachability | LAN must accept guest MAC addresses and provide usable addressing |
| Performance expectation | Removes DERP when successful; retains SLIRP overhead | Removes DERP when successful and avoids user-mode NAT overhead |
| Existing disks and identity | Reconfigure networking while preserving both | Reconfigure networking while preserving both |
| Failure handling | Stale hints or bind failures; discovered paths and relays remain available | Bridge, DHCP or firewall failure can remove guest connectivity entirely |

QEMU documents TAP and bridge helpers in its
[bridge backend options](https://www.qemu.org/docs/master/system/qemu-manpage.html).
Neither option guarantees connectivity across LAN client isolation, VLAN
boundaries or restrictive host firewalls. A bridge also depends on the physical
interface and network accepting multiple guest MAC addresses; Wi-Fi setups may
need a different arrangement. Avoid automatically restructuring the host's
management interface.

Bridging can retain Tailscale-only application access, but only with explicit
filtering. Current CockroachDB generation uses `--listen-addr=0.0.0.0:26257` in
[`internal/ignition/generate.go`](../internal/ignition/generate.go). TLS protects
authentication but is not a substitute for keeping the SQL port off the LAN.
Audit K3s listeners, NodePorts and forwarded pod traffic too. Before enabling a
bridge, install persistent rules allowing the required DHCP, neighbor discovery,
egress replies and Tailscale UDP transport, while rejecting unsolicited
application input and forwarding from the physical NIC. Cover IPv4 and IPv6,
preserve tailnet pod routing and verify interaction with K3s/Docker rules.

Expect better locality and potentially lower forwarding overhead, not a promised
CPU reduction. Tailscaled still encrypts traffic in both modes. Compare process
CPU over the same interval and traffic volume; removing a throughput bottleneck
can increase total CPU by allowing more useful traffic.

## Suggested implementation and verification sequence

1. Extend diagnostics to report selected peer endpoints, direct versus DERP or
   peer-relay paths, configured forwards and address mismatches. Keep output
   targeted; do not dump daemon preferences or credentials.
2. Add explicit, opt-in saved UDP forwarding and guest endpoint configuration.
   Keep current SLIRP behavior when it is absent. Exercise enrollment and K3s
   route updates with the pinned Tailscale configuration format.
3. Test one existing VM as a separately authorized canary. Change only network
   configuration; preserve its disk, ownership, Tailscale state and cluster
   credentials. If a restart is needed, schedule it through the existing safe
   lifecycle procedure rather than restarting the group.
4. Validate local direct paths before deciding whether host-side router mapping
   or an optional bridge backend is worth its additional machinery.

Local tests should cover port ownership and collisions, restart persistence,
remote-host selection, address validation, QEMU argument construction, absence
of application forwards, configuration mutability, identity preservation and
rollback. Run `go test ./...` and `go vet ./...` for an implementation; mocks do
not establish that UDP forwarding or NAT traversal works on the live hosts.

The live acceptance matrix should include:

| Scenario | Required observation |
| --- | --- |
| VMs on different LAN hosts | Both directed probes reach a host LAN endpoint directly |
| Two VMs on one host | Both directions work through the host endpoint; do not assume SLIRP loopback behavior |
| Host firewall blocks the new port | Useful diagnostic and working relay fallback |
| VM stop and start | Same saved endpoint and node identity; no unrelated process disturbed |
| Host address changes | Explicit stale-address detection and successful endpoint refresh |
| Peer outside the home | Record direct or relayed result; local success is not evidence of outside success |
| LAN client without Tailscale | Cannot reach SQL, API, bootstrap, SSH or pod applications through the new configuration |
| Kubernetes and database traffic | Existing directed pod checks, DNS, database health and API access continue working |

For each networking option, record latency, packet/byte rate and synchronized
tailscaled, QEMU and K3s CPU samples under comparable load. Store live test
evidence in a runbook. Leave this document as the design comparison and keep
unimplemented tasks in the outstanding-work files when implementation is chosen.
