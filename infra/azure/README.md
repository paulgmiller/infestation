# Azure provider

The [mixed-cluster runbook](../../docs/mixed-cluster.md) describes the tested
Azure VM alongside two local VMs. All three use the same Flatcar Ignition,
Tailscale connectivity, helper build and host database/bootstrap logic.

Azure supplies the VM, managed disk, NIC and VNet/subnet. The cluster does not
rely on that VNet connecting to any peer. The prototype used default outbound
access with inbound denied and no assigned public IP; an explicit egress design
is required before treating this as a deployment baseline.

The tested VM CLI provisions through ARM and the Azure CLI. Pulumi is the
preferred common provider layer, and the infrastructure package has project ID
`flatcar-k3s-azure`. Keep that ID and saved stack ownership stable during
consolidation. See [architecture](../../docs/architecture.md) and [TODO.md](../../TODO.md).


Azure commands take explicit `--region` and `--sku` inputs, defaulting to `westus2`
and `Standard_B2s`. `vm cluster create --azure` enables the mixed topology with
those defaults. The deployment/check commands use the same defaults; there is no
separate planning command or price/quota/SKU discovery. Azure reports unsupported
sizes or capacity failures during provisioning rather than silently choosing a
different size.
