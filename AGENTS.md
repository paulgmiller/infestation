# Infestation contributor instructions

Infestation is one Go module at this repository root. Read README.md first.

- `cmd/infestation` is the CLI; portable logic lives under `internal/`.
- `internal/cluster`, `pki`, and `ignition` are shared by Azure and QEMU.
- `internal/provider/{azure,qemu}` defines Pulumi infrastructure; `infra/` holds
  thin entry points and stack configuration. Keep the existing Pulumi project IDs
  unless deliberately migrating state; changing the repository layout is not a
  reason to replace live resources.
- CockroachDB runs as a TLS-enabled host Docker service, outside Kubernetes.
  Servers run the K3s control plane through Kine/CockroachDB; workers run agents.
  Database hosting defaults on for both roles. Cilium is installed after the API
  is ready, with kube-proxy enabled. Do not substitute Flannel or SQLite/etcd.
- Azure uses SLB ingress/egress, three-zone placement and optional temporary
  Cockroach data disks. QEMU uses private NAT and persistent qcow2 overlays.
- PKI is generated with Go crypto; Ignition uses official v3.3 types/validation.
  Never overwrite an existing CA or token. Root/admin credentials and CA signing
  keys stay on the workstation and must never be embedded in node profiles.
- `make check` validates without provisioning. Add meaningful tests for identity,
  storage, bootstrap/recovery guards, and infrastructure lifecycle changes.
- No provisioning, disk formatting, remote deployment, database initialization,
  or destructive infrastructure operations without an explicit user request.
  A request to refactor the repository permits local source/state-path migrations,
  not recreating a deleted cloud cluster or starting stopped VMs.
- Keep state, credentials, generated Ignition and kubeconfigs ignored/private.
  Preserve existing local VM storage and state. Distinguish unit/mock/preview
  checks from actual deployment or failure-recovery verification.
- Tailscale, automatic reboot coordination, and managed AKS/GKE/EKS remain outside
  the current implementation. Cite vendor sources for version-specific claims.
