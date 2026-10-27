# Research and learning questions

Recorded 2026-10-27 after the Azure Uniform VMSS bootstrap and scale-out tests.
This is a research backlog, not a set of implementation decisions. Start with
upgrade design before another replacement experiment. Existing target decisions
are in [operations-plan.md](operations-plan.md); test evidence and teardown
history are in the [Azure runbook](../infra/azure/RUNBOOK.md).

## Questions requested by the user

### 1. Container images and data on ephemeral storage

Can we move container images and container data onto Azure's local temporary
storage to reduce managed OS disk usage and cost?

- Identify the actual paths and ownership of K3s/containerd images, snapshots,
  writable layers, host Docker images/data, logs, and pod ephemeral volumes.
  Which paths can be relocated safely on Flatcar, and in what startup order?
- Separate reproducible image/cache data from application/database state. Which
  data may disappear with a VM, and which needs replication, backups, or durable
  volumes? How should local-path persistent volumes be treated?
- Can image layers and Cockroach stores share the temporary disk, or should
  space, I/O and failure budgets be isolated? How do image GC and disk-pressure
  eviction avoid starving the host database?
- Measure cold-start pull time, registry limits, disk usage and rebuild behavior
  after resource-disk loss. What disk size is actually required, and can the OS
  disk shrink safely? Keep OS-disk and temporary-data-disk choices distinct.
- What are the equivalent disk types and loss guarantees on AWS, Google Cloud
  VMs, and local QEMU? How do SCSI and NVMe setups differ?

### 2. K3s cloud-provider integration

Can we add the external Azure cloud-controller-manager to K3s, with equivalent
provider integrations later for AWS and Google Cloud VMs?

- What K3s flags, provider IDs, VMSS configuration and bootstrap ordering are
  required? How are cloud node initialization, addresses and topology labels set?
- Can Services of type LoadBalancer manage public/internal Azure frontends with
  our private nodes? What routing, firewall, health-probe and identity permissions
  are required?
- Who owns each load balancer, backend pool, outbound rule and NSG rule: Pulumi
  or the controller? How do we prevent conflicting reconciliation of the API/
  egress SLB and application Services? Can one SLB serve the intended topology?
- Which controllers are needed with the chosen CNI, and which are unnecessary?
  Treat disk CSI integration as a separate question from cloud load balancing.
- Can managed identity provide narrowly scoped access without long-lived cloud
  credentials? What changes across Azure/AWS/Google and on-prem?

### 3. Tailscale Funnel and private operator access

Can Tailscale Funnel provide ingress for applications and/or the Kubernetes API,
possibly with one Funnel forwarding to a Cilium ingress or Gateway?

- Compare public Funnel ingress with private tailnet access/Serve. Which users
  and endpoints need to be public, and which should be reachable only by operators?
- Can one Funnel front one ingress/gateway and route multiple applications?
  How would the API endpoint fit alongside them? Check hostname, port, protocol,
  certificate and service-limit constraints rather than assuming they fit.
- Where does TLS terminate? Can kubeconfig certificate authentication and API
  authorization remain correct? Test watch, exec, logs and port-forward streams;
  do not solve certificate issues by disabling TLS verification.
- Where does the Tailscale process run: host, pod, operator-managed proxy, or
  another gateway? What is the failure domain of a single Funnel, and how would
  ingress survive a node or zone loss?
- How are node enrollment, machine identity, keys/tags and permissions handled
  during replacement? How are retired machines removed from the tailnet?
- How would Tailscale SSH/bastion access support debugging without public node
  IPs or reopening host SSH? Avoid making first bootstrap depend on operator VPN
  access. Region-qualified naming and operator-controlled traffic cutover remain
  part of the existing target design unless explicitly revised.

### 4. Simpler CNI and gateway stack

Is Cilium more complexity than this cluster needs? Compare it with Flannel or
another simpler CNI, NetworkPolicy enforcement, and a gateway such as Traefik.

- List the features we actually need: connectivity, policy, ingress/Gateway API,
  observability, encryption, service routing, cloud integration and failure behavior.
- NetworkPolicy is an API; which component would enforce it with each candidate
  CNI? Do not assume a simpler overlay implements policy by itself.
