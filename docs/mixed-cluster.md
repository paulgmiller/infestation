# Mixed local/Azure cluster test

The 2026-11-10 test reached three Ready K3s servers and three healthy CockroachDB
members: two independent local QEMU/SLIRP VMs and one Azure VM. Application
communication uses Tailscale; no connected underlay between the sites is needed.
Teardown was requested after collecting evidence; see the teardown checkpoint
below for its verified state. This was a repaired prototype
run, not a verified unattended fresh deployment or failure-recovery test.

## Access the running test

```sh
export KUBECONFIG="$PWD/state/vms/mix-1110/kubeconfig"
kubectl get nodes

# The same configuration, prepared on Azure and copied over Tailscale SSH:
export KUBECONFIG="$PWD/state/vms/mix-1110/kubeconfig.from-azure"
kubectl get nodes

tailscale ssh core@mix-1110-2.quoll-duck.ts.net
```

Both kubeconfigs use `https://k8s-api.quoll-duck.ts.net:443`, retain the cluster
CA/client credentials and verify `kubernetes.default.svc.cluster.local`. The
node-published copy is `/etc/infestation/kubeconfig`, owned by root, mode 0600.
Retrieve it with `sudo cat` through Tailscale SSH; never print it into logs.
Routine sessions use Flatcar's `core` user with sudo. Standard sshd remains
masked. Unrestricted sudo still provides full administration.

The existing `svc:k8s-api` and its auto-approver are deliberately reused, at the
owner's request. The old `tail3-1109` cluster remains stopped with disks preserved.
Do not start that cluster while it advertises the same Service: first retire
its advertisements or choose separate Service identities. Its pod ranges also
overlap the current test, so concurrent use requires a separate CIDR design.

## Reproduce with a fresh name

Complete the [tailnet policy setup](tailnet-policy.md), including traffic grants,
before running the commands below.

Prerequisites: QEMU/KVM, the CLI, an authenticated Azure CLI, and a reusable,
preauthorized `tag:roach` enrollment key. The tailnet policy needs pod-route
approval for `10.42.0.0/16`, Service approval for `svc:k8s-api`, network grants,
and SSH access for members to `tag:roach` devices. The tested member SSH rule is:

```json
{
  "action": "accept",
  "src": ["autogroup:member"],
  "dst": ["autogroup:self", "tag:roach"],
  "users": ["autogroup:nonroot", "root"]
}
```

The independent enrollment flow uses two calls and distinct group names. These
can run on different workstations; only tailnet enrollment details are shared.
Source defaults to the published `master` branch, which must contain the new
enrollment helper; override `--helper-source-ref` to select a published revision.
This flow still needs fresh VM verification.

```sh
bin/infestation vm create --name local-new --state-dir state/local \
  --init --size 2 --tailnet-domain quoll-duck.ts.net \
  --tailscale-auth-key-file secrets/tsauth

bin/infestation azure create --name cloud-new --state-dir state/cloud \
  --size 1 --tailnet-domain quoll-duck.ts.net \
  --tailscale-auth-key-file secrets/tsauth
```

The first call creates `local-new-0/1`; only `local-new-0` gets `--init` and the
one-hour SQL bootstrap credential. The second creates `cloud-new-0` with no local
CA/token or knowledge of the first call’s inventory. After Tailscale enrollment,
it obtains shared leaf/join credentials from a peer’s helper on port 7444.
Tailnet policy must permit that port between `tag:roach` peers.

Azure region and SKU default to `westus2` and `Standard_B2s`. Creation returns
after provisioning; from a tailnet-connected workstation, use `--wait 30m` or
`vm wait` with that workstation’s group name. The first three database
participants must arrive within the initializer credential’s one-hour lifetime;
expiry requires explicit recovery. See [bootstrap](bootstrap.md).

The historical run below used source commit
`138c5676d497f665517d6fc67c1d5a0712e7a15e` and a static three-node inventory. Its
latency/topology evidence does not verify independent enrollment.

