# Infestation contributor instructions

Infestation is one Go module at this repository root. Read README.md first.

- `cmd/infestation` is the CLI; portable logic lives under `internal/`.
- Read `docs/goals.md` and `docs/architecture.md` for the intended provider
  boundary. QEMU and Azure are the current providers; any compatible VM compute
  provider should be supportable through Go libraries. Shared bootstrap
  must not depend on a provider's object storage, private DNS or remote execution.
- `internal/cluster`, `pki`, and `ignition` are shared node preparation.
  `internal/vm` currently manages QEMU processes/QMP and Azure ARM/CLI lifecycle.
  The target is Azure Go SDK and go-qemu behind a shared Go provider contract.
  Pulumi and its legacy deployment paths are retired. Preserve saved ownership,
  disks and private state; refactoring is not a reason to replace live resources.
- CockroachDB runs as a TLS-enabled host Docker service, outside Kubernetes.
  K3s servers use Kine/CockroachDB; workers run agents. Database hosting defaults
  on for both roles. Use host Tailscale and K3s's native Tailscale/Flannel
  integration with kube-proxy and CoreDNS. Do not substitute a separate overlay
  or SQLite/etcd. Host containers must use working tailnet DNS explicitly.
- Nodes need egress, not a shared/connected underlay or public ingress. Keep
  provider-specific NIC/subnet/egress requirements explicit. Follow the open
  egress decision in TODO.md rather than silently adding a load-balancer dependency.
- PKI uses Go crypto; Ignition uses official v3.3 types/validation. Never overwrite
  an existing CA or token. Root/admin credentials and CA signing keys stay on the
  workstation and must not enter node profiles. The explicit bootstrap exception
  is a separately issued one-hour SQL root client certificate/key, delivered only
  to the explicitly selected `--init` server, or to a shared API profile where only instance 0
  may use it and other instances remove their copies. Shared node leaf credentials
  are permitted for pooled profiles and independent tailnet enrollment; never substitute admin/CA signing keys.
  Remove temporary credentials after SQL setup succeeds.
- Before the v1 tag, backward compatibility is not required. Remove obsolete
  interfaces and formats without compatibility shims or retired-profile guards.
  Continue protecting resource ownership, VM disks and private state.
- Use `go test ./...` and `go vet ./...` for local validation without provisioning.
  Add meaningful tests for identity, storage, bootstrap/recovery guards and
  infrastructure lifecycle changes. Distinguish mocks from live verification.
- No provisioning, disk formatting, remote deployment, database initialization,
  or destructive infrastructure operations without an explicit user request.
  Refactoring permits local source/state-path migrations, not recreating deleted
  cloud resources or starting stopped VMs. Preserve local VM disks and private
  state unless deletion is explicitly requested.
- Keep state, credentials, generated Ignition and kubeconfigs ignored/private.
  Do not log keys, auth URLs, raw daemon preferences or kubeconfig contents.
- `api_tailnet_hostname` declares a shared SAN and host API Service forwarder on
  servers only. Workers must not advertise it. Keep raw TLS passthrough to
  localhost:6443, not a ClusterIP. Defining the tailnet Service and its approval/
  access policy is separate from node configuration.
- The tailnet is the administrator boundary. Routine Tailscale SSH uses `core`
  with sudo. Keep published kubeconfig root-only with normal Kubernetes TLS and
  client authentication. Do not edit K3s's managed kubeconfig in place.
- TODO files contain outstanding work only. Put test evidence in runbooks and
  keep documentation focused on the current design. Reboot coordination and
  managed Kubernetes services remain unimplemented. Cite vendor sources for
  version-specific claims.