- Compare a Flannel/policy-controller/Traefik combination, other simpler CNIs,
  and the Cilium setup (historically kube-proxy enabled; new profiles use
  Cilium replacement, pending deployment verification). Is using several smaller
  controllers simpler to operate than the existing stack?
- Measure idle and loaded CPU/memory, startup time, image footprint, debugging
  effort and upgrade risk on the 2-vCPU/8-GiB baseline.
- How do ingress versus Gateway API, TLS handling, LoadBalancer Services and
  Tailscale ingress work with each option? What changes in the cloud-provider setup?
- How would an existing cluster migrate safely, or should a CNI change require
  a replacement cluster? Cilium remains the tested baseline for comparison.

### 5. Continuous Flatcar/K3s/Cockroach maintenance

Could kured, system-upgrade-controller, Flatcar update tooling, or a custom
controller coordinate upgrades as new versions become available, including
Kubernetes drain and graceful Cockroach handling?

- What does each tool coordinate: detecting releases, staging artifacts,
  rebooting, updating a system extension, upgrading a DB process, or replacing
  whole VMs? Are separate mechanisms needed for Flatcar, K3s and Cockroach?
- Define the difference between a temporary reboot with the same store and a
  permanent VM/store replacement. When is Cockroach drain appropriate, and when
  must a member be fully decommissioned? Avoid assuming every reboot should
  evacuate all replicas.
- Can pre/post-maintenance hooks safely manage a host Docker/systemd database
  outside Kubernetes? How are automatic service restarts prevented from undoing
  the maintenance action?
- Who owns the maintenance lease and durable journal? How are retries, crashes,
  interrupted drains/decommissions, concurrent operators and lost leases fenced?
- What compatibility/skew and release-channel policies apply to each component?
  How are artifacts pinned/verified, and what constitutes a safe rollback after
  Kubernetes changes or Cockroach cluster-version finalization?
- Which availability checks, PDBs, local-volume checks, replication thresholds,
  zone constraints and spare capacity must pass before the next destructive step?
- Should orchestration run inside Kubernetes, outside it, or both? How can it
  recover when the API itself or its Cockroach datastore is unhealthy?

## Additional questions from testing

### Rolling replacement and state ownership

- Can the Go workflow update a pool template, surge one VM, wait for both K3s
  and Cockroach readiness, then retire exactly one old VM at a time?
- How are Azure instance ID, IMDS identity, Kubernetes Node name and Cockroach
  node ID mapped and verified before any deletion? How do ID reuse and old K3s
  registration secrets affect replacement?
- How do runtime discovery and readiness support a temporary fourth API server,
  zero workers, and eventual removal of api-0 without permitting fresh database
  initialization on another node?
- How are stale private DNS records removed? A record's cache TTL is not a
  retirement workflow. What happens if cleanup is interrupted or an IP is reused?
- How are out-of-band VMSS changes reconciled into the saved plan, stack config
  and Pulumi state so a later apply cannot accidentally scale in or downgrade?
- What are the required regional and SKU-family quotas, surge budget and zone
  capacity? What is the fallback when a replacement cannot be allocated?
- How do manual VMSS model upgrades, reimage and first-boot customData interact?
  Can we prove which profile/version each instance actually consumed?
- What must a resumable upgrade command record, validate and refuse to repeat?
  How do we recover after decommission succeeded but VM/DNS/Node cleanup did not?

### Bootstrap, certificates and credentials

