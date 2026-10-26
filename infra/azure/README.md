# Azure Flexible VMSS

Run commands from the repository root. Build the CLI with `make build`; the
Pulumi program here is a thin entry point to `internal/provider/azure` in the
same root Go module. The project ID `flatcar-k3s-azure` is retained for existing
state compatibility; new resource names can use `infestation-*`.

## Plan and deploy

Planning requires Azure CLI authentication. Applying requires Pulumi 3.272.0;
bootstrap additionally uses Docker for the Cockroach admin CLI, kubectl and
Cilium CLI 0.20.1. PKI/SSH/Ignition are native Go and require no Python, OpenSSL,
or Docker certificate-generation container.

```sh
mkdir -p state
chmod 700 state
ssh-keygen -t ed25519 -f state/dev-key -N ''
bin/infestation azure plan --config examples/cluster.json --name infestation-dev \
  --ssh-public-key state/dev-key.pub --admin-cidr YOUR_PUBLIC_IP/32 \
  --output-dir state/dev
```

Edit a private copy of the example inventory first if you want workers. Include
exactly three servers and all workers; workers host CockroachDB by default.
Choose addresses inside the configured subnet, excluding Azure's first four and
last reserved addresses. Defaults use `192.168.1.0/24`; avoid overlap with any
connected network and the K3s pod/service ranges `10.42.0.0/16`, `10.43.0.0/16`.

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
overwrite an existing plan, spreads the servers over three discovered zones,
and adds matching Cockroach locality tiers and the API DNS SAN. Resolved sizes
and placements stay fixed between previews instead of changing with prices.

In the earlier West US 2 quota snapshot, `Standard_D2d_v4` was selected: 2 vCPUs,
8 GiB RAM, 75 GiB resource disk, $0.113/node-hour (about $247/month for three VMs
at 730 hours). That excludes OS disks, networking and discounts. Rerun discovery
into a fresh plan for current prices.

API access defaults to the admin source ranges. Add `--api-cidr 0.0.0.0/0` for
explicit public IPv4 API access. SSH remains independently source-restricted;
SQL and kubelet are not publicly exposed.

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
bin/infestation azure deploy --plan-dir state/dev --stack dev \
  --ssh-private-key state/dev-key --flatcar-version 4757.2.1 --preview-only
```

Then create the cluster and automatically complete bootstrap:

```sh
bin/infestation azure deploy --plan-dir state/dev --stack dev \
  --ssh-private-key state/dev-key --flatcar-version 4757.2.1 \
  --initialize-new-cluster
```

The wrapper saves a refreshed Pulumi preview and applies the saved plan with
refresh. Review its prompt; `--yes` is available for unattended deployment.
`--repo` locates the checkout when invoking a binary from another directory.

The same command handles fresh PKI, Ignition, one guarded database init, Kine
SQL grants, Cilium, readiness checks and admin kubeconfig retrieval. The CA key
and SQL root client credentials stay on the workstation. Bootstrap records
initialization intent before its RPC, then records/verifies the cluster ID.
It never automatically repeats an ambiguous init or replaces a database after
quorum loss. Re-running bootstrap uses idempotent SQL and checks existing Cilium.

```sh
export KUBECONFIG="$PWD/state/dev/kubeconfig"
kubectl get nodes -o wide
kubectl get --raw=/readyz
bin/infestation smoke --config state/dev/cluster.json --state-dir state/dev
```

The kubeconfig embeds admin credentials and retains TLS CA verification. The
Standard Load Balancer passes TCP 6443 through to K3s; its DNS name is included
in every server certificate. Cilium keeps kube-proxy enabled and uses the
Kubernetes Service VIP for bootstrap rather than one fixed private server IP.

## Networking and storage

One Standard public frontend supports an API server pool plus an all-node egress
pool. Explicit outbound rules handle TCP and UDP with 1,024 SNAT ports/node by
default. Validation checks allocation blocks and the single-IP port budget;
`outboundPortsPerNode` adjusts the allocation. There is no NAT Gateway.

A persistent 32 GiB Standard SSD OS disk holds Flatcar state, node credentials,
K3s state, images and logs. Cockroach uses `/mnt/resource/cockroach` on the local
temporary resource disk. Flatcar's WAAgent formats/mounts that disk as ext4; the
Cockroach service requires WAAgent and checks mount, filesystem and capacity.
Missing/undersized temporary storage cannot fall back onto the OS disk.

Three-zone locality and RF=3 tolerate one replica loss while a store rejoins
surviving peers. They do not replace backups or ensure recovery after multiple
stores lose quorum. OS disks are retained with `Detach`; VMs are protected from
Pulumi replacement. Resource-group deletion still removes all resources.
The bootstrap package does not automatically decommission old Cockroach IDs or
remove K3s registration secrets when replacing an entire VM; those operations
need a deliberate replacement procedure and tests.

## State and verification

Each plan gets its own local backend, passphrase, owner record, PKI and token.
Keep that directory and SSH key in private backups. An existing plan/image/stack
identity cannot silently change during a resumed deploy. `--initialize-new-cluster`
is only for the first fresh init; it cannot override an existing identity or
previous initialization intent. The CLI currently uses local file state; teams
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
