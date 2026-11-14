# Compute provider contract

`internal/vm.ComputeProvider` is the boundary used by shared creation and CLI
lifecycle dispatch. QEMU and Azure implement it. Resolving a provider does not
inspect infrastructure, contact a cloud API, or launch compute.

Shared creation validates cluster/node configuration, reserves private state,
creates or preserves credentials, and renders Ignition before calling the
provider's `Create` method. The provider receives a `PreparedGroup`: owned group
identity, node names, references to private Ignition files, an image selection,
resource sizing, placement and the explicit egress mode. It receives no cluster
inventory beyond this owned group and no separate signing/admin credentials.
Node enrollment, membership discovery and bootstrap remain guest responsibilities.

Both adapters verify that the prepared request matches the saved inventory and
provider ownership. They check the complete batch's private file paths and
permissions before creating compute. Providers must retain partial state on
failure and must not replace existing resources as a retry. A private Ignition
profile can itself contain enrollment secrets; neither its contents nor encoded
customData may be written to ordinary output or error messages.

The current capabilities are explicit:

| Behavior | QEMU | Azure Uniform VMSS |
| --- | --- | --- |
| Ignition delivery | Private file per VM via QEMU fw_cfg | Private SDK customData using the first node's shared profile; IMDS derives instance identity |
| Image | Prepared local Flatcar qcow2 | Pinned Flatcar marketplace image |
| Persistent root disk | 20 GiB overlay | 32 GiB managed OS disk |
| Temporary stop | Contract refuses pending quorum/drain implementation; underlying power operation retains disks | Contract refuses pending quorum/drain implementation; underlying deallocation retains disks |
| Compute removal | Retains overlay unless deletion explicitly requested | Deletes owned resource group, including OS disks |
| Egress | QEMU user networking (SLIRP NAT) | Azure default outbound access, an unresolved prototype limitation |

`Capabilities` describes the implementation; it does not establish that internet
or tailnet connectivity was verified. Neither adapter requires public ingress,
cloud object storage, a private DNS service, or a guest command extension. Azure's
future explicit egress implementation must update the declared mode and surface
any NAT/load-balancer dependency rather than silently adding it.

`Start`, `Stop`, `Status` and `Remove` operate on saved owned state. `Start` never
creates replacement resources or credentials. The QEMU adapter retains its
process/QMP identity checks; the Azure adapter retains saved subscription/group
identity and ownership-tag checks. Database decommissioning gates belong to the
lifecycle workflow, and the removal adapter delegates to that guarded workflow;
the contract is not an alternative deletion bypass. Any explicit abandoned-cluster
teardown still has to verify resource ownership and obey the selected disk policy.
Temporary stop currently refuses to power down compute because whole-group
quorum preflight and temporary database draining are not yet implemented. A
permanent decommission is not a substitute for a reversible stop.

The contract currently lives alongside the existing implementations in
`internal/vm`; Azure now uses typed Go SDK clients while QEMU still uses the handwritten QMP
transport. Moving QEMU to go-qemu is separate work. Their client
implementations may change behind this boundary without changing shared guest
preparation, resource identity, or disk retention semantics. Adding another
provider requires an adapter and its own image/profile/egress capabilities, plus
provider-specific sizing and identity preparation; it does not require a different
cluster enrollment protocol.
