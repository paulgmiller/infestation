# Initial cluster bootstrap

For now, only the server VM whose inventory name ends in `-0` may initialize
CockroachDB or run the initial Kine SQL setup. The default inventory names it
`kube-0`; Azure Uniform pools use IMDS instance ID `0` in the API pool,
normalized to `api-0`. Worker instance `0` is never an initializer.
There must be exactly one eligible `-0` server, and it must host CockroachDB.
Selection does not depend on inventory ordering and there is no generated
initializer-role flag. Other nodes join and wait; they never initialize themselves.

This intentionally keeps initial deployment simple. If `kube-0` never starts,
bootstrap waits/fails until it is brought up. There is no automatic fallback to
another fresh database node. After successful bootstrap, CockroachDB has no
special dependence on ordinal zero for normal database leadership.

The initialization request always targets that VM's private DNS name (or static IP) and original
Cockroach store. A saved initialization marker/cluster ID protects against
accidentally initializing another cluster after a timeout or loss of quorum.
Existing inventories/PKI are not renamed automatically: an older inventory
without a `-0` server needs a deliberate migration or a new deployment plan.

## Initial SQL credential

Deployment issues a separate root client certificate/key with a one-hour lifetime
using the existing Cockroach CA. Static inventories put it only in `-0` Ignition; Uniform VMSS puts it in the
shared API profile because all API instances use one template. Nonzero API
instances delete their copy; workers never receive it. It is stored under
`/etc/infestation/bootstrap-client`, with file mode `0600`. The CA signing key and
long-lived administrator key remain on the deployment workstation.

Before initial SQL setup, the node verifies the temporary certificate against its
CA, checks its validity and short lifetime, and stages it in `/run/infestation`.
That root-only directory is mounted read-only into the Cockroach container for
local CLI operations. After Kine SQL setup succeeds, the node first persists the
`sqlReady` marker, then removes both source and runtime credential files. Failed
SQL attempts retain the credential for retry within its lifetime. Subsequent
starts use Kine's ordinary certificate and do not require the deleted root key.

The one-hour lifetime starts when Ignition is generated. If it expires before
setup completes, fresh credentials must be deliberately delivered to `-0`;
there is no automatic renewal of the temporary root certificate. Regenerating
local Ignition does not update first-boot customData on an existing Azure VM.
Generated profiles and secret Pulumi/Azure customData can retain the expired
credential after its node files are deleted; deletion is not certificate revocation.

Cluster-ID probes opt in to `allow_unsafe_internals` only for the helper's
read-only identity-query sessions on the pinned Cockroach version. They do not
change cluster settings or Kine's application-session defaults.

## TODO: remove the initial dependency on ordinal zero

A renewable Azure Blob lease would improve initializer selection when ordinal
zero cannot start. Lease acquisition should choose and persist a single init
target; losing a lease must not authorize initializing another fresh store after
an ambiguous init request. Blob lease ownership cannot fence an already-running
Cockroach RPC.

Keep that future coordination behind a provider-independent interface. AWS S3
and Google Cloud Storage have conditional object writes; on-prem may have no
object store at all. Azure Blob leasing must not become a mandatory dependency
of shared bootstrap logic. No lease implementation is included for now.

## Validation boundary

Unit tests cover target selection, credential scope/lifetime, SQL retries and
cleanup. An opt-in Docker test against `cockroachdb/cockroach:v26.2.7` initializes
a fresh secure store, runs Kine SQL setup with the temporary credential, removes
it and verifies a subsequent start using Kine credentials:

```sh
INFESTATION_DOCKER_TEST=1 go test ./internal/nodebootstrap -run '^TestPinnedSQLBootstrap$' -count=1 -v
```

The test uses a cached image, no networking or host ports, and disposable data.
It does not verify three-VM membership, Flatcar first boot, Azure managed identity,
Cilium or Azure VM provisioning without SSH credentials. A fresh Azure deployment
and failure-recovery drill remain to be performed.
