# Azure Flexible VMSS

This provisions a new resource group, VNet/subnet, NAT Gateway, network security
group, and a **Flexible VM Scale Set** containing individually declared Flatcar
VMs. Each VM gets the matching Ignition, static private IP, and explicitly chosen
availability zone. Both roles run CockroachDB by default; workers run the K3s
agent without an API server.

This is an explicit node inventory, not a capacity-based autoscaling profile.
Flexible VMSS supports this arrangement with different configurations per VM.
There is no shared node certificate, CA signing key on instances, automatic
database initialization, Cilium installation, or remote shell provisioning.
The [cross-cloud design](../README.md) describes the choice of tooling.

## Prepare locally

You need Python 3, Docker for certificate generation, Go >= 1.26.6, the Pulumi
CLI, Azure CLI, and access to an Azure subscription. The Go SDK dependencies
and checksums are pinned in `go.mod` and `go.sum`; Azure Native uses Azure's APIs
directly. No Terraform or OpenTofu installation is required.

From this directory:

```bash
cp ../../cluster.json cluster.local.json
mkdir -m 700 state
go mod download
pulumi login --local
pulumi stack init dev
```

Choose a strong state-encryption passphrase when prompted. Then merge the
settings from `Pulumi.stack.example.yaml` into the created `Pulumi.dev.yaml`,
preserving Pulumi's generated `encryptionsalt` field. Local stack files and
state are ignored by Git. For a team backend you can instead use Pulumi Cloud
or an existing, access-controlled Azure Blob container:

```bash
pulumi login 'azblob://YOUR_CONTAINER?storage_account=YOUR_STORAGE_ACCOUNT'
```

Edit `cluster.local.json`: set your SSH public key and node names/private IPs,
and add any worker entries with `role: "worker"`. Include all three servers and
all workers; do not set `single_node_lab` or `cluster_mac`. Azure reserves each
static IP on the NIC and delivers it through DHCP. No host network override is
needed. Use addresses inside `subnetCIDR`, excluding its first four and final
addresses. Avoid overlap with K3s pod `10.42.0.0/16` and service `10.43.0.0/16`
networks, including any peered networks.

Edit `Pulumi.dev.yaml` with your subscription, region, network ranges, and a
zone assignment for **every** node. Keep assignments stable when adding nodes.
Spread the three database/server hosts across three zones if those zones support
your selected SKU. Verify VM quota and SKU availability in your subscription.

Find and pin a Marketplace Flatcar version in that region:

```bash
az login
az account set --subscription YOUR_SUBSCRIPTION_ID
az vm image list --location westus2 --publisher kinvolk \
  --offer flatcar-container-linux-free --sku stable-gen2 --all \
  --query '[].{version:version,urn:urn}' --output table
```

Replace the region above if needed and set `flatcarVersion` to an exact version
returned by Azure. If Azure requires accepting Marketplace terms, review them
and then run:

```bash
az vm image terms show --publisher kinvolk --offer flatcar-container-linux-free --plan stable-gen2
az vm image terms accept --publisher kinvolk --offer flatcar-container-linux-free --plan stable-gen2
```

Generate fresh credentials and Ignition for this deployment:

```bash
python3 ../../generate.py pki --config cluster.local.json --state-dir state
python3 ../../generate.py ignition --config cluster.local.json --state-dir state
```

An existing PKI directory is never overwritten. When expanding an existing
cluster, use its original CA/token and issue node certificates for new database
hosts; do not create a replacement CA or initialize a second cluster.

## Review the plan and deploy

```bash
go test ./...
pulumi preview --diff --save-plan=state/deployment-plan.json
```

Review the resource count, zones, addresses, costs, and disk settings before
applying. The NAT Gateway, its public IP, VM disks, and VMs incur Azure charges.
Creating these local files and validating them does not deploy anything.

The following command actually provisions machines:

```bash
pulumi up --plan=state/deployment-plan.json
```

Private node IPs are the default. Manage them from a host with connectivity to
the subnet. For an initial workstation-accessible deployment, set
`publicNodeIPs: true` and `adminCIDRs: ["YOUR_PUBLIC_IP/32"]` before previewing.
Only SSH is permitted from those sources; SQL, kubelet, and the Kubernetes API
are not publicly exposed. NAT provides explicit outbound connectivity for
Ignition's sysext download and container pulls, including when all nodes are
private. Node-to-node traffic is allowed within the node subnet for CockroachDB,
K3s, and Cilium. Workload ingress is a separate future configuration.

```bash
pulumi stack output privateIPs
pulumi stack output publicIPs
ssh core@REACHABLE_NODE_IP 'sudo systemctl status cockroachdb k3s'
# On workers, use k3s-agent instead of k3s.
```

## Initialize a brand-new cluster

Wait until all database processes are running. Keep the root client credentials
on your workstation. From this directory, forward SQL through the first server
in a separate terminal (replace both addresses for your configuration):

```bash
ssh -N -L 127.0.0.1:26257:192.168.1.101:26257 core@REACHABLE_SERVER_IP
```

**Only for a brand-new, uninitialized database cluster**, run this once:

