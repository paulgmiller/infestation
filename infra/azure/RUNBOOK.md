> Historical smoke-test record. The project is now the root Go module; see [migration notes](../../docs/migration.md) for current commands and preserved state.

# Azure smoke deployment, 2026-10-25

The current [deployment guide](README.md) supersedes this historical NAT Gateway
and persistent-database profile with SLB egress, temporary database storage,
automatic SKU/zone planning, and automated bootstrap. A separate private
`state/next` preview resolved `Standard_D2d_v4` with 75 GiB temporary disks and
three zones, and planned 17 resources. That revised profile has not been deployed.

**Deleted after testing:** `flatcar-azure-smoke-rg` and its resources were deleted
at the user's request on 2026-10-25. Azure's deletion wait completed and
`az group exists` returned `false`. The inventory and access instructions below
are historical. Local credentials and Pulumi state are retained; the kubeconfig
no longer connects. Refresh and review state before any future apply, which could
recreate the cluster.

The [deployment guide](README.md) describes a fresh installation. This records
the actual deployment and how to reconnect without creating another cluster.
The [local QEMU runbook](../qemu/RUNBOOK.md) records the preceding host test;
those three local VMs are shut off with disks preserved and `running: false`.

## Inventory and access

| Item | Deployed value |
| --- | --- |
| Subscription | North Briton |
| Resource group | `flatcar-azure-smoke-rg` |
| Region / Flexible VMSS | `westus2` / `flatcar-azure-smoke-vmss` |
| Pulumi project / stack | `flatcar-k3s-azure` / `smoke` |
| Servers | `kube1`, `kube2`, `kube3`, in zones 1, 2, 3 |
| Private IPs | `192.168.1.101`, `.102`, `.103` |
| VM size / OS disks | `Standard_D2s_v3` (2 vCPU, 8 GiB), 128 GiB Premium SSD each |
| Flatcar image | `kinvolk:flatcar-container-linux-corevm-amd64:stable-gen2:4757.2.1` |
| K3s / CockroachDB | `v1.35.8+k3s1` / `v26.2.7` |
| Cilium CLI / chart | `0.20.1` / `1.20.4` |
| Public API | `https://flatcar-azure-smoke-api.westus2.cloudapp.azure.com:6443` |
| API allowed sources | `0.0.0.0/0`, explicitly requested for this temporary lab |
| SSH allowed sources | `216.160.64.23/32` |

Run on the provisioning workstation from `infra/azure`:

```bash
export KUBECONFIG="$PWD/state/kubeconfig"
kubectl get nodes -o wide
kubectl get --raw=/readyz
kubectl get pods -A
python3 state/run.py pulumi stack output
python3 state/run.py pulumi preview --refresh --diff --stack smoke
```

The kubeconfig uses the public load balancer with context `azure-smoke`; no API
tunnel is needed. It embeds cluster-admin client credentials and is mode `0600`.
Copy it privately to another computer and retain those permissions. SSH remains
restricted independently; update `adminCIDRs` deliberately if that source changes.

For database administration, use `pulumi stack output publicIPs` through the
wrapper to find kube1's current public IP. The running session has a local SQL
tunnel on `127.0.0.1:26257`. If reconnecting later, start one in another terminal:

```bash
ssh -i state/id_ed25519 -N -L 127.0.0.1:26257:192.168.1.101:26257 core@KUBE1_PUBLIC_IP
```

Use `state/pki/admin` with the Cockroach CLI over that tunnel, retaining TLS
verification. The private `state/check.py db-status` helper uses that local port.

Private files are ignored by Git: `cluster.local.json`, `Pulumi.smoke.yaml`,
`state/backend`, `state/passphrase`, `state/pulumi-home`, `state/pki`, SSH key
`state/id_ed25519`, and kubeconfig. Keep private backups of state, encryption
passphrase, and credentials together. Do not regenerate PKI or rerun database
initialization to reconnect. The local `state/run.py` and `state/check.py` helpers
are private session conveniences, unavailable in a fresh checkout.

