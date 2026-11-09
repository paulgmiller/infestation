# Root Go migration (2026-10-26)

The former `flatcar-crdb-k3s/` directory is now the repository root. The module,
CLI, and product are **infestation**. Python generators, planners, bootstrap and
deployment scripts have been replaced by Go packages. The older direct-QEMU
migration script is retired; its test record is retained as historical context.

| Previous tool | Current command |
| --- | --- |
| `generate.py pki` | `infestation pki` |
| `generate.py ignition` | `infestation ignition` |
| Azure `plan.py` | `infestation azure plan` |
| Azure `deploy.py` | `infestation azure deploy` |
| Azure `bootstrap.py` | `infestation bootstrap` |
| QEMU `prepare-image.py` | `infestation image` |
| `qemu-lab.py cross-smoke` | `infestation smoke` |
| Copy admin config through SSH | `infestation kubeconfig` |

Configuration field names remain compatible. Azure and QEMU now share
`internal/cluster.Config` and `Node`. Both providers' mock tests generate profiles
through `internal/ignition`; tests no longer spawn Python. Native PKI emits
Cockroach-compatible RSA PEM files with the same filenames and CN identities.
It never overwrites existing PKI. Ignition is validated against official v3.3
schema/types before publishing it, and existing join tokens are preserved.

## Local state

Ignored state moved with `infra/`, including keys, PKI, images, local Pulumi
backends, passphrases, and helper scripts. The libvirt pool and VM disks under
`/var/lib/libvirt/images` were not moved. The stopped VMs were not started, and
the deleted Azure group was not recreated.

Absolute local paths in ignored stack configs and plan ownership records were
updated to the new source location. Archived backend records/logs may still
contain old paths; do not edit resource identities or encrypted historical state
by string replacement. Use a reviewed refresh/preview before future applies.

Private `state/run.py` helpers from the earlier session remain as local archive
conveniences; current source and supported commands do not depend on them.
Existing helper SQL paths and cross-smoke calls were redirected to the new code.
For fresh deployments, use the Go binary and the documented backend settings.
Back up the private state/passphrase before changing tooling or resource options.

## Pulumi compatibility

Keep the existing project IDs and provider versions. Azure uses Azure Native
3.29.0. QEMU uses terraform-provider bridge 1.5.0 parameterized with libvirt
0.9.9. The QEMU adapter preserves tokens, resource names, permission/protection
settings, and runtime PTY path exclusions. No generated libvirt SDK or
Terraform/OpenTofu executable is needed to compile the Go project.

Ordinary tests and previews do not establish that newly generated native-PKI
profiles boot successfully. Earlier VM smoke tests used the previous tooling;
a fresh native-Go deployment and fault-recovery test remain outstanding.

## Verification performed

- `make check`: all root-module tests, `go vet`, and formatting checks passed.
- `go test -race ./...`: shared packages and Pulumi mocks passed.
- The built CLI completed native PKI and schema-validated Ignition generation
  without Python, Docker, or OpenSSL for those operations.
- CockroachDB 26.2.7 `cert list` accepted the native Go CA, node and Kine
  certificates in a network-isolated CLI container; no database was started.
- The native Go Azure deployment command completed a preview of 17 resources;
  no Azure resources were provisioned.
- A real QEMU refresh/preview reported 14 unchanged resources after the move;
  no VMs were started, stopped, replaced, or deleted by the refactor.
