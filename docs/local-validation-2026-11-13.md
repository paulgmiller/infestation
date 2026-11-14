# Local lifecycle/provider/export validation, 2026-11-13

This record covers local source, unit tests and export generation. No live
cluster deployment, database membership change, VM start/stop, or Proxmox import
was performed during this work.

The three priority tasks used separate worktrees from a temporary clone because
the original workspace's Git metadata is read-only:

| Task | Worktree | Task commits |
| --- | --- | --- |
| Quorum investigation | `/tmp/infestation-quorum` | `17cb215` |
| Guarded database removal | `/tmp/infestation-removal` | `cdc7aa4` |
| Shared provider contract | `/tmp/infestation-provider` | `e07583c`, `ee118db` |

Their changes were integrated into the original working directory without
modifying its Git history. Integration also routes CLI removal through the
provider contract and removes obsolete removal-signature wrappers. Proxmox
export and its tests/docs were implemented in that working directory.

## Checks

Using `GOCACHE=/tmp/infestation-go-cache` avoids the read-only home build cache.

- `go vet ./...` passed.
- `go build -buildvcs=false -o bin/infestation ./cmd/infestation` passed.
  Disabling build VCS stamping avoids Go's attempt to write version metadata
  into the read-only module cache; it does not change source selection.
- `go test ./...` passed all tests except three existing QEMU socket tests:
  `TestQEMURemovalRetainsPrivateStateAndOptionalDisks`,
  `TestQEMURemovalQuitsVerifiedProcess`, and
  `TestQEMURemovalAfterPartialCreate`. Their Unix socket listen/dial operations
  were denied with `operation not permitted` by this environment.
- Running the full suite with exactly those three tests excluded passed.
- Export tests now cover join-only profiles, private permissions, required auth
  keys, refusing overwrites, VHD conversion arguments, backing/external storage
  refusal and conversion failures. Disk/command fixtures are mocked and not
  bootable guests. The earlier Python/shell wrappers and template flow were
  removed in favor of a Go-only export and manual setup instructions.
- New lifecycle/provider tests use mocked provider/CLI responses. They establish
  guard ordering, identity checks, strict batch command selection and retained
  resources on failures; they do not establish live allocator convergence.
- `git diff --check` passed after integration.

## Proxmox export

The current command exports a dynamic VHD and private Ignition containing the
supplied enrollment key, with checksums and non-secret manifest metadata. No
Python/shell helper scripts or credential-free templates are generated. See
[manual Proxmox setup](friend-vm.md#proxmox). Generated disks and profiles are
ignored and are not repository source. No Proxmox boot, tailnet enrollment,
workload readiness or adequate performance is claimed.

The earlier script-based example artifacts were superseded by this flow. Their
local disk/checksum validation does not constitute validation of a VHD conversion
or of a Proxmox boot.

## Azure SDK integration

Published master `0ba5f27` (Azure typed SDK bindings) was merged with the local
lifecycle/provider work in `/tmp/infestation-integration`, commit `0272682`, then
the resolved source delta was applied to the original working directory. The
original workspace's read-only Git metadata prevents moving its branch reference
or recording the merge there. Source integration preserves typed SDK ownership
and instance inventory checks, guarded database removal, and explicit subscription
propagation through the prepared provider request.

## Remaining limits

Continuation checks restored the missing `internal/retirement` protocol source
from the integration worktree. `go vet ./...` and the complete `go test ./...`
suite passed; the latter required execution outside the sandbox to permit Unix
socket tests. No VM was provisioned or started. The workstation removal path now
delegates to a guest `retire` command, but that command is not yet implemented.
Normal removal fails closed until the guest implementation is completed.

Historical evidence still cannot establish which suspect predicate repeatedly
excluded s5 or map all lost voter IDs. The investigation remains open.
Normal removal requires every target's identity reachable, even on retries after
partially successful compute deletion. Kubernetes workload draining and API
withdrawal remain outstanding. Public temporary stop currently refuses because
the whole-batch quorum check and temporary database drain are not implemented.
Durable off-workstation credential custody is still a separate design task.
