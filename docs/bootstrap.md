# Cluster bootstrap

The inventory must identify exactly one database-hosting server whose name ends
in `-0`. That node alone may initialize CockroachDB and perform initial Kine SQL
setup. Selection is independent of inventory order. Other nodes join and wait;
they do not initialize themselves. There is no automatic initializer failover.

## First boot

Ignition installs node configuration, credentials and systemd services. A pinned
Go container builds the helper from an immutable source commit before enrollment.
Host Tailscale then enrolls the node and supplies its runtime address. CockroachDB
and K3s start after host preparation; the container uses Quad100 for tailnet DNS.

The initializer checks peer database listeners and queries for an existing cluster
identity. It records initialization intent before the initial request. A saved
cluster ID prevents accepting a different database identity. The present marker
lives on the initializer's persistent disk; losing that disk is an unresolved
replacement/recovery case, not permission to initialize again.

## Temporary SQL credential

A separate root client certificate/key has a one-hour lifetime and is delivered
only to the initializer's private profile. The CA signing key and long-lived
administrator key remain on the workstation. The node validates the temporary
credential and stages it in root-only `/run/infestation`, mounted read-only into
the host database container for SQL setup.

After Kine SQL setup succeeds, the node persists `sqlReady` and removes both
source and runtime copies of the temporary credential. Failed attempts can retry
within its lifetime. Subsequent starts use Kine credentials. Expiry requires
deliberate credential recovery; resume must not silently issue a new CA/token.
Deleting guest files does not remove copies from generated Ignition/provider
state and does not revoke the certificate.

## Access and publication

K3s generates its own serving/client credentials. Each server publishes a separate
root-only `/etc/infestation/kubeconfig`, replacing the server URL with the shared
API Service and retaining normal CA, hostname and client authentication checks.
The publisher retries and refreshes after source credential changes. Retrieval
uses Tailscale SSH with `core` + sudo. Local QEMU diagnostic/access channels are
also available for boot inspection.

## Verification boundary

Tests cover initializer selection, initialization intent, identity mismatch,
credential scope/lifetime, SQL retries and cleanup. A pinned database integration
test can be run explicitly:

```sh
INFESTATION_DOCKER_TEST=1 go test ./internal/nodebootstrap \
  -run '^TestPinnedSQLBootstrap$' -count=1 -v
```

The [mixed test](mixed-cluster.md) exercised the distributed topology after
repairs. Fresh unattended provisioning, initializer replacement, lost-marker and
quorum-loss recovery remain outstanding in [TODO.md](../TODO.md).
