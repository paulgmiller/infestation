# Infestation

Infestation provisions Flatcar VMs running host-level CockroachDB and K3s, with
Cilium networking. It is one Go module with a shared configuration, PKI,
Ignition, planning, and bootstrap implementation for Azure and local QEMU/KVM.

Three server nodes run the Kubernetes API, backed by TLS CockroachDB through
Kine. Additional workers run the K3s agent and host CockroachDB by default;
`"cockroachdb": false` opts a node out. Cockroach starts independently of
Kubernetes and stores its data outside pods. Kube-proxy stays enabled; Flannel,
Traefik, ServiceLB, embedded etcd, and SQLite are not used.

This is a tested prototype, not a production-ready operations platform. Earlier
QEMU and Azure smoke deployments passed; the revised temporary-disk Azure
profile has preview/unit coverage but has not passed a new VM deployment or
failure-recovery test. The Azure smoke resource group was deleted. The local
Pulumi QEMU VMs remain stopped with their persistent disks preserved.

## Build and test

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
Ignition, used only by API instance `0`. Static inventories deliver it only to `-0`. The pinned-image SQL initialization/cleanup path passes
an isolated Docker test; fresh Azure deployment without SSH is not yet verified.
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

Normal clusters require exactly three servers. A deliberate single-server lab
can set `"single_node_lab": true`; it must have exactly one database host.
Workers in that mode must set `"cockroachdb": false`. A workers-only inventory
requires explicit HTTPS `server_url` values and the original cluster token and
CA-issued credentials; do not create a new CA for nodes joining an existing cluster.

## Deploy

- [Azure Uniform VMSS](infra/azure/README.md): automatically choose and pin the
  cheapest compatible on-demand VM SKU with automatic three-zone placement, use one SLB
  for API ingress and egress, and orchestrate draft node-local bootstrap through one CLI command.
- [Local QEMU/KVM](infra/qemu/README.md): private NAT network, stable DHCP/MAC
  identity, persistent qcow2 overlays, and the same Go-generated Ignition.
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
for portal-based discovery and access roles. SQL credential delivery is wired
through `-0` Ignition; complete node-local first boot and periodic publication
still need fresh Azure deployment verification.

To run the directed DNS/service check after bootstrap:

```sh
bin/infestation smoke --config state/local/cluster.json \
  --state-dir state/local --kubeconfig state/local/kubeconfig
```

This creates resources in namespace `infestation-smoke` and tests every directed
pair of configured nodes. It is an explicit workload test, not a read-only check.
Each invocation creates fresh check jobs instead of reusing completed results;
finished jobs expire after one hour.

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
application database persistence, and production sizing. Tailscale integration
and managed AKS/GKE/EKS are outside this stack.
