# Project goals

These capture the user's stated direction. They are design goals, not a claim
that all behavior is implemented or proven. Outstanding work is in [TODO.md](../TODO.md).

## Connectivity and dependencies

- Nodes should need only outbound connectivity. No inbound firewall opening,
  router port forwarding or externally reachable node API/SSH endpoint should
  be required for the cluster to function.
- Cluster communication must not depend on shared or connected networking:
  no common VNet/VPC, peering, bridged local network or site-to-site underlay VPN.
  Azure VMs may share a VNet, but the cluster must work when they do not share
  connectivity with the other nodes. A VM provider may still require a NIC and
  VNet/subnet as ordinary provisioning plumbing.
- Avoid required external/cloud load-balancer resources for cluster traffic or
  ingress. A stable private API
  endpoint should be available through Tailscale with Kubernetes TLS preserved.
  Public application ingress should also be able to use outbound tunnels; its
  high-availability design remains open.
- Keep required cloud services minimal: compute, persistent disk and the
  networking needed for egress. Do not require cloud object storage for helper
  delivery, kubeconfig retrieval or ordinary cluster bootstrap.
- Explicit egress infrastructure is a tradeoff to resolve, not an exemption to
  hide. Azure default outbound access was a prototype shortcut, not the target
  egress design. An egress proposal that introduces a load-balancer dependency
  must surface that tradeoff rather than silently changing the goal. See [networking TODOs](networking-todo.md).

## Portability and simplicity

- Keep a small, consistent CLI: `infestation <provider> create` always creates
  CockroachDB and K3s. Servers are the default; `--agent` omits the API server
  and cannot combine with `--init`. Providers share lifecycle verbs. Connectivity
  checks belong in a separate kubeconfig-driven CLI, independent of provisioning.
- Support one cluster per tailnet for now. Use conventional API/bootstrap
  endpoints so friends need no additional cluster selector or global node list.
- Discover arbitrarily named participants through the tailnet. For now, designate
  one founding node with `--init`; it automatically initializes once two peers
  are ready, or joins an existing database. Defer automatic election until it can
  distinguish an unavailable existing cluster from a new one.
- Prefer explicit removal and fresh creation over repairing partially provisioned
  infrastructure. Failed creates retain resources/state for inspection; cleanup
  must respect resource ownership, existing disks and cluster identity.
- Provision compute from exactly one provider per command. Mixed clusters use
  separate independent calls, such as local `--init --size 2` and Azure `--size 1`.
- Support any compatible VM compute provider. QEMU and Azure are the current
  providers; other clouds, hosts and friends' machines should use the same node
  contract. Use direct Go libraries: Azure Go SDK for cloud management and
  go-qemu for local QMP, with QEMU processes and disks managed from Go.
- Make it simple to give friends the repository/binary and an enrollment key so
  they can create a VM on the supplied tailnet. Accept a command argument or
  environment variable as well as a private key file.
- Allow the creator to start nodes across hosts/providers, then let friends
  independently add VMs using tailnet enrollment details and the cluster
  name/endpoint. No complete shared node inventory or central edit is required.
  Each provider group owns its own resources; cluster membership is discovered.
- Prefer standard Flatcar images and shared Ignition/bootstrap logic. Build the
  helper natively in a Go container from a pinned source revision so deployment
  does not require a custom image or published helper binary per architecture.
  Revisit this distribution choice after measuring its operational costs.
- Automate future node enrollment, pod-route approval and API Service host
  approval using scoped tags/policy, rather than approving every node manually.
- Validate the mixed local/cloud topology before adding features, then pause to
  simplify the implementation and address resilience findings.

## Access, availability and evidence

- Creator workstations must be disposable once provisioning and credential
  handoff complete. Cluster operation, enrollment, credential renewal, safe
  decommissioning and disaster recovery must not depend on their continued
  availability or retained local files. Durable credential custody and recovery
  access must exist elsewhere, including when the cluster itself is unavailable.
  Recoverable provider ownership records must allow another authorized operator
  to manage cloud resources. Local QEMU compute still depends on its physical
  host; losing a creator workstation is distinct from losing a VM host and disks.
  This is a design goal; workstation-only signing/admin custody remains a current
  implementation limitation pending the credential-store design.
- Treat the tailnet as the administrator security boundary. Tailnet members
  should be able to access SSH and retrieve kubeconfig; routine sessions use
  `core` with sudo.
- Give the Kubernetes API a shared tailnet Service/DNS endpoint advertised by
  control-plane servers. Keep normal Kubernetes CA, hostname and client
  authentication checks; forward raw TCP to each local API.
- Keep kube-proxy for ordinary Services and CoreDNS for Kubernetes DNS. Give
  each node a route for its API-assigned PodCIDR through native K3s networking.
- Measure directed Tailscale ping latency between nodes, record direct versus
  relay paths, and retain raw results alongside pod DNS/HTTP and database health.

## Open requirements and accepted prototype limits

NetworkPolicy enforcement was requested for investigation and remains undecided.
Same-node pod traffic is not encrypted by the host overlay; this was identified
as a possible acceptable tradeoff, not settled as a permanent security contract.
High availability during API/process failure, safe replacement, updates and
restore are goals still needing design and tests.

The [operations plan](operations-plan.md) also records earlier targets: tolerate
one availability-zone loss, and support operator-triggered recovery after a
larger outage within one hour with at most fifteen minutes of data loss. Those
are unproven targets; backup storage, dataset size and recovery throughput remain
open. They should not be confused with the short connectivity test results.