The wrapper selects this deployment's file backend and passphrase. It currently
reuses the pinned Pulumi CLI and Go caches under `../qemu/state`; retain those
tools or install the pinned CLI independently if that directory is removed.

## Lessons for a fresh deployment

1. Check subscription quota **and** SKU/zone availability before choosing VMs.
   West US 2 had only 10 total vCPUs and zero Dsv5-family quota. Three D2s v3
   instances fit the available DSv3 quota and listed all three zones.
2. Verify the exact regional Marketplace version. QEMU used Flatcar `4757.2.2`,
   while the verified Azure image was `4757.2.1`. CoreVM has the same image
   contents as Flatcar's other Azure offers, without purchase-plan metadata or
   Marketplace terms acceptance. See the [vendor guide](https://www.flatcar.org/docs/latest/deploy/cloud/azure/).
3. Generate a new SSH key, CA, node certificates, and K3s token for a separate
   cluster. Do not reuse the local lab's database identity.
4. Wait for SSH and CockroachDB on all three nodes. The Azure apply completed in
   about three minutes; SSH initially refused connections during first boot.
   Boot diagnostics confirmed successful Ignition processing.
5. Initialize the fresh database once, then apply `bootstrap.sql`. K3s retries
   until its Kine database, role, grants, and sequence setting exist. An early
   missing kubeconfig or database connection error can precede successful bootstrap.
6. Install Cilium after the API exists. Use the tested settings below and inspect
   the effective config: the CLI's auto-detection message suggested full kube-proxy
   replacement, but the explicitly configured effective value was `false`.

The tested installation command (run only on a fresh cluster) was:

```bash
../qemu/state/tools/cilium --kubeconfig state/kubeconfig install --version 1.20.4 \
  --set operator.replicas=2 --set kubeProxyReplacement=false \
  --set ipam.operator.clusterPoolIPv4PodCIDRList=10.42.0.0/16 \
  --set cni.binPath=/opt/cni/bin --set cni.confPath=/etc/cni/net.d \
  --set k8sServiceHost=192.168.1.101 --set k8sServicePort=6443
```

Cilium's configured bootstrap API address is currently kube1's private IP;
recovery from that endpoint being unavailable has not been tested. The public
load balancer provides the workstation API endpoint.

## API load balancer and provider behavior

The Standard Load Balancer passes TCP 6443 to the three server NICs, checks their
TCP listeners every five seconds, and uses a zone-redundant public IP. It has no
SQL or workload ingress rules. The NSG allows API clients separately from SSH
and permits probes from the `AzureLoadBalancer` service tag ahead of the custom
deny rule. The probe checks listener availability, rather than authenticated
Kubernetes `/readyz`.

For new clusters, add the DNS name to `cluster.json`'s `tls_sans` before generating
Ignition. For this already-running cluster, the generated K3s configuration was
installed privately on each node and K3s restarted one node at a time. Each API
passed `/readyz` before proceeding. TLS was then verified against all three node
addresses using the load balancer DNS name and the cluster CA, and through the
load balancer itself. No CA or client credentials were replaced.

Real Azure refreshes exposed derived networking fields:

- Public IP `natGateway` is maintained through the NAT resource.
- Inline NSG rule `etag` fields are Azure-managed metadata.
- LB backend address entries are derived from membership owned by NIC IP
  configurations. Preserve those entries when updating the LB; manage membership
  through the NICs.

The program ignores those specific fields while retaining managed networking
configuration. Saved-plan enforcement initially rejected a preview/apply pair
because refresh-derived fields differed. Pair a refreshed saved preview with a
refreshed apply:

```bash
python3 state/run.py pulumi preview --refresh --diff --save-plan=state/next-plan.json --stack smoke
python3 state/run.py pulumi up --refresh --plan=state/next-plan.json --stack smoke
```

Review the plan before applying. Protection blocks VM replacement; OS disks use
`Detach`. Existing VM custom data is ignored because Ignition runs only on first
boot. Changes to runtime configuration need a separate update procedure.

## Verified and untested

Verified: all three K3s nodes Ready, Cilium and Envoy 3/3, operators 2/2, effective
kube-proxy replacement `false`, six directed DNS/service HTTP jobs passing, and
three live CockroachDB nodes with zero unavailable/under-replicated ranges.
The public kubeconfig passed `get nodes` and `/readyz` with TLS verification.
The Go tests and eight Python generator/helper tests passed.
The final Pulumi preview with refresh reported **19 unchanged resources**.

Azure reboot recovery, replacement recovery, deliberate API-backend failure,
and durability under faults remain untested. The VMs, disks, public IPs, NAT,
and load balancer incur charges while retained; stopping VMs alone does not remove
all those charges. Teardown requires a separate, deliberate operation that handles
Pulumi protection and detached disks; no automatic cleanup is configured.

## 2026-10-26 Uniform VMSS test (incomplete; teardown requested)

New test plan: `state/infestation-vmss-test`, stack `uniformtest`, resource group
`infestation-vmss-test-rg` in West US 2. Selected `Standard_D2d_v4` at
$0.113/node-hour: 2 vCPU, 8 GiB RAM, 75 GiB temporary disk and 32 GiB managed
Standard SSD OS disk. Regional quota was 10 vCPUs, so this test requested three
API instances plus one worker; no second worker was created.

Verified in Azure: the automatic-zone-placement subscription feature was already
registered. The versioned Pulumi resource
`azure-native:compute/v20260301:VirtualMachineScaleSet` is in provider 3.29.0's
schema. Azure accepted two Uniform pools and placed API instance IDs 0/1/2 in
zones 1/2/3; worker 0 was allocated in zone 1. Node NICs were private.
Kubernetes/SQL readiness and worker scale-out were **not verified**.

Live deployment exposed these issues, fixed in source:

- NSG priority must be at least 100. SSH deny now uses 100, internal allow 110.
- A Standard load balancer rejected mixed public/private frontends. Retain one
  public API/egress SLB. Workers discover a live API member through private DNS
  before each K3s agent start, after their host database can already start.
- Azure requires OS authentication configuration. Generate a throwaway Ed25519
  public key, immediately clear/discard its private key, and retain only the
  public key to satisfy the API. No user key is required. SSH remains masked and
  denied at the NSG.
- Flatcar rejected `/usr/local/sbin/check-cockroach-storage` as read-only during
  Ignition. The script now lives under `/opt/infestation`. This prevented the
  attempted guests from reaching Cockroach/SQL initialization. Corrected
  profiles were patched into the pools, manual instance upgrades requested,
  and reimages requested; corrected boot had not been verified at teardown.
- Manual VMSS reimage can reuse the instance's previous model. Apply the latest
  pool model to instances before reimaging. Prefer a fresh cluster for the next
  clean-first-boot test rather than relying on this interrupted recovery.
- Azure retail pagination uses explicit `prices.azure.com:443`; accept the
  default HTTPS port while preserving the hostname/userinfo checks.

Local checks passed: Go suite, vet, race checks on PKI/Ignition/node bootstrap,
provider mocks for both pools, wildcard/IMDS identity tests, and an isolated
Cockroach v26.2.7 test of wildcard TLS, SQL init, credential cleanup and subsequent
Kine authentication. The pinned node helper was uploaded manually during
first-boot diagnosis, so the complete CLI upload/first-boot ordering still needs
an unattended fresh-deployment test.

The user requested stopping the experiment and deleting the resource group.
The active deploy was terminated and the storage delete lock removed for that
teardown. Preserve local PKI/state and logs; they include secret material.
Failed Ignition boot diagnostics can dump full generated profiles: filter
console output to status/error lines and do not share the raw log. After cloud
teardown, the local backend is stale; refresh deliberately or use a new plan and
stack name tomorrow. Do not blindly recreate from the interrupted stack.

Teardown completed: Azure's `group wait --deleted` succeeded for
`infestation-vmss-test-rg`. Both Uniform scale sets were removed before the
remaining network cleanup. The local Pulumi update was canceled; no automatic
redeployment is running.
