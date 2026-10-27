# Azure Uniform VMSS

Run commands from the repository root. Build the CLI with `go build -o bin/infestation ./cmd/infestation`; the
Pulumi program here is a thin entry point to `internal/provider/azure` in the
same root Go module. The project ID `flatcar-k3s-azure` is retained for existing
state compatibility; new resource names can use `infestation-*`.

## Plan and deploy

Planning requires Azure CLI authentication. Applying requires Pulumi 3.272.0
and Go to build the node bootstrap helper. SSH keys, SSH connectivity and a
workstation Docker/Cilium installation are no longer part of deployment.

**Fresh Azure deployment remains unverified.** Initial SQL credential delivery
is wired through `-0` Ignition and passes an isolated pinned-Cockroach test.
Planning and preview are reviewable; complete Flatcar/K3s/Cilium first boot
without SSH has not yet been demonstrated. See the
[bootstrap design](../../docs/bootstrap.md) for the fixed `-0` initializer and
future Blob lease TODO.

```sh
mkdir -p state
chmod 700 state
bin/infestation azure plan --name infestation-dev
```

The only required planning flag is `--name`. Plans default to `state/<name>`;
`--output-dir` overrides that path. No user SSH key is read or required. Azure requires an OS authentication
profile: generation creates a throwaway public key and immediately clears and
discards its private key. Only that unusable public key is stored in the profile;
SSH services remain masked and inbound port 22 denied. Existing plan
directories are never overwritten. New plans normalize inventory entries to
`api-0`, `api-1`, `api-2` and `worker-N`; only API instance `0` may initialize
CockroachDB. Existing plans and PKI are not renamed automatically.

Include three server entries and one entry per initial worker in `--config`.
Their IP fields are inventory placeholders; Azure supplies DHCP addresses and
IMDS supplies each instance's identity and locality. Workers host CockroachDB
by default; all members of a role must use the same database setting.
The current Uniform network uses `192.168.1.0/24`. Avoid overlap with connected networks and K3s ranges.

The planner checks Linux on-demand retail prices, subscription and family quota,
and actual regional SKU/zone capabilities. Defaults are at least 2 vCPU, 8 GiB
RAM, 64 GiB usable database capacity (with 10% raw-disk headroom), x64 Gen2, and
three available zones. Spot is excluded. Burstable sizes are included if they
meet the other requirements; `--exclude-burstable` excludes them. Use
`--min-vcpus`, `--min-memory-gib`, and `--min-disk-gib` to change minimums.

The supported temporary-storage profile uses Flatcar's SCSI resource disk;
NVMe-only local disks require a separate tested setup and are excluded. The
planner chooses the cheapest **compatible** SKU, not an arbitrary Azure size.
Regional availability/quota checks cannot guarantee capacity at allocation time.

