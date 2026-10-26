# Flatcar + CockroachDB + K3s on Azure Flexible VMSS

Pulumi Go provisions an explicit node inventory in a Flexible VM Scale Set. The
shared Python generator supplies Flatcar Ignition, TLS, CockroachDB, and K3s
server/worker configuration. Workers run CockroachDB by default and run the K3s
agent. Three servers provide the Kubernetes API.

The current cost-focused profile uses one Standard Load Balancer for API ingress
and TCP/UDP egress, a small persistent OS disk, and local temporary storage for
CockroachDB. There is **no NAT Gateway**. A deployment wrapper automates database
initialization, Kine SQL setup, Cilium installation, readiness checks, and private
kubeconfig retrieval.

The earlier Azure smoke deployment was verified and then deleted; its historical
inventory and lessons are in [RUNBOOK.md](RUNBOOK.md). The new temporary-storage
and automatic-bootstrap workflow has unit/mock coverage and a real Azure preview;
its VM first boot and recovery still need an integration test.

## Prerequisites

- Azure CLI authenticated to the intended subscription.
- Pulumi CLI 3.272.0, Go >=1.26.6, Python 3, Docker, kubectl, and Cilium CLI 0.20.1.
- Workstation SSH access to the nodes, either through source-restricted public
  node IPs (the automated profile) or existing private connectivity.
- A fresh SSH key and separate local state for each new database cluster.

The Azure Native SDK dependencies are pinned in `go.mod` and `go.sum`. No
Terraform or OpenTofu CLI is needed. Run from `flatcar-crdb-k3s/infra/azure`.

## Choose the cheapest compatible size and assign zones

Edit the node inventory in a copy of `../../cluster.json` if you need workers.
Use private addresses in `192.168.1.0/24`, excluding Azure's reserved addresses.
The defaults include three servers. Then prepare a plan:

```bash
mkdir -p state
chmod 700 state
ssh-keygen -t ed25519 -f state/dev-key -N ''
python3 plan.py --cluster ../../cluster.json --name flatcar-dev \
  --ssh-public-key state/dev-key.pub --admin-cidr YOUR_PUBLIC_IP/32 \
  --output-dir state/dev
```

Use fresh paths; do not overwrite an existing deployment's key or state. The
planner queries regional Linux on-demand retail prices, SKU capabilities, zones,
and subscription quota. It chooses the lowest-priced compatible SKU with:

- At least 2 vCPUs and 8 GiB RAM; Spot pricing is excluded.
- At least 64 GiB of Cockroach filesystem capacity, with 10% raw-disk headroom.
- x64/Generation 2 support, a SCSI resource disk, and three available zones.
- Enough VM-family and total vCPU quota for the entire configured inventory.
- Burstable sizes included if they meet the other constraints; use
  `--exclude-burstable` to exclude them.

`--min-vcpus`, `--min-memory-gib`, and `--min-disk-gib` override the minimums.
NVMe-only local disks currently require a separate tested provisioning profile
and are excluded. The program does not claim to choose the cheapest unsupported
VM in Azure. SKU availability does not guarantee allocation capacity at apply.

The planner assigns the three servers to distinct discovered zones and adds
matching Cockroach locality tiers (`cloud`, `region`, `zone`). Workers are spread
across those zones too. It writes `state/dev/plan.json` and `cluster.json`,
including the API DNS SAN, and refuses to overwrite the output directory. Sizes
and placements stay pinned rather than being recalculated during each preview.
Changing the inventory or hardware of a live cluster needs a deliberate update
procedure; choosing another cheapest SKU must not silently replace database VMs.

On 2026-10-25, with the current West US 2 subscription quota, the selected SKU
was `Standard_D2d_v4`: 2 vCPUs, 8 GiB RAM, a 75 GiB resource disk, and $0.113 USD
per node-hour. Three nodes are about $247/month at 730 hours, excluding OS disks,
public IPs, SLB, bandwidth, taxes, and subscription discounts. Rerun discovery
into a fresh plan for current prices. The private example plan is `state/next`.

API access defaults to `--admin-cidr`. Add `--api-cidr 0.0.0.0/0` to explicitly
allow public IPv4 API access. SSH remains restricted to the admin source range.
The API still requires Kubernetes credentials and uses verified TLS.

## Preview or deploy with automatic bootstrap

Pin a Flatcar Marketplace version available in the region, for example:

```bash
az vm image list --location westus2 --publisher kinvolk \
  --offer flatcar-container-linux-corevm-amd64 --sku stable-gen2 --all \
  --query '[].{version:version,urn:urn}' --output table
```

The CoreVM offer has the same contents as Flatcar's other Azure offers and
requires no image purchase plan or terms acceptance. The old smoke deployment
used `4757.2.1`.

A preview creates private local PKI and a separate local Pulumi backend, but no
Azure resources:

```bash
python3 deploy.py --plan-dir state/dev --stack dev \
  --ssh-private-key state/dev-key --flatcar-version 4757.2.1 --preview-only
```

To create the cluster and complete bootstrap:

