# Cluster bootstrap

Apply the one-time [tailnet policy setup](tailnet-policy.md) first.
Exactly one founding node is explicitly authorized with `--init`:

```sh
infestation vm create --name local --init --size 2 \
  --tailnet-domain YOUR-TAILNET.ts.net --tailscale-auth-key-file secrets/tsauth
infestation azure create --name cloud --size 1 \
  --tailnet-domain YOUR-TAILNET.ts.net --tailscale-auth-key-file secrets/tsauth
```

These calls can run on different workstations. Each creates nodes on exactly one
provider and records only its own machines. Use distinct group names in the
shared tailnet. The first call authorizes its first node; the second call creates
no CA, cluster token or SQL bootstrap credential. Ignition passes `--init` only
to the designated helper. The flag grants authority, rather than forcing a fresh
database. Hostname suffixes do not grant authority.

The initializer waits for three listening database participants, including itself
and two peers, before the first init RPC. An explicit single-node lab profile is
the exception. Existing database identity and recorded intent prevent fresh init
on subsequent starts. If another founding CA is discovered, enrollment stops.
Never designate two independent initializers: a partition can hide the conflict.

`create` always installs CockroachDB and K3s. By default K3s runs as a server;
`--agent` selects a worker without an API server. `--agent --init` is rejected
before state creation, downloads or provisioning. Agents still host a database,
participate in the three-database bootstrap threshold and share enrollment
material. They never initialize SQL or advertise the API Service.

Azure uses one shared VMSS profile per create call. Before Tailscale enrollment,
the helper checks IMDS against the configured group/set/region, derives
`NAME-<instance-id>`, and updates the hostname, K3s and tailnet identities.
Instance 0 retains `--init` only when the group was created with it. All other
instances clear the init command and remove their temporary SQL root copies.
Database advertise addresses and locality are supplied after runtime discovery.
See [Azure VMSS](azure-vmss.md).

There is no automatic election or initializer failover. A future one-time election
must agree on a durable target; seeing three peers or ranking IPs is insufficient.

## First boot

Ignition installs node configuration, credentials and systemd services. A pinned
Go container builds the helper from an immutable source commit before enrollment.
Host Tailscale then enrolls the node and supplies its runtime address. CockroachDB
and K3s start after host preparation; the container uses Quad100 for tailnet DNS.

After enrollment, the helper discovers online `tag:roach` peers in the supplied
tailnet using `tailscale status --json`. It probes a helper-owned HTTP endpoint
on port **7444**, bound to a Tailscale IPv4 address. This is separate from K3s
(6443) and CockroachDB (26257). Policy must allow cluster peers to reach 7444;
no public listener, Tailscale API key or node-to-node SSH permission is needed.

