# Codex instructions: Flatcar / CockroachDB / K3s bootstrap

## Mission
Turn this **unverified starter scaffold** into a reproducible, tested, safe-to-try
three-node Flatcar Linux lab deployment. Read README.md and the generator before
making changes. Explain any assumptions and cite vendor documentation for version-
specific behavior.

## Intended architecture
- Three separate x86-64 Flatcar nodes (initially kube1/kube2/kube3); machine
  addresses and SSH key are set in cluster.json, not hardcoded elsewhere.
- CockroachDB runs outside Kubernetes, as a host-level Docker container managed
  by systemd, with durable local storage and a TLS-enabled three-node cluster.
- K3s runs as a control-plane server on each machine; all three use the same
  logical CockroachDB datastore via Kine, with local SQL endpoints initially.
- No embedded etcd, SQLite, or Flannel. kube-proxy stays enabled initially.
- The approved networking transition is recorded in NETWORKING-PLAN.md. Cilium
  is being retired; evaluate K3s's Tailscale/Flannel extension integration before
  writing custom PodCIDR glue. The generator still leaves Flannel disabled until
  that integration is implemented and verified.
- Private API access uses one host-level Tailscale Service/TailVIP advertised by
  API servers only, forwarding raw TCP to localhost:6443, never to a ClusterIP.
  Workers must not advertise it. Keep host networking independent of Kubernetes.
- Flatcar provisioning is declarative and repeatable via generated Ignition
  configurations. Keep host networking independent of Kubernetes.

## First assignment: audit and strengthen this scaffold
1. Audit generate.py, bootstrap-db.sh, cluster.json, README.md, and emitted
   Ignition syntax against current Flatcar, CockroachDB, and K3s documentation.
   In particular, check the K3s system-extension URL, mount points, sysext
   availability at first boot, systemd unit path/enablement, and prerequisites.
2. Check CockroachDB CLI/certificate flags, file ownership, cluster
   initialization, Kine SQL permissions, and K3s datastore TLS arguments.
3. Check systemd dependency ordering and failure/retry behavior; make sure
   CockroachDB does not rely on K3s or the CNI to start.
4. Add useful automated tests for generator output and validation (prefer Python
   standard library where sufficient); include Ignition schema/Butane validation
   where tooling is available. Clearly distinguish tests you actually ran from
   checks requiring Flatcar VMs.
5. Document a repeatable disposable-VM smoke test that starts three nodes,
   initializes CockroachDB once, starts all K3s servers, then checks cluster
   readiness after replacement networking is installed. Never claim it passed unless run.
6. Preserve the small/simple architecture. Implement the networking checkpoint
   in stages; distinguish saved plans, generated configuration, and live verified
   deployment. Do not add unrelated HAProxy or auto-reboot management.

## Safety and working conventions
- Treat this repository as a prototype; **no machine provisioning, disk
  formatting, remote deployment, `cockroach init`, or destructive command**
  without the user's separate explicit approval.
- Never commit generated Ignition, credentials, CA material, or token files.
  Keep pki/ and output/ ignored. Do not put live secrets in examples.
- Don't weaken TLS or run CockroachDB in insecure mode just to simplify setup.
- Check image versions and URLs instead of assuming this scaffold's pins work.
- Prefer targeted patches with explanatory commit-friendly diffs; report
  verification performed, blockers, and remaining risks.
- Do not assume Codex can see the ChatGPT conversation; this repository and
  README.md are the source of truth.