The Azure resource group is `cloud-new-rg`. Its resources are one VM, one
managed OS disk, a NIC, a VNet/subnet and an NSG. It contains no storage account,
load balancer, public IP resource, managed identity, private DNS zone, or remote
execution extension. Boot diagnostics are disabled. The subnet explicitly opts
into Azure default outbound access; it uses Azure-provided egress NAT and is not
an isolated network without internet. The NSG denies unsolicited inbound traffic,
including VNet traffic. A successful direct Tailscale path uses NAT traversal and
stateful return traffic, not an inbound API/SSH listener on a public address.
This outbound mechanism is an explicit prototype shortcut to replace. See the
[prioritized egress TODO](networking-todo.md#before-calling-deployment-unattended)
and [Azure retirement scope](https://learn.microsoft.com/en-us/azure/virtual-network/ip-services/default-outbound-access).
New-subnet defaults changed; existing default outbound access is not subject to
a blanket shutdown. No load balancer was used in this test.

Azure requires an OS login credential in this VM creation API. The CLI generates
an RSA public key, supplies it in the OS profile, and never serializes the private
half. This satisfies Azure validation; Tailscale SSH handles real access.

A failed creation preserves profiles, disks and provider metadata for inspection.
There is no partial-provisioning repair command. Creation refuses existing state;
plan explicit cleanup and a fresh create rather than rewriting an incomplete
deployment. Preserve existing data, CA/token and failure evidence; never
initialize another database merely because existing peers are unavailable.

## Actual evidence and repairs

| Node | Placement | Tailnet IPv4 | Runtime Flatcar |
| --- | --- | --- | --- |
| mix-1110-0 | Local KVM, independent SLIRP NAT | 100.74.172.16 | 4757.2.2 |
| mix-1110-1 | Local KVM, independent SLIRP NAT | 100.103.14.101 | 4757.2.2 |
| mix-1110-2 | Azure westus2, Standard_B2s | 100.115.196.35 | 4757.2.3 |

All run K3s v1.35.8+k3s1 and CockroachDB v26.2.7. Azure was provisioned from image
4757.2.2 but booted a staged Flatcar update after the manual restart; a pinned
image does not pin the subsequent running OS. Azure's NIC private IP is
192.168.240.4 with no public IP assignment. Peer route checks in both directions
show `tailscale0`. The recorded Azure resource list contains exactly the five
resource types described above.

The run needed these interventions, now reflected in source:

- Azure initially rejected a Linux profile with no login credential. Added the
  disposable public key and retried the existing deployment.
- A retry used the wrong cached image filename. Corrected it before launching
  the local nodes; the Azure VM and PKI were preserved.
- Azure CockroachDB kept retrying join RPCs every second, but Docker selected
  Azure DNS (`168.63.129.16`), which could not resolve tailnet hostnames. The
  host resolved them correctly. A premature VM restart did not fix it. Added
  `--dns=100.100.100.100` to Tailscale Cockroach profiles and applied that setting
  to the Azure container, restarting only CockroachDB. It then joined normally.
  [Quad100](https://tailscale.com/docs/reference/quad100) is the local Tailscale
  resolver; Docker DNS settings must be explicit when host split DNS matters.
- SSH policy initially allowed only `autogroup:self`; the owner added
  `tag:roach` as a destination. All three nodes use the same SSH policy.
- The kubeconfig publisher used `/usr/bin/k3s`, but this sysext installs K3s at
  `/usr/local/bin/k3s`. Corrected the generator and live publisher scripts.

After repair, three APIs passed readiness and all three Kubernetes nodes were
Ready. CockroachDB reported three available/live members in local and Azure
localities, 62 ranges per member, and zero unavailable or under-replicated ranges.
All six directed pod DNS/ClusterIP HTTP Jobs completed; three initially retried
connection-refused errors while web endpoints were starting. CoreDNS,
metrics-server and local-path-provisioner were Running.
The Azure-published kubeconfig was copied over `core` + sudo and passed both
node listing and `/readyz` through the shared Service with normal TLS validation.

Latency evidence: `tailnet-latency-5cae10fe-a769-49e4-b08e-4417f33c7411.json` in
private `state/vms/mix-1110`. Every directed pair returned 10/10 DISCO replies:

| Direction | Path | Median ms | p95 ms |
| --- | --- | ---: | ---: |
| Local 0 → Local 1 | DERP(sea) | 8 | 9 |
| Local 1 → Local 0 | DERP(sea) | 8 | 9 |
| Local 0 → Azure | Direct | 8 | 13 |
| Azure → Local 0 | Direct | 8 | 8 |
| Local 1 → Azure | Direct | 8 | 9 |
| Azure → Local 1 | Direct | 8 | 10 |

These are short host-overlay measurements, not pod/SQL latency, throughput or
availability guarantees. Ten-sample nearest-rank p95 equals the maximum.

## Stop here for the simplification and resilience review

The consolidated [review TODO list](networking-todo.md) tracks the work below,
including explicit Azure egress.

The node-0 initialization marker is on its persistent disk, not Blob Storage or
a distributed guard. No initializer replacement, multi-disk-loss recovery or
quorum-loss/reinitialization scenario was tested. Preserve that state. Removing
the Azure Blob dependency for this static topology does not solve recovery safety
for a dynamically replaced initializer.

Review next: reduce duplicate bootstrap/access paths; make DNS assumptions
explicit; define durable initialization/replacement safety; distinguish liveness
from API readiness for Service withdrawal; coordinate Flatcar updates; improve
failed-build diagnostics and cold-build distribution; gate smoke clients on
ready endpoints. Kine logs also contain unsupported `pg_total_relation_size()`
maintenance-status warnings against CockroachDB despite successful API checks.
Investigate their operational impact during that review. These are findings,
not additional feature work authorized by this test.

To stop compute while preserving disks and evidence:

```sh
bin/infestation vm stop --name mix-1110 --state-dir state/vms
```

This stops both local VMs and deallocates Azure compute. The Azure resource group
and managed disk remain until explicitly removed.


## Teardown checkpoint

At the owner's request, both local QEMU VMs were gracefully shut down and their
processes verified absent. Azure resource group `mix-1110-rg` was deleted;
`az group exists` returned false after its resource list became empty. The Azure
VM, managed OS disk, NIC, VNet/subnet and NSG have been removed. Local disks,
PKI, kubeconfigs and raw latency/verification reports remain private for reference.
Tailnet device/Service registration cleanup is a separate TODO; do not start
old local nodes into the reused Service without reviewing those advertisements.