`GET /v1/join` provides cluster settings, the K3s token, public database CA, shared
wildcard database node leaf/key and Kine client leaf/key. It never provides CA
signing keys, SQL root/admin client keys or enrollment auth keys. All tailnet
members are administrators by design. HTTP travels inside Tailscale/WireGuard;
the server rejects non-tailnet source addresses and responses disable caching.
Discovery uses the DNS name, tags and stable ID supplied by Tailscale, not an IP
ranking. See [Tailscale status fields](https://github.com/tailscale/tailscale/blob/main/ipn/ipnstate/ipnstate.go).

The joiner validates the bundle, pins it in root-only
`/var/lib/infestation/join.json`, installs the leaves, and supplies runtime
Cockroach join seeds and the K3s token before those services start. Existing
credentials and pins cannot be silently replaced. Every enrolled node serves
the bundle, so later joins can use another member when the initializer is down.
`GET /v1/node` reports membership identity for runtime discovery. Shared leaf keys
match the chosen administrator boundary; rotation/revocation needs further work.

The initializer queries peers for an existing database identity, records intent
before init and saves the resulting UUID. Ambiguous init results never authorize
another init request. The marker remains on its persistent disk; losing it is an
unresolved recovery case, not permission to initialize another database.

## Startup recovery and helper compatibility

Joiners may boot before the initializer. Failed enrollment retries every fifteen
seconds without creating founding credentials. Generated discovery profiles use
systemd `Upholds` from successful preparation services to their persistent
dependents, so enrollment recovery also revives canceled CockroachDB, K3s and
membership start jobs. Readiness and access publication are driven by timers;
completing oneshots are not continuously restarted. This requires systemd 249 or
later; see [systemd's dependency documentation](https://github.com/systemd/systemd/blob/main/man/systemd.unit.xml).
Stopping an upheld service alone is temporary while its preparation service
remains active. For node shutdown, use the provider's VM stop command.

K3s has a five-minute startup timeout, a sixty-second shutdown timeout and a
fifteen-second restart delay. A process that never signals readiness or finishes
shutdown is retried rather than left indefinitely activating. These limits do
not time out CockroachDB formation or authorize fresh initialization. See
[systemd service timeouts](https://github.com/systemd/systemd/blob/main/man/systemd.service.xml).

The renderer and helper share an explicit interface version, reported by
`infestation-node --contract-version` without loading credentials or configuration.
Enrollment checks it before accepting helper output. Compatible fixes may use
different source commits; incompatible interface changes require a contract bump.
Older helpers without this interface are rejected with a compatibility error.
Publish a compatible helper revision before using newly generated profiles;
the default published `master` must include these changes. Existing Ignition and
guest binaries are not updated by rebuilding the workstation CLI.

Database startup also validates required runtime variables before running Docker.
A seeds-only environment file is rejected with the missing variable's name.
Enrollment regenerates `/run/infestation/cluster.env` from persisted node settings
on reboot; manual edits there are temporary.

Local VM status includes dependency results, queued jobs, enrollment and membership
logs, the helper build identity, and peer discovery endpoint results. Database/API
TCP probes include successfully discovered members as well as saved nodes.
An open TCP listener does not establish SQL readiness or database membership.
See [the November bootstrap troubleshooting record](bootstrap-troubleshooting.md)
for the manually recovered cluster and remaining live verification.

## Temporary SQL credential

A separate root client certificate/key has a one-hour lifetime and is delivered
only to the initializer's private profile. The CA signing key and long-lived
administrator key remain on the workstation. The node validates the temporary
credential and stages it in root-only `/run/infestation`, mounted read-only into
the host database container for SQL setup.

After Kine SQL setup succeeds, the node persists `sqlReady` and removes both
source and runtime copies of the temporary credential. Failed attempts can retry
within its lifetime. Subsequent starts use Kine credentials. Expiry requires
deliberate credential recovery; retries must not silently issue a new CA/token.
Deleting guest files does not remove copies from generated Ignition/provider
state and does not revoke the certificate.

## Access and publication

K3s generates its own serving/client credentials. Each server publishes a separate
root-only `/etc/infestation/kubeconfig`, replacing the server URL with the shared
API Service and retaining normal CA, hostname and client authentication checks.
The publisher retries and refreshes after source credential changes. Retrieval
uses Tailscale SSH with `core` + sudo, or the helper’s tailnet-only
`GET /v1/kubeconfig` endpoint. That endpoint exposes the same admin credentials to
tailnet administrators; Kubernetes API traffic retains normal TLS. Agent helpers
forward kubeconfig requests to a discovered server. Use `<provider> kubeconfig
--name NAME --out FILE`; the CLI writes mode 0600 and prints only the path.
Local QEMU diagnostic/access channels are also available for boot inspection.

## Verification boundary

Tests cover independent enrollment, offline restart, conflicting enrollment CAs,
credential exclusion, endpoint source checks, initializer selection, initialization intent, identity mismatch,
credential scope/lifetime, SQL retries and cleanup. A pinned database integration
test can be run explicitly:

```sh
INFESTATION_DOCKER_TEST=1 go test ./internal/nodebootstrap \
  -run '^TestPinnedSQLBootstrap$' -count=1 -v
```

The [mixed test](mixed-cluster.md) exercised the distributed topology after
repairs. The independent two-call flow has not been VM-tested. Fresh unattended
provisioning, initializer replacement, lost-marker and
quorum-loss recovery remain outstanding in [TODO.md](../TODO.md).