```bash
CRDB_IMAGE=$(python3 -c 'import json; print(json.load(open("cluster.local.json"))["cockroach_image"])')
docker run --rm --network host --user "$(id -u):$(id -g)" \
  -v "$PWD/state/pki/admin:/certs:ro" --entrypoint=/cockroach/cockroach \
  "$CRDB_IMAGE" init --certs-dir=/certs --host=127.0.0.1:26257
docker run --rm --network host --user "$(id -u):$(id -g)" \
  -v "$PWD/state/pki/admin:/certs:ro" -v "$PWD/../../bootstrap.sql:/bootstrap.sql:ro" \
  --entrypoint=/cockroach/cockroach "$CRDB_IMAGE" sql \
  --certs-dir=/certs --host=127.0.0.1:26257 --file=/bootstrap.sql
```

This preserves TLS verification: the generated SQL node certificates include
`127.0.0.1`. If initialization succeeds but SQL setup fails, retry only the SQL
command. Never rerun `init` against an initialized cluster.

K3s servers retry until SQL is ready. Forward the API through a server in a
separate terminal:

```bash
ssh -N -L 127.0.0.1:6443:127.0.0.1:6443 core@REACHABLE_SERVER_IP
```

Then fetch an admin kubeconfig privately:

```bash
umask 077
ssh core@REACHABLE_SERVER_IP 'sudo cat /etc/rancher/k3s/k3s.yaml' > state/kubeconfig
export KUBECONFIG="$PWD/state/kubeconfig"
kubectl get --raw=/readyz
kubectl get nodes -o wide
```

Install Cilium using the [K3s-specific instructions](https://docs.cilium.io/en/stable/installation/k3s/)
and the configuration already tested in [the QEMU lab](../../QEMU-LAB.md): keep
kube-proxy enabled, pod CIDR `10.42.0.0/16`, CNI binaries `/opt/cni/bin`, and CNI
configuration `/etc/cni/net.d`. The API tunnel lets your workstation reach the
API; Cilium agents must use an API endpoint reachable from the nodes. Wait for
all nodes to become Ready, then run the repository's pod/DNS/service smoke tests.

## Persistence and changes

Each VM uses a persistent Premium SSD managed OS disk. CockroachDB's
`/var/lib/cockroach` currently lives there, as in the existing generator.
There is no ephemeral database disk and no automatic disk formatting. A separate
database data disk with a tested attachment/recovery procedure remains future
work; this is not yet a production storage profile.

VMs have Pulumi resource protection enabled; changes that require replacement
are blocked. Their managed OS disks use Azure's `deleteOption: Detach`.
These settings do not protect against deletion through Azure itself or resource
group deletion outside this state. Keep backups independent of these machines.

Ignition is applied only on first boot. Changes to `osProfile.customData` are ignored on
existing VMs, so adding a node does not replace every existing VM when the join
list changes. Existing CockroachDB nodes discover peers through the cluster.
Changing role, tokens, certificates, or service settings on running hosts needs
an explicit update procedure; regenerating Ignition does not update them.
Autoscaling, automatic repairs, OS upgrades, and coordinated Flatcar/database
maintenance are not configured here.

Ignition includes private keys and the join token. The Go program wraps custom
data in a Pulumi secret, so Pulumi encrypts it in state and redacts normal output.
The original `.ign` files still contain plaintext credentials; keep the state
passphrase and all generated files private. Local stack settings, saved plans,
state, generated credentials, and kubeconfig are ignored by Git. Use an
access-controlled backend before collaborating on deployments. Do not upload
CA private keys or root client credentials to Azure VMs.

## Validation status

Go tests cover configuration validation and mocked Azure resource registration:
three servers plus one worker, private/public SSH modes, bad subnet/zone inventory,
pod-network overlap, missing SSH source restrictions, unpinned images, stale or
oversized Ignition, worker database defaults/opt-out, secret custom data, disk
retention, and VM protection. They invoke the actual Python generator using
temporary fake credentials. No generated Ignition fixtures or live secrets are
committed, and mocks do not create resources or authenticate to Azure.

From this directory:

```bash
go test ./...
```

For the generator and QEMU helper tests, from the project root:

```bash
python3 -m unittest discover -v
```

Go mock tests require Python 3 but do not require the Pulumi CLI. These checks do not prove Azure image
availability, subscription quota, actual first boot, database initialization,
Cilium networking, or VM replacement recovery. **No Azure deployment has been
performed or verified.**

## Vendor references

- [Flatcar on Azure, Marketplace images, and Terraform custom data](https://www.flatcar.org/docs/latest/deploy/cloud/azure/)
- [Azure Flexible VMSS and individually configured VMs](https://learn.microsoft.com/en-us/azure/virtual-machine-scale-sets/virtual-machine-scale-sets-attach-detach-vm)
- [Explicit outbound connectivity with NAT Gateway](https://learn.microsoft.com/en-us/azure/nat-gateway/nat-gateway-design)
- [Pulumi Go SDK](https://www.pulumi.com/docs/iac/languages-sdks/go/)
- [Pulumi mock tests and their limits](https://www.pulumi.com/docs/iac/guides/testing/unit/)
- [Pulumi Azure Native provider](https://www.pulumi.com/registry/packages/azure-native/)
