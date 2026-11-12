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
