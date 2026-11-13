# Independent bootstrap troubleshooting, 2026-11-12

This records user-supplied live output from `basement-0`, `bee-0` and `cloudbug-0`.
It is evidence of manual recovery, not an unattended bootstrap test of the
subsequent source changes.

The initializer initially reported one reachable database listener. Both joiners
were connected to Tailscale, but their original CockroachDB/K3s start jobs had
failed on dependencies while enrollment waited for the founding host. Bee's
enrollment eventually succeeded after 108 attempts. Docker was active; the
dependent services remained inactive until manually started.

Bee's database then rejected an empty `--locality` argument. Its runtime
environment file contained only `INFESTATION_SEEDS`; generated units also expected
`INFESTATION_DATABASE_ADDRESS` and `INFESTATION_LOCALITY`. Appending those two
values allowed the existing database service to start. The helper/service source
revision mismatch is a hypothesis; guest revision comparison was not captured.
No disks, CA, tokens or database identities were replaced.

Once three database listeners were reachable, the initializer moved past database
setup to API readiness. Basement and cloudbug became Kubernetes Ready first.
Bee's original K3s process remained activating after logging a shutdown request;
restarting it produced the user's final report of three nodes. Database member
health and directed workload DNS/HTTP connectivity were not captured.

K3s also logged a Kine Maintenance/Status failure because CockroachDB rejected
`pg_total_relation_size()`. Metrics API discovery errors appeared during startup.
Neither issue was resolved or tested under sustained load in this session.

The local regression suite checks generated recovery dependencies for QEMU/Azure
server and agent profiles, bounded K3s timeouts, execution of helper/environment
guards, runtime environment regeneration, and read-only diagnostics with mocked
tailnet membership endpoints. Generated recovery graphs also passed the local
systemd unit verifier using inert dependencies under a temporary root. This does
not exercise a running systemd manager or
provision VMs. Fresh live verification still needs joiners-first startup, delayed
initializer availability, unavailable database recovery and reboot without
manual runtime file edits. These remain in [TODO](../TODO.md).

## Queenbee join blocked by lost quorum, 2026-11-13

User-supplied `vm status --name queenbee` output at 07:02:27 UTC showed
successful tailnet enrollment and credential exchange, but CockroachDB could not
allocate a node ID. Range r4, spanning `/System/{NodeLivenessMax-tsd}`, listed
voters n1/s1, n3/s3 and n4/s4, with n1 and n3 down. Its reported closed timestamp
was 06:53:34 UTC. K3s/API readiness failures were downstream of database
unavailability; a listening database TCP port did not establish range health.

Read-only Tailscale inspection showed basementbugger, shitbox and queenbee online;
beeroach was offline, last seen at 06:53:50.1 UTC. Startup journals identified
basementbugger as database node n4 (started November 12 at 20:28:52 UTC) and
shitbox as n5 (started November 13 at 00:24:22 UTC), both in the same database
cluster. Shitbox was not a voter for the failing range. Its earlier Kubernetes
Ready result did not verify CockroachDB replica placement or health.

The user reported that Azure nodes had been removed. Source inspection confirmed
that provider removal does not decommission CockroachDB members before deleting
compute. However, n1/n3 were not mapped to hosts, and the removal times, effective
dead-store timeout and earlier allocator/snapshot errors were not captured.
The unresolved question is why automatic replica replacement had not put a voter
on shitbox while beeroach was still available. Insufficient eligible capacity,
placement constraints, replication errors and the failure timeline remain
possibilities, not established causes.

The user subsequently clarified that basementbugger, beeroach and shitbox remained
Kubernetes Ready for several hours while both cloudmotel nodes were NotReady.
This makes a brief wait for the default five-minute dead-store timeout an
insufficient explanation. Actual database health and eligible placement during
that interval still need verification; Kubernetes readiness is not evidence that
the missing voting replicas had been replaced.

Follow-up read-only inspection found a concrete allocator blocker before beeroach
went offline: at 06:34:28–06:34:54 UTC, basementbugger's replication queue repeatedly
reported `1 matching stores are currently throttled: [s5: suspect]` across many
ranges, including system tables. Store s5 belongs to shitbox (node n5). At
06:44:59 UTC the queue reported 27 replicas failing with `0 of 3 live stores are
able to take a new replica for the range`. Thus shitbox was not being accepted as
a replacement destination despite Kubernetes Ready. Why it remained suspect and
whether this explains the entire earlier window remain unresolved. Later logs
also mapped database node n2 to cloudmotel-0 through failed RPC messages.

CockroachDB v26.2 defaults `server.time_until_store_dead` to five minutes, after
which dead-store replicas become candidates for replacement. This does not
automatically erase their votes or allow a minority to change range membership.
See [node shutdown and dead-node handling](https://docs.cockroachlabs.com/docs/v26.2/node-shutdown)
and [CockroachDB's self-repair design](https://github.com/cockroachdb/cockroach/blob/master/docs/design.md#self-repair).
No stores, database membership or infrastructure were modified during diagnosis.
