# Tailnet setup

Configure this once before distributing enrollment keys. The chosen boundary is
that tailnet members and tagged devices can administer the cluster. One cluster
uses the tailnet, `tag:roach`, pod range `10.42.0.0/16` and Service `svc:k8s-api`.

## One-time setup

1. Enable [MagicDNS](https://tailscale.com/docs/features/magicdns) and note the
   tailnet suffix (for example `quoll-duck.ts.net`) for `--tailnet-domain`.
2. In the Tailscale Services console, define **k8s-api** with endpoint **tcp:443**.
   Service tags are optional because the example grants access by Service name.
   Infestation configures server advertisements; the enrollment key does not
   create the Service. See [Tailscale Services](https://tailscale.com/docs/features/tailscale-services).
3. Merge [the policy example](../examples/tailscale-policy.json) into Access
   controls and save it in Tailscale's editor. Merge entries into existing maps
   and append rules to existing arrays; preserve unrelated tags such as
   `tag:zebu` and existing ACL/grant/SSH rules. The file is a merge example, not
   a replacement for the rest of a tailnet's policy.
4. Create a reusable, preauthorized auth key carrying **tag:roach**, then give
   friends the key, tailnet suffix and repository/binary. Preauthorization avoids
   per-device approval when device approval is enabled. It does not substitute
   for the route/Service auto-approvers below. Treat the key as a secret.

The example is strict JSON, which the tailnet policy editor accepts. Comments
and trailing commas in an existing HuJSON policy are fine. The editor validates
policy semantics; local JSON parsing alone does not prove the policy is accepted
or that a connection works.

## What each section does

### Tag ownership

```json
"tagOwners": {"tag:roach": []}
```

An empty owner list is valid: Owners, Admins and Network admins implicitly own
tags and can assign this tag or issue a tagged auth key. Friends using that
already-issued key do not need permission to mint another key or assign arbitrary
tags. Add an explicit user/group only if they should have that authority.
The node helper expects `tag:roach` during peer discovery/enrollment.
[Tailscale tag ownership](https://tailscale.com/docs/features/tags#define-tag-owners).

### Automatic route and Service-host approvals

```json
"autoApprovers": {
  "routes": {"10.42.0.0/16": ["tag:roach"]},
  "services": {"svc:k8s-api": ["tag:roach"]}
}
```

The route rule covers per-node pod subnets inside the `/16`; K3s assigns those
subnets. The Service rule lets tagged API hosts join the existing API Service
without individual host approval. Agents do not advertise that Service.
Approval and traffic access are separate: these rules alone do not allow a
client to connect. See [route auto-approvers](https://tailscale.com/docs/reference/syntax/policy-file#auto-approvers)
and [Service auto-approval](https://tailscale.com/docs/features/tailscale-services#set-up-automatic-approval).

### Traffic grants

The example allows members, tagged machines and routed pod sources to reach the
cluster nodes and pod CIDR. This intentionally covers all protocols/ports within
that cluster scope, matching the administrator boundary. A second grant permits
TCP 443 to `svc:k8s-api`. It does not grant access to unrelated destinations.
If existing ACLs already allow those flows, additional equivalent grants may be
redundant. Grants and existing ACLs can coexist.
[Tailscale grants](https://tailscale.com/docs/reference/syntax/grants).

Tagged machines need their own source selector: human-member access alone does
not cover tagged cluster identities. Pod CIDRs also appear as sources because
pod traffic retains pod source IPs. Including them allows pod-to-pod and
pod-to-node traffic as well as ordinary host traffic.

| Destination | Purpose |
| --- | --- |
| Node TCP 22 | Network access needed alongside the SSH authorization rule |
| Node TCP 7444 | Helper enrollment, peer discovery and kubeconfig retrieval |
| Node TCP 6443 | Direct K3s API and agent/server joins |
| Node TCP 10250 | Kubelet access, including logs/exec paths |
| Node TCP 26257 | CockroachDB peer and SQL traffic |
| Pod CIDR, all protocols | Cross-node pod traffic and direct pod access |
| `svc:k8s-api`, TCP 443 | Shared private Kubernetes API endpoint |

The example is deliberately broader than a minimum-port list. It does not grant
SSH login merely by allowing TCP 22, and it does not make kube-proxy Service IPs
routable from ordinary tailnet clients.

### Tailscale SSH

The example preserves the member SSH rule used in the mixed test:

```json
{
  "action": "accept",
  "src": ["autogroup:member"],
  "dst": ["autogroup:self", "tag:roach"],
  "users": ["autogroup:nonroot", "root"]
}
```

`accept` does not require a periodic interactive SSH check. The `tag:roach`
destination covers tagged VMs, which are not owned by a human user for purposes
of `autogroup:self`. Routine login is `core` followed by sudo. Root login remains
permitted by this supplied policy. Both network access to TCP 22 and an SSH rule
are required. The helper enables Tailscale SSH on the node; standard sshd is masked.
[Tailscale SSH requirements](https://tailscale.com/docs/features/tailscale-ssh#ensure-tailscale-ssh-is-permitted-in-access-control-policies).

This SSH rule authorizes human-member sources, not arbitrary tagged machines.
Node enrollment and joining use the helper over HTTP inside WireGuard, so
node-to-node SSH is not required. Shared/external users and other tagged SSH
sources need explicit rules if they are intended to log in.

## Clients and verification

A Linux workstation that should reach pod IPs must accept advertised routes:

```sh
tailscale set --accept-routes
```

Use Tailscale DNS settings for node/Service names. Profiles configure DNS and
route acceptance on the VMs; host database containers use `100.100.100.100`.
CoreDNS continues to provide Kubernetes DNS. No public inbound firewall rule,
router forwarding, Funnel or public load balancer is needed for these private
flows.

After boot, check that machines have `tag:roach`, pod advertisements are approved,
and all intended API servers appear as approved Service hosts. Test SSH as
`core`, helper-based kubeconfig retrieval and authenticated Kubernetes readiness.
Use `infestation-smoke` for pod DNS/HTTP checks and optional host latency samples.
If old advertisements remain pending after a policy change, inspect them in the
console rather than assuming automatic approval has already happened.

The existing mixed test verified its SSH and approvals setup after repairs.
The independent enrollment flow and this consolidated grant example still need
fresh live verification. No tailnet policy is changed by editing these files.
