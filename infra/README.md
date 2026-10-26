# Pulumi entry points

These small Go programs call the shared provider packages in the root Go module:

- [Azure](azure/README.md): `infestation/internal/provider/azure`.
- [QEMU/KVM](qemu/README.md): `infestation/internal/provider/qemu`.

The Pulumi project IDs (`flatcar-k3s-azure` and `flatcar-k3s-qemu`) intentionally
remain unchanged so existing stack URNs are preserved. There are no nested Go
modules or Python generators here. Run `make check` at the repository root.

See [architecture](../docs/architecture.md) and [migration notes](../docs/migration.md).