It saves `plan.json` and `cluster.json` in a new private directory, refuses to
overwrite an existing plan, and pins the cheapest compatible SKU. Both pools use
`placement.zonePlacementPolicy: auto` with three-zone balancing; zone lists are
not prescribed. IMDS supplies `cloud=azure,region=...,zone=...` at startup.
Automatic zone placement is a preview requiring the subscription feature
`Microsoft.Compute/VmssAutomaticZonePlacement` and Compute API `2026-03-01`.
The pinned Pulumi provider supports that versioned resource; the implementation
registers it directly because the default Go SDK lacks its placement fields.
[Azure placement documentation](https://learn.microsoft.com/en-us/azure/virtual-machine-scale-sets/virtual-machine-scale-sets-automatic-zone-placement).

In the earlier West US 2 quota snapshot, `Standard_D2d_v4` was selected: 2 vCPUs,
8 GiB RAM, 75 GiB resource disk, $0.113/node-hour (about $247/month for three VMs
at 730 hours). That excludes OS disks, networking and discounts. Rerun discovery
into a fresh plan for current prices.

The API defaults to public IPv4 access (`0.0.0.0/0`) with K3s authentication and
TLS. `--admin-cidr` sets the API/admin source ranges; `--api-cidr` overrides
those ranges. Neither flag opens SSH. Nodes have no public IPs; SSH is denied by the NSG
and masked in Ignition. SQL and kubelet are not publicly exposed.

Pin a Marketplace image available in your region:

```sh
az vm image list --location westus2 --publisher kinvolk \
  --offer flatcar-container-linux-corevm-amd64 --sku stable-gen2 --all \
  --query '[].{version:version,urn:urn}' --output table
```

The CoreVM offer matches the other Flatcar Azure image contents and needs no
purchase plan/Marketplace terms acceptance. The historical smoke test used
`4757.2.1`. Preview and generate local credentials without provisioning:

```sh
bin/infestation azure deploy --plan-dir state/infestation-dev --stack dev \
  --flatcar-version 4757.2.1 --preview-only
```

Then create the cluster and automatically complete bootstrap:

```sh
bin/infestation azure deploy --plan-dir state/infestation-dev --stack dev \
  --flatcar-version 4757.2.1
```

The wrapper saves a refreshed Pulumi preview and applies the saved plan with
refresh. Review its prompt; `--yes` is available for unattended deployment.
`--repo` locates the checkout when invoking a binary from another directory.

The intended flow prepares PKI and Ignition, uploads the checksum-pinned node
helper, and waits for nodes to initialize themselves. The `-0` VM owns init and
SQL setup. K3s installs Cilium from an Ignition-provided HelmChart manifest.
The CA key and long-lived SQL root client credentials stay on the workstation.
A separately issued one-hour SQL root client credential is embedded only in
the API pool's shared private Ignition. The node deletes its temporary credential files after
Kine SQL setup succeeds and uses the normal Kine identity thereafter. This
credential is not distributed through Blob Storage. See the [credential lifecycle](../../docs/bootstrap.md#initial-sql-credential).
The full command must not yet be considered Azure deployment-verified.

```sh
export KUBECONFIG="$PWD/state/infestation-dev/kubeconfig"
kubectl get nodes -o wide
kubectl get --raw=/readyz
bin/infestation smoke --config state/infestation-dev/cluster.json --state-dir state/infestation-dev
```

The kubeconfig embeds admin credentials and retains TLS CA verification. The
Standard Load Balancer passes TCP 6443 through to K3s; its DNS name is included
in every server certificate. Cilium keeps kube-proxy enabled and uses the
Kubernetes Service VIP for bootstrap rather than one fixed private server IP.

## Retrieve the kubeconfig

The Azure stack creates a StorageV2 account in the cluster resource group using
`Standard_RAGZRS`: zone redundancy plus a readable copy in Azure's paired region.
The `access` container is private, anonymous blob access and Shared Key auth are
disabled, and HTTPS/TLS 1.2 are required. The public HTTPS storage endpoint is
reachable with Azure identity authentication; this is not a private endpoint.
Creation requires permission to assign Azure RBAC roles. The deployment identity
receives Storage Blob Data Contributor scoped to this container. Additional
operators need Storage Blob Data Reader on the container or an enclosing scope;
subscription/resource ownership alone does not grant blob data access.

Bootstrap publishes `access/kubeconfig.json` after the API passes its TLS and
readiness check, before Cilium/node checks. This lets you fetch credentials to
debug an incomplete networking bootstrap. Deployment outputs contain the
non-secret `kubeconfigBlob` location and `apiEndpoint`; no kubeconfig credentials
are exported into Pulumi outputs.

```sh
az login
# After deployment has saved outputs.json and bootstrap has published the blob:
bin/infestation azure kubeconfig --outputs state/infestation-dev/outputs.json \
  --out state/infestation-dev/downloaded-kubeconfig
KUBECONFIG="$PWD/state/infestation-dev/downloaded-kubeconfig" kubectl get nodes
```

The command requires Azure CLI authentication and blob read permission. It does
not use SSH, Docker, kubectl, the original SSH key or the PKI directory. Files are
written with mode `0600`. Existing files require `--force`; failed downloads or
invalid kubeconfigs leave existing files unchanged. Executable authentication,
external credential files, proxies and disabled TLS verification are rejected.
Neither tokens nor kubeconfig contents are printed.

On a fresh workstation, obtain the account name and regional API hostname from
the Azure portal and fetch without any local deployment state:

```sh
bin/infestation azure kubeconfig --account ACCOUNT_NAME \
  --subscription SUBSCRIPTION_ID \
  --endpoint https://REGIONAL_API_HOSTNAME:6443 --out state/recovered-kubeconfig
```

The draft node-local bootstrap publishes and periodically refreshes the
kubeconfig using VM managed identity. There is no legacy SSH fetch path.
Fetching repeatedly downloads the latest published copy; it cannot renew an
expired certificate itself. Full first boot and renewal still need integration
verification. See the [operations plan](../../docs/operations-plan.md).

The account and its `CanNotDelete` lock are Pulumi-protected. Deleting the resource
group is blocked until the Azure lock is explicitly removed. Deleting these
resources through Pulumi also requires deliberately removing Pulumi protection.
The lock does not prevent deletion of individual blobs. Access credentials are
not subject to the planned one-day **database backup** retention; database backup
jobs, retention and regional restore are not implemented by this change.

References: [Azure identity authorization](https://learn.microsoft.com/en-us/rest/api/storageservices/authorize-with-azure-active-directory),
[Azure redundancy](https://learn.microsoft.com/en-us/azure/storage/common/storage-redundancy),
[Azure resource locks](https://learn.microsoft.com/en-us/azure/azure-resource-manager/management/lock-resources).

## Pool identity and scale-out

New plans create two Uniform scale sets, `<name>-api` and `<name>-worker`, with
one shared Ignition profile per role. Increase worker capacity with Azure VMSS
scaling; new nodes fetch the same checksum-pinned Go helper, read IMDS, register
an A record in `<name>.<region>.internal`, then start Cockroach and the K3s agent.
The API pool stays at three members. There is no autoscale policy or safe
scale-in/decommission implementation yet. Pulumi capacity remains authoritative
on subsequent applies; coordinate out-of-band capacity changes with stack config.

```sh
az vmss scale --resource-group <name>-rg --name <name>-worker --new-capacity 2
```

A shared `CN=node` certificate covers `*.<name>.<region>.internal` plus localhost;
no future instance names need pre-registration. The one-year leaf key is shared
between database nodes. CA signing and long-lived admin keys stay local. Rotation
requires deliberately issuing a replacement leaf, updating both pool templates
and rolling the nodes; no automatic renewal is implemented. A compromised VM
can impersonate another database node. Pool managed identities can update the
cluster private DNS zone; this is a trusted cluster boundary.

Only API instance ID `0` initializes SQL. The API profile includes the one-hour
SQL credential; other API instances remove their copy and workers never receive
it. Azure does not guarantee sequential IDs or that ID `0` will exist. If it is
absent at initial deployment, bootstrap waits instead of selecting another node.
[Azure instance IDs](https://learn.microsoft.com/en-us/azure/virtual-machine-scale-sets/virtual-machine-scale-sets-instance-ids).

## Networking and storage

One Standard load balancer supplies public API ingress and egress.
Workers discover a reachable API node through private DNS before each agent
start; node NICs have no public addresses. Explicit outbound rules handle TCP and UDP with 1,024 SNAT ports/node by
default. Validation checks allocation blocks and the single-IP port budget;
`outboundPortsPerNode` adjusts the allocation. There is no NAT Gateway.

A persistent 32 GiB Standard SSD OS disk holds Flatcar state, node credentials,
K3s state, images and logs. Cockroach uses `/mnt/resource/cockroach` on the local
temporary resource disk. Flatcar's WAAgent formats/mounts that disk as ext4; the
Cockroach service requires WAAgent and checks mount, filesystem and capacity.
Missing/undersized temporary storage cannot fall back onto the OS disk.

Three-zone locality and RF=3 tolerate one replica loss while a store rejoins
surviving peers. They do not replace backups or ensure recovery after multiple
stores lose quorum. Scale sets are protected from Pulumi replacement. Uniform scale-in deletes
instances and can erase their local database stores; do not use it as a safe
database decommission procedure. The artifact account's delete lock blocks resource-group
deletion until explicitly removed; after removal, deleting the group removes
the storage account and its replicated artifacts too.
The bootstrap package does not automatically decommission old Cockroach IDs or
remove K3s registration secrets when replacing an entire VM; those operations
need a deliberate replacement procedure and tests.

## State and verification

Each plan gets its own local backend, passphrase, owner record, PKI and token.
Keep that directory in private backups. An existing plan/image/stack
identity cannot silently change during a resumed deploy. Only the `-0` VM may perform first initialization; an existing identity or
previous ambiguous initialization intent is not overridden automatically. The CLI currently uses local file state; teams
can use the thin Pulumi entry point and shared backends with separately prepared
profiles if needed.

[RUNBOOK.md](RUNBOOK.md) records a prior real Azure deployment, since deleted.
It used the earlier Python tooling, NAT Gateway and persistent database storage.
The root Go migration has unit/mock checks and an actual 17-resource preview;
its temporary-disk first boot, automatic bootstrap and disk-loss recovery still
need a new VM integration test. No cloud deployment is performed by `make check`.

References: [Flatcar Azure](https://www.flatcar.org/docs/latest/deploy/cloud/azure/),
[SLB outbound rules](https://learn.microsoft.com/en-us/azure/load-balancer/outbound-rules),
[WAAgent defaults](https://github.com/flatcar/scripts/blob/main/sdk_container/src/third_party/coreos-overlay/app-emulation/wa-linux-agent/files/0001-flatcar-changes.patch),
[Cockroach storage guidance](https://www.cockroachlabs.com/docs/stable/recommended-production-settings).
