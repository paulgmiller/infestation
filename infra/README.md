# Infrastructure providers

QEMU and Azure are the current VM compute providers. The intended architecture
supports any compatible provider through a shared VM/disk/Ignition/egress contract,
preferably implemented with Pulumi. See [architecture](../docs/architecture.md).

The Pulumi entry points use the root Go module. Preserve the project IDs
`flatcar-k3s-azure` and `flatcar-k3s-qemu` so stack ownership and resource URNs do
not change during refactoring. No source cleanup should apply infrastructure or
replace disks automatically.

Current operational guides: [QEMU](qemu/README.md), [Azure](azure/README.md), and
[the mixed test](../docs/mixed-cluster.md). Outstanding consolidation work is in
[TODO.md](../TODO.md).