Follow through on the [operations plan's bootstrap and recovery commitments](operations-plan.md#agreed-operations-design):

- Keep first initialization restricted to API instance 0 for now: what happens
  when that instance never appears? Would provider-independent coordination
  eventually replace the fixed target without making Blob Storage mandatory
  on-prem? How do we fence initialization after an ambiguous result?
- Once SQL readiness and cluster identity are durable, can every surviving API
  member safely publish kubeconfig/readiness after the original initializer is gone?
- How should shared wildcard node certificates rotate before expiry, and how
  are compromised pool identities handled without distributing the CA key?
  What tradeoffs would per-node leaf identities introduce later?
- What happens when a one-hour SQL bootstrap credential expires before initial
  setup finishes? What is the deliberate recovery path without SSH or accidental
  reuse of the long-lived administrator key?
- How are helper binaries, system extensions and images verified and rolled
  forward? How do profile versions and deployed helper versions stay observable?
- How do we refresh and recover published kubeconfigs after K3s certificate
  renewal without depending on the original workstation?
- Exactly which infrastructure configuration, K3s bootstrap/token/PKI inputs,
  Cockroach credentials and encryption material must be recoverable from
  protected storage? How do we reconcile the current workstation-only signing
  keys with recovery that must work after that workstation is lost? Define
  storage protection and operator access before choosing a delivery mechanism.

### Data safety, placement and recovery

These questions implement the [agreed availability and recovery targets](operations-plan.md#agreed-operations-design);
they do not change the targets or expand recovery beyond Cockroach databases.

- Prove one-zone survival for both the Kubernetes API and Cockroach, including
  workload placement and remaining CPU/memory capacity. Node readiness and
  successful connectivity tests alone do not establish this guarantee.
- What storage and decommission evidence is sufficient before deleting a VM?
  How do applications with local volumes or other database engines participate?
- Implement and measure the agreed backup/restore targets: all Cockroach DBs,
  including Kine's database, restored together in another region; 15-minute
  recovery-point and one-hour recovery-time targets, with one-day retention.
- How do we restore a consistent set of databases and matching K3s recovery
  inputs, choose safe restore parallelism, and hold application startup until
  its databases are actually ready? What component owns that readiness gate,
  especially while the Kubernetes datastore itself is being restored?
- What regional replication lag, backup-chain dependencies, credentials and
  recovery artifacts are necessary? How do delete locks and deliberate resource-
  group deletion interact with the recovery policy?
- How much data can meet those targets, and how do we exercise a real restore,
  rather than infer success from artifact-storage redundancy?
- Can an operator recover into a region with no pre-created compute or network
  using cloud credentials obtained through the provider portal, without the
  original workstation? Rehearse discovery, quota/feature availability,
  provisioning, restore and kubeconfig retrieval as one timed runbook.
- How do we publish new region-qualified endpoints and hand them to the
  operator for manual Traffic Manager/CNAME/routing changes? Measure the
  one-hour target through healthy endpoints and a usable kubeconfig; record
  application traffic cutover separately, as the operations plan specifies.
- How are the operator-supplied Cockroach license secret and automatic
  activation delivered, verified, renewed and restored? Eligibility and
  obtaining the license remain operator responsibilities.

### Resource accounting and operational evidence

Source: the [operations plan's host database and resource-budget proposal](operations-plan.md#agreed-operations-design).

- Measure Cockroach CPU, memory and disk I/O alongside K3s and real applications.
  Are the proposed 500m CPU / 2-GiB DB budget and 500m CPU / 1.5-GiB OS/K3s
  reservation adequate? How do CPU weights allow DB bursting while host memory
  limits, kubelet reserves and stand-in requests avoid double-counting its budget?
- With application resource requests optional, how do we handle demand that the
  scheduler cannot account for? Test memory pressure, eviction and OOM behavior
  while protecting the host DB and API availability.
- Are stand-in pods still the best way to expose host DB readiness/Service
  endpoints and reserve resources? Should the DaemonSet actually proxy traffic,
  or only represent the host listener through a Service? Define TLS ownership,
  port binding and readiness during SQL unavailability or decommissioning.
  Keep Kine's direct/local bootstrap path independent of pod/CNI readiness.
- Which signals should become alerting once the alerting system is chosen?
  Keep alerting deferred, but collect useful diagnostics and health evidence now.
- How do we retain useful serial/boot diagnostics without exposing generated
  Ignition credentials? Which records must remain private?

## Evidence to retain for each experiment

Record the exact versions/profiles, node and VM identities, initial/final pool
capacities, quota/cost assumptions, health gates, interruption/resume behavior,
and cleanup result. Separate source/unit/mock checks, successful fresh bootstrap,
scale-out, upgrade/replacement, reboot, zone failure and regional restore tests.
The last Azure test established fresh bootstrap, worker scale-out, role-label
reconciliation and directed connectivity; it did not establish safe upgrades,
decommissioning, reboot recovery or disaster recovery.