```bash
python3 deploy.py --plan-dir state/dev --stack dev \
  --ssh-private-key state/dev-key --flatcar-version 4757.2.1 \
  --initialize-new-cluster
```

Review the Pulumi plan when prompted; `--yes` is available for unattended runs.
The wrapper creates per-node credentials, generates Ignition, saves a refreshed
preview, applies that plan with refresh, and then completes the post-VM work.
No manual `cockroach init`, SQL grants, CNI installation, or kubeconfig copying
is required. CA private keys and admin SQL credentials remain on the workstation.

`--initialize-new-cluster` permits a first initialization attempt. Before that
RPC, the workflow writes an intent marker. It never automatically repeats that
attempt after an interruption. Successful connections record and subsequently
verify the database's cluster ID. An established cluster becoming unavailable
must recover or be restored, rather than being initialized as a different cluster.
The Kine SQL batch is idempotent. Repeated bootstrap skips an existing Cilium
installation, checks its readiness and effective kube-proxy setting, and refreshes
the private admin kubeconfig.

Cilium keeps kube-proxy enabled, uses pod CIDR `10.42.0.0/16`, CNI binaries
`/opt/cni/bin`, and configuration `/etc/cni/net.d`. Its bootstrap API address is
the Kubernetes Service VIP (`10.43.0.1:443`) through kube-proxy, avoiding a fixed
single-server IP. This revised bootstrap path still needs a VM integration test.

Once deployment completes:

```bash
export KUBECONFIG="$PWD/state/dev/kubeconfig"
kubectl get nodes -o wide
kubectl get --raw=/readyz
```

Retain the plan directory, backend, passphrase, PKI, and SSH key in private
backups. The passphrase file and all generated credentials are mode `0600` in a
private directory. Local stack configs, state, plans, Ignition, and kubeconfigs
are ignored by Git. The backend is local to this plan; team deployments should
use an access-controlled shared backend. Do not use the old deleted smoke stack
as though it still refers to a running cluster.

## Networking and disks

One Standard public frontend supports an API backend pool containing servers
only, plus an egress pool containing **all nodes**. The explicit outbound rule
handles TCP and UDP with 1,024 SNAT ports per node by default. Inbound API SNAT
stays disabled. `outboundPortsPerNode` is configurable; validation checks its
allocation block and the single-public-IP port budget. NAT Gateway can be added
in a future change without altering the VM image.

The default OS disk is a persistent 32 GiB Standard SSD managed disk, containing
Flatcar state, certificates, K3s state, logs, and container images. CockroachDB
uses `/mnt/resource/cockroach` on the separate local temporary disk. Flatcar's
Azure WAAgent formats the SCSI resource disk as ext4 and mounts `/mnt/resource`
on boot. The Cockroach unit requires WAAgent and checks the exact mount,
filesystem, and capacity before starting. It never falls back to the OS disk
when the temporary disk is absent or too small. The generator does not add a
blanket disk-formatting command.

The three-zone database uses Cockroach's default replication factor of three.
A lost store rejoins surviving peers using the existing CA and join addresses;
bootstrap does not initialize it as a new cluster. Three replicas can retain
quorum through one replica loss; they do not guarantee recovery after multiple
local stores are lost before repair. Testing re-replication after one disk loss,
and scheduling backups independently of these disks, remain necessary.

Managed OS disks use `deleteOption: Detach`, and VMs have Pulumi protection.
Resource-group deletion still removes the group and its resources. No ephemeral
OS disk is configured. New VM replacement also needs a procedure for stale
Cockroach identities, K3s node registration, and application storage; that is
separate from restarting with an empty resource disk and intact OS state.

## Validation

```bash
go test ./...
python3 -m unittest discover -v
# From the project root for shared generator tests:
python3 -m unittest discover -v
```

Go mocks cover server/worker identity, TLS secrets, VM protection, retained OS
disks, SLB ingress/egress pools, public/private SSH modes, source ranges, SNAT
budgets, locality/zone matching, and rejection of stale temporary-storage profiles.
Python tests cover price/quota filtering, zone assignment, and initialization
identity/intent guards, including retries after interrupted initialization and
loss of quorum. Shared tests cover generated credential ownership, role defaults,
SANs, and temporary-storage mount guards. QEMU rejects the Azure storage profile.

The historical manual smoke test passed actual Flatcar first boot, database
initialization, three Ready nodes, Cilium, six directed DNS/service checks, and
public API TLS. It used NAT Gateway and persistent database storage. Those results
do **not** establish that the revised SLB-egress, temporary-disk, and automatic
bootstrap profile has passed a real deployment or a fault-recovery test.

References: [Azure outbound rules](https://learn.microsoft.com/en-us/azure/load-balancer/outbound-rules),
[Flatcar Azure integration](https://www.flatcar.org/docs/latest/deploy/cloud/azure/),
[Flatcar WAAgent defaults](https://github.com/flatcar/scripts/blob/main/sdk_container/src/third_party/coreos-overlay/app-emulation/wa-linux-agent/files/0001-flatcar-changes.patch),
[Cockroach production guidance](https://www.cockroachlabs.com/docs/stable/recommended-production-settings).
