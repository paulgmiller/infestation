# Standalone connectivity checks

`infestation-smoke` is separate from the provisioning CLI. It needs `kubectl`, a
reachable Kubernetes API and credentials permitted to list nodes and create/read
namespaces, pods, Services, Jobs and pod logs. No Infestation inventory, VM state,
Azure login, Tailscale API key or workstation Tailscale client is required beyond
whatever connectivity is needed to reach the Kubernetes API.

```sh
go install ./cmd/infestation-smoke
infestation-smoke --kubeconfig /private/path/kubeconfig --out-dir smoke-results
```

Without `--kubeconfig`, kubectl uses its normal `KUBECONFIG`/current-context rules:

```sh
export KUBECONFIG=/private/path/kubeconfig
infestation-smoke
```

The checker discovers all registered Kubernetes nodes, requires at least two,
and fails before deploying checks if any node is unready, being deleted or has
invalid/duplicate identity. This avoids reporting success for an incomplete
subset of the cluster. It supports arbitrary Kubernetes DNS node names rather
than a provisioning hostname convention.

It creates one BusyBox HTTP server and ClusterIP Service per node, then fresh
Jobs for every directed source/destination pair. Clients check Kubernetes DNS
and the destination Service response. A successful rerun requires new Jobs;
it cannot reuse earlier completed results. Readiness selects the current run.

The `infestation-smoke` namespace, web pods and Services remain for inspection.
Finished Jobs expire after one hour. The output directory holds the discovered
node list and workload manifests. PASS messages are printed with Job logs;
failures return a nonzero exit status. This is a cluster-mutating test, not a
read-only health query.

## Optional Tailscale latency

```sh
infestation-smoke --kubeconfig /private/path/kubeconfig \
  --out-dir smoke-results --tailnet-latency
```

This still uses only Kubernetes API access from the caller, but requires every
node's InternalIP to be a Tailscale IPv4 address, a host-installed Tailscale CLI
at `/usr/bin/tailscale`, and its socket at `/run/tailscale/tailscaled.sock`.
Credentials/admission policy must permit privileged Jobs with those host mounts.
The jobs ask each host daemon to ping every other discovered node's tailnet IP.
They record ten samples per directed pair, preserve direct/relay transitions and
save raw output plus timing statistics in the output directory. Missing or
unrecognized samples fail the measurement rather than producing a false pass.

Unit/mock tests verify discovery, coverage, input rejection and manifests.
Running the binary against a real cluster is a separate live test.
