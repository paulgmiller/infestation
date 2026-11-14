# Replica replacement investigation, 2026-11-13

The captured evidence establishes a replacement-destination failure before the
final outage: basementbugger's allocator rejected shitbox's store s5 as `suspect`.
It does not establish why s5 was suspect, or explain the entire
several-hour interval reported by the operator. The investigation remains open.
The [original record](bootstrap-troubleshooting.md#queenbee-join-blocked-by-lost-quorum-2026-11-13)
contains the observations; this document separates conclusions from missing
evidence and describes collection for a recurrence.

## Timeline and identity

All timestamps below are UTC. A startup journal timestamp records that process
start, not necessarily the store's first creation or successful replica receipt.

| Time | Observation | What it establishes |
| --- | --- | --- |
| November 12, 20:28:52 | Basementbugger startup reports n4/s4. | Host-to-database identity. |
| November 13, 00:24:22 | Shitbox startup reports n5/s5 in the same database cluster. | Enrollment into that database identity, not eligibility for every range. |
| Several hours, operator report | Basementbugger, beeroach and shitbox Kubernetes Ready; cloudmotel-0/1 NotReady. | Kubernetes observations; database placement and liveness for this interval were not captured. |
| 06:34:28–06:34:54 | Basementbugger repeatedly logs `s5: suspect` as a throttled destination. | Concrete allocator rejection before beeroach disappears. |
| 06:44:59 | Replication queue reports 27 failing replicas and no destination among three live stores. | Replacement had not converged; this message alone does not identify all rejected stores. |
| 06:53:34 | r4's later error reports this closed timestamp. | A timestamp reported by the replica, not a precise failure instant. |
| 06:53:50.1 | Tailscale last-seen time for beeroach. | Tailnet observation, not a database shutdown timestamp. |
| 07:02:27 | Queenbee status reports r4 voters n1/s1, n3/s3, n4/s4, with n1/n3 down. | Only n4 remained available for this three-voter range; n5 was not a voter. |

Later failed-RPC logs identify n2 as cloudmotel-0. The captured record does not
map n1/n3 to hosts or map beeroach to a database ID. Do not infer those mappings
from VM creation order or the remaining names. Cluster UUID, database node ID,
store ID, advertised address and provider instance ID need to be recorded
together; Kubernetes names and Tailscale IDs are different identities.

## What the version-pinned source explains

The reviewed source is CockroachDB **v26.2.7**, the incident version. These are
defaults in that source; the incident's effective settings were not read.

| Setting | Source default | Role |
| --- | --- | --- |
| `server.time_until_store_dead` | 5 minutes | Dead-store classification after stale store gossip. |
| `server.time_after_store_suspect` | 30 seconds | Eligibility cooldown after observed node unavailability. |
| `server.time_after_store_suspect_in_store_liveness` | 30 seconds | Eligibility cooldown after StoreLiveness support withdrawal. |

The names and defaults are defined in
[v26.2.7 liveness.go](https://github.com/cockroachdb/cockroach/blob/v26.2.7/pkg/kv/kvserver/liveness/liveness.go).
The two suspect settings are distinct from the five-minute dead-store setting.

In [v26.2.7 store_pool.go](https://github.com/cockroachdb/cockroach/blob/v26.2.7/pkg/kv/kvserver/allocator/storepool/store_pool.go),
`StoreDetailMu.status` tests whether the current time is still inside either
suspect interval. Observing an unavailable/dead/draining node updates the
observer's `LastUnavailable`; recent StoreLiveness withdrawal supplies the other
timestamp. A new failure can postpone eligibility again. StoreLiveness withdrawal
can also occur for legitimate reasons, so its presence alone does not prove a
network or hardware fault. The allocator's assessment is local to the observer.

`getStoreListFromIDs` increments its live-store count for a suspect store while
excluding it under the allocation filters used to reject suspect destinations.
Consequently, “three live stores” does not mean three eligible stores. The log's
`suspect` reason also differs from snapshot-rejection throttling; the shared
“throttled stores” wording does not establish a failed snapshot.

A three-voter range needs two available voters. Replacing a dead replica needs a
healthy range quorum and an eligible destination. The five-minute timeout does
not remove votes by itself. Once r4 had only n4 available, adding fresh nodes
could not let that minority commit a new voting membership. See
[CockroachDB node shutdown](https://docs.cockroachlabs.com/docs/v26.2/node-shutdown).

This supports the sequence “replacement blocked, then another voter unavailable,
then lost quorum.” It does not prove s5 stayed suspect continuously, that either
suspect setting was at its default, or that nested virtualization caused it.
The CPU and scheduling behavior observed after quorum loss cannot establish
the earlier cause.

## Read-only collection for a recurrence

Capture this while the old voting majority is still available, before routine
stop/remove. Use private output directories (`umask 077`) and preserve complete
logs alongside filtered excerpts. Record observation start/end times and the
gateway host for every query; observations on different nodes need not agree.
Do not paste credentials, enrollment bundles or kubeconfigs into an incident.

On an authorized operator workstation with the **matching CockroachDB binary**,
set the target host and an existing operator certificate directory, then collect
the node view. Credentials remain on that workstation; do not copy admin keys to
guests. The command is read-only and bounded:

```sh
cockroach node status --host="$DB_HOST:26257" --certs-dir="$DB_CERTS" \
  --all --format=csv --timeout=15s > node-status.csv
```

Run the statements in [quorum-observations.sql](quorum-observations.sql)
individually in an authenticated `cockroach sql` session. Each statement has a
15-second session timeout. Save failures as evidence and continue with the next
statement; lost quorum can make SQL and catalog authentication unavailable.
These queries are an operator collection recipe, not a new CLI implementation
or a live-verified recovery procedure. `crdb_internal` tables are version-specific
and may require administrator privileges; their
[documented scope](https://docs.cockroachlabs.com/docs/v26.2/crdb-internal)
includes gossip, node/store status and range descriptors. `SHOW ZONE
CONFIGURATIONS` records explicit overrides; examine inherited configuration as
well before concluding that a target meets placement requirements.

For the incident ranges, use range descriptors across the whole keyspace, not
only `SHOW RANGES FROM DATABASE system`: metadata and node-liveness ranges are
not ordinary tables in that database. Save all descriptors in a small cluster;
on a larger cluster narrow by the failing range IDs. Keep voter arrays distinct
from all replicas and record transitional/learner replicas too. Compare
per-range voters against live, eligible stores and independent failure domains.

On each reachable database host, capture the same complete interval, including
before the first suspected failure. For this incident the interval would have
included both cloudmotel removals, the shitbox join and the final beeroach loss:

```sh
sudo journalctl --utc -u cockroachdb.service --since "$START_UTC" \
  --until "$END_UTC" --no-pager -o short-iso-precise > cockroachdb.log
rg -n -i 'suspect|heartbeat|liveness|support|snapshot|replica|allocator|clock|stall|quorum' \
  cockroachdb.log > placement-excerpts.log
sudo journalctl --utc -k --since "$START_UTC" --until "$END_UTC" \
  --no-pager -o short-iso-precise > kernel.log
```

Keep raw journals private; review and redact before sharing. Collect host clock
synchronization state, disk/free-space and I/O-latency observations, memory
pressure and CPU/scheduling stalls over the same interval. A single resource
snapshot after the outage cannot distinguish sustained contention from retries
caused by the outage. Capture provider lifecycle records with timestamps to
correlate removal, suspend and host failure; do not change cluster settings or
enable invasive workload benchmarks as part of evidence capture.

## Evidence still needed to close this incident

- Map n1/n3 and beeroach using startup journals, retained node-status output or
  other surviving identity records; obtain actual cloudmotel removal times.
- Recover effective settings and placement overrides at the time, including
  capacity and draining/decommissioning state of prospective destinations.
- Determine which suspect predicate was triggered on s5, how often it renewed,
  and which observer reported it. Correlate liveness/support changes with host,
  disk, clock and tailnet observations before the final outage.
- Follow r4's replication queue over the reported Ready interval and distinguish
  failed eligibility from failed snapshot transfer and other constraint errors.

If the old journals/settings are no longer available, record those gaps rather
than treating a fresh cluster's healthy placement as proof of the old cause.
No remote collection, database changes or infrastructure operations were run
during this documentation/source review. Local validation was `git diff --check`;
the collection commands and SQL have not been exercised against a live database.
