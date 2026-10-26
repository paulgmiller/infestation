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
| Validate, PKI, Ignition, image preparation | None |
| Azure price/quota planning | Azure CLI, authenticated subscription |
| Pulumi preview/apply | Pulumi CLI 3.272.0; Azure credentials or local libvirt access |
| Database/Kubernetes bootstrap | Docker (Cockroach CLI), kubectl, Cilium CLI 0.20.1 |
| Connectivity smoke test | kubectl and a reachable cluster |

Bootstrap currently assumes a Linux workstation for Docker host networking and
Unix state permissions. SSH transport and forwarding use Go's SSH library. New host keys are recorded
in private `known_hosts`; changed host keys are rejected. PKI uses Go crypto,
and Ignition uses the official v3.3 types and validator. There is no Docker or
OpenSSL dependency for certificate generation.

## Layout

```text
cmd/infestation/             CLI entry point
internal/cluster/           shared inventory and validation
internal/pki/               native Cockroach CA/node/client certificates
internal/ignition/          Flatcar Ignition and systemd configuration
internal/azureplan/         price, quota, SKU, and zone resolution
internal/bootstrap/         guarded database init, Kine SQL, Cilium, kubeconfig
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
then set the real SSH public key and node addresses:

```sh
mkdir -p state/local
chmod 700 state state/local
cp examples/cluster.json state/local/cluster.json
# Edit state/local/cluster.json before continuing.
bin/infestation validate --config state/local/cluster.json
bin/infestation pki --config state/local/cluster.json --state-dir state/local
bin/infestation ignition --config state/local/cluster.json --state-dir state/local
```

PKI creation refuses to overwrite existing credentials. Ignition regeneration
preserves the K3s token. Keep the CA signing key and admin credentials on the
workstation; only per-node credentials enter private Ignition files. Generated
files are mode `0600` in private state directories and are ignored by Git.

Normal clusters require exactly three servers. A deliberate single-server lab
can set `"single_node_lab": true`; it must have exactly one database host.
Workers in that mode must set `"cockroachdb": false`. A workers-only inventory
requires explicit HTTPS `server_url` values and the original cluster token and
CA-issued credentials; do not create a new CA for nodes joining an existing cluster.

## Deploy

- [Azure Flexible VMSS](infra/azure/README.md): automatically choose and pin the
  cheapest compatible on-demand VM SKU and three-zone placement, use one SLB
  for API ingress and egress, and complete bootstrap through one CLI command.
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
