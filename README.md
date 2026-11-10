# Infestation

Current priorities: [overall TODO](TODO.md); detailed [networking review](docs/networking-todo.md).

Infestation provisions Flatcar VMs running host-level CockroachDB and K3s over
Tailscale. The tested static path uses native K3s Tailscale/Flannel networking,
kube-proxy and CoreDNS. Nodes need outbound connectivity, with no connected
underlay or public ingress requirement. Cockroach runs outside Kubernetes and
backs the API through Kine; SQLite and embedded etcd are not substitutes.

This is a tested prototype. The [mixed local/Azure test](docs/mixed-cluster.md)
verified three Ready nodes, three healthy database members, all six directed
DNS/HTTP checks and tailnet SSH kubeconfig retrieval after documented repairs.
The test machines are torn down; private local disks and evidence are preserved.
Unattended fresh provisioning and failure-recovery remain unverified.

The older Pulumi Azure/VMSS workflow still uses Cilium, Blob-backed bootstrap,
private DNS and public API/egress networking. Its remaining migration is tracked
in [TODO.md](TODO.md). Public Cilium Gateway support has been removed. Existing
Pulumi project IDs and state must be preserved during that cleanup.

## Build and test

For a single Tailscale VM, see [Create a VM on a friend's tailnet](docs/friend-vm.md).
The [mixed local/Azure test](docs/mixed-cluster.md) verified two local VMs and one
Azure VM over Tailscale without Blob Storage, after documented prototype repairs.
The [three-node cluster runbook](docs/isolated-vm-cluster.md) creates independent
NAT VMs running CockroachDB and K3s over Tailscale. The 2026-11-09 test verified
three Ready nodes, three healthy database members and all six directed
DNS/Service checks after approving the pod routes.
With the binary and QEMU installed, the enrollment command is:

```sh
# Supply a tagged auth key through TAILSCALE_AUTH_KEY first.
infestation vm create --name alice-node
```

The single-VM command creates an enrollment-only Flatcar VM. `vm cluster create`
creates the three-node cluster; Azure retains its existing networking.

Requires Go **1.26.6 or newer**:

```sh
make build
make check
bin/infestation --help
```

A fresh checkout builds and tests without Python, Docker, Pulumi, Azure login,
or generated libvirt SDK source. Infrastructure tests use Pulumi mocks; they do
not provision anything. Runtime prerequisites depend on the command:

| Operation | Additional tools |
| --- | --- |
| Validate, PKI, image preparation | None |
| Local Ignition | Built `bin/infestation-node` helper |
| Azure price/quota planning | Azure CLI, authenticated subscription |
| Fetch Azure kubeconfig from Blob Storage | Azure CLI, authenticated identity with blob read access |
| Pulumi preview/apply | Pulumi CLI 3.272.0; Azure credentials or local libvirt access |
| Node-local bootstrap (draft) | Ignition services, node Docker/K3s, managed identity on Azure |
| Connectivity smoke test | kubectl and a reachable cluster |

SSH keys and workstation SSH bootstrap have been removed. Ignition masks SSH
services and sockets, and Azure denies inbound port 22. PKI uses Go crypto and
Ignition uses the official v3.3 types and validator.

Node-local SQL bootstrap uses a one-hour credential in the API pool's private
Ignition, used only by API instance `0`. Static inventories deliver it only to `-0`. Fresh Azure deployment without SSH and the pinned-image SQL
initialization/cleanup path have passed deployment and isolated Docker tests.
Only the `-0` server (`kube-0` by default) may initialize CockroachDB and perform
initial SQL setup. If it never starts, initial deployment waits. See the
[bootstrap design and Blob lease TODO](docs/bootstrap.md).

## Layout

```text
cmd/infestation/             CLI entry point
internal/cluster/           shared inventory and validation
internal/pki/               native Cockroach CA/node/client certificates
internal/ignition/          Flatcar Ignition and systemd configuration
internal/azureplan/         price, quota, SKU, and zone resolution
internal/bootstrap/         shared bootstrap guards, Kine SQL, kubeconfig
internal/nodebootstrap/     draft node-local init and kubeconfig publication
internal/deploy/            preview/apply/bootstrap orchestration
internal/provider/azure/    Azure Native Pulumi resources and mock tests
internal/provider/qemu/     libvirt Pulumi resources and mock tests
internal/image/             verified Flatcar image preparation
internal/smoke/             directed DNS and cross-node service checks
infra/{azure,qemu}/         thin Pulumi entry points and stack examples
examples/                   portable inventory and workload examples
docs/                       architecture, migration notes, historical lab records
```

`internal/` is intentional: these packages are implementation details of the
Infestation binary and its infrastructure programs, rather than a public Go SDK.

The tested egress-only path uses host Tailscale and K3s's native Tailscale/Flannel
integration, with kube-proxy restored. The older Pulumi Azure profiles still use
Cilium and a public API endpoint; their migration and Blob dependency removal
are tracked in [TODO.md](TODO.md).

The optional public Cilium Gateway path has been retired: `--gateway`,
`smoke --public-url`, the Gateway examples and Azure web forwarding/probes are
removed. Existing stacks with `gatewayEnabled: true` are rejected until that
setting is deliberately removed after reviewing the resource changes. This
source cleanup does not apply changes to existing cloud resources. The
[2026-10-27 experiment](infra/azure/RUNBOOK.md#2026-10-27-cilium-replacement-and-public-httpbin-gateway)
remains historical evidence, not a current deployment workflow.

## Shared node preparation

Copy [examples/cluster.json](examples/cluster.json) to a private state directory,
then set the node addresses (no SSH key is required):

```sh
mkdir -p state/local
chmod 700 state state/local
cp examples/cluster.json state/local/cluster.json
# Edit state/local/cluster.json before continuing.
bin/infestation validate --config state/local/cluster.json
bin/infestation pki --config state/local/cluster.json --state-dir state/local
go build -o bin/infestation-node ./cmd/infestation-node
bin/infestation ignition --config state/local/cluster.json --state-dir state/local
```

PKI creation refuses to overwrite existing credentials. Ignition regeneration
preserves the K3s token. Keep the CA signing key and admin credentials on the
workstation; only signed node/client credentials enter private Ignition files. Azure Uniform
pools use a shared wildcard node certificate so new instances can join without
a certificate enrollment service. Generated
files are mode `0600` in private state directories and are ignored by Git.

Normal clusters require exactly three servers. The explicit `smoke_test` topology
uses two API servers and one worker, with CockroachDB required on all three.
It exercises both roles but does not establish zone-failure guarantees.
A deliberate single-server lab
can set `"single_node_lab": true`; it must have exactly one database host.
Workers in that mode must set `"cockroachdb": false`. A workers-only inventory
requires explicit HTTPS `server_url` values and the original cluster token and
CA-issued credentials; do not create a new CA for nodes joining an existing cluster.

## Optional private API TailVIP

The generator supports an opt-in top-level `api_tailnet_hostname` setting in
your private inventory JSON, for example `"api_tailnet_hostname": "k8s-api.example.ts.net"`.
Replace `example.ts.net` with the real tailnet suffix. Omit the field (or set it
to null) to leave API TailVIP provisioning disabled.

When enabled, each generated K3s **server** gets the shared hostname in its
`tls-san` list and an enabled host-level `tailscale-api.service` that runs:

```sh
/usr/bin/tailscale serve --service=svc:k8s-api --tcp=443 tcp://127.0.0.1:6443
```

Only server profiles advertise this Service. Worker profiles never receive
the forwarder or shared API SAN, even when they host CockroachDB. A server with
`cockroachdb: false` still runs the API and advertises it.

**Prerequisites are external to this generator:** install the Tailscale binary
at `/usr/bin/tailscale` and its `tailscaled.service`, enroll each server using a
tag-based identity, and define `svc:k8s-api` with endpoint `tcp:443` in the tailnet.
Approve each host advertisement (or configure scoped auto-approval) and grant
administrative clients access to the Service on TCP 443. The unit retries failed
configuration every 15 seconds; it does not install Tailscale, enroll hosts, or
create the tailnet Service/policy. Do not enable the option without those steps.

After verifying the shared certificate SAN on every server, clients on the
tailnet can use `https://k8s-api.<tailnet>.ts.net:443` in kubeconfig with the
existing Kubernetes CA and client credentials. Serve forwards raw TCP, preserving
Kubernetes TLS and authentication. It targets the local API, not a ClusterIP.
Keep direct server endpoints for bootstrap and recovery.

Serve configuration persists in Tailscale state and is not withdrawn when K3s or
the oneshot unit stops. This avoids tying host networking to Kubernetes lifecycle,
but means a live Tailscale host with a failed local API may still receive traffic.
There is no readiness-aware withdrawal yet. Removing the option affects future
Ignition only; retiring an existing host advertisement requires an explicit
`tailscale serve drain svc:k8s-api` (then clearing its configuration after draining).
Default routing provides active/passive failover, not per-request balancing.

The mixed local/Azure test verified this API Service endpoint with Kubernetes
TLS validation. Readiness-aware withdrawal and failure/recovery remain untested.
See [Tailscale Services](https://tailscale.com/docs/features/tailscale-services),
[TCP forwarding](https://tailscale.com/docs/reference/tailscale-cli/serve#use-a-tcp-forwarder),
and [route selection](https://tailscale.com/docs/features/route-selection).

The [networking checkpoint](docs/networking-plan.md) records the selected
egress-only design: host-level Tailscale on every Flatcar node, K3s's native
Tailscale/Flannel extension integration replacing Cilium, kube-proxy restored,
and Cloudflare Tunnel replicas for public web ingress. No shared VNet/VPC or
inbound node connectivity is required. Host networking, kube-proxy and API access
were verified in the static mixed test after repairs. Public web ingress is still
planned. The older Pulumi provider retains Cilium and its networking until the
remaining migration in [TODO.md](TODO.md) is completed.

## Deploy

- [Azure Uniform VMSS](infra/azure/README.md): automatically choose and pin the
  cheapest compatible on-demand VM SKU with automatic three-zone placement, use one SLB
  for API ingress and egress, and orchestrate draft node-local bootstrap through one CLI command.
- [Local QEMU/KVM](infra/qemu/README.md): private NAT network, stable DHCP/MAC
  identity, persistent qcow2 overlays, and the same Go-generated Ignition.
- [Research questions](docs/research-questions.md): storage, cloud integration,
  Tailscale ingress, CNI alternatives, maintenance and recovery questions.
- [Architecture and portability](docs/architecture.md): shared and cloud-specific
  responsibilities. Google Compute Engine and AWS EC2 are planned, not implemented.

Azure's current cost-focused profile retains a 32 GiB managed OS disk and puts
Cockroach data on a separate local temporary resource disk. Three replicas and
zone locality can tolerate one replica loss; they are not backups and do not
recover a cluster after multiple stores lose quorum. QEMU keeps database data
on persistent VM overlays. Disk availability checks prevent accidental fallback
to the OS disk in the Azure temporary-storage profile.

The automatic bootstrap stores initialization intent before its one init RPC,
then records and verifies the cluster ID. It refuses to initialize a different
database after an ambiguous init or loss of quorum. Kine SQL is idempotent;
Cilium setup and kubeconfig retrieval can be resumed without replacing PKI.

Azure deployments now declare private, read-access geo-zone-redundant artifact
storage in the cluster resource group. Bootstrap publishes the admin kubeconfig;
fetch it without SSH or the original PKI directory:

```sh
az login
# After deploying a plan named infestation-dev and publishing its kubeconfig:
bin/infestation azure kubeconfig --outputs state/infestation-dev/outputs.json \
  --out state/infestation-dev/downloaded-kubeconfig
```

The command uses Azure identity authentication, keeps TLS verification and writes
mode `0600`. Existing files require `--force`. See the [Azure guide](infra/azure/README.md#retrieve-the-kubeconfig)
for portal-based discovery and access roles. Fresh Uniform VMSS first boot, SQL credential cleanup and Blob kubeconfig
publication were verified in the 2026-10-27 deployment.

To run the directed DNS/service check after bootstrap:

```sh
bin/infestation smoke --config state/local/cluster.json \
  --state-dir state/local --kubeconfig state/local/kubeconfig
```

This creates resources in namespace `infestation-smoke` and tests every directed
pair of configured nodes. It is an explicit workload test, not a read-only check.
Each invocation creates fresh check jobs instead of reusing completed results;
finished jobs expire after one hour.

For a Tailscale cluster, add `--tailnet-latency` to record directed host RTTs and
paths alongside the pod DNS/HTTP checks. Public Cilium Gateway checks have been
removed; public ingress is a separate follow-up design.

For a disposable Azure provision/smoke/teardown cycle:

```sh
go build -o bin/infestation ./cmd/infestation
bin/infestation azure check
```

The check defaults to Flatcar `4757.2.1`, CockroachDB `v26.2.7`, and K3s
`v1.35.8+k3s1`. Override any with `--flatcar-version`, `--cockroach-version`, or
`--k3s-version`; omitted versions keep their pinned defaults.

This opt-in command provisions **three VMs: two API nodes and one worker**, all
hosting CockroachDB. It runs six directed internal DNS/ClusterIP HTTP checks,
then removes its own tagged resource group
and verifies its absence. Cleanup is attempted on provisioning/test failure and
cancellation with a separate ten-minute deadline; failures are returned along
with the test error. Local private state and `check.log` remain under a fresh
`state/infcheck-*` directory. Each invocation uses a new name; it never resumes
or tears down a pre-existing cluster. See the [Azure guide](infra/azure/README.md#disposable-azure-check)
for prerequisites and verification limits.

## Existing state and verification

The repository was flattened and the Python tools replaced by Go on 2026-10-26.
See [migration notes](docs/migration.md) before reusing existing stacks or local
helpers. Pulumi project IDs retain their legacy names to preserve resource URNs;
the codebase, module, and CLI are named **infestation**. A source move must not
become an accidental VM replacement.

The historical [Azure runbook](infra/azure/RUNBOOK.md) and
[Pulumi QEMU runbook](infra/qemu/RUNBOOK.md) distinguish actual deployment tests
from unit/mock/preview checks. The [direct-QEMU lab record](docs/legacy-qemu-lab.md)
is archived; its retired Python commands are not the current workflow.

Still to design/test: coordinated Flatcar/database maintenance, backups and
restore, replacement identities and K3s registration, repeated disk-loss recovery,
application database persistence, and production sizing. Tailscale pod-network
integration is verified only within the documented prototype limits. Managed
AKS/GKE/EKS remain outside this implementation. See [TODO.md](TODO.md) for the
current priorities and the remaining legacy-provider cleanup.
