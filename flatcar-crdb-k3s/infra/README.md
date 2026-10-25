# Pulumi deployment targets

The portable layer is `cluster.json` plus `generate.py`: Flatcar Ignition,
per-node certificates, host-level CockroachDB, and K3s server/worker roles.
Cloud definitions supply networking, disks, instance identity, and delivery of
each node's Ignition. CockroachDB is enabled on both roles by default.

The first implementation is [Azure Flexible VMSS](azure/README.md), written in
Go with Pulumi's Azure Native provider. Versions and checksums are pinned in
`go.mod` and `go.sum`. The program reads the existing generator's outputs;
it does not reimplement certificate generation or host-service configuration.

The [local QEMU/KVM target](qemu/README.md) uses Pulumi Go with the current
libvirt provider, a NAT network, fixed DHCP reservations, and persistent qcow2
disks. It consumes the same generator outputs and keeps its resources separate
from the older direct-QEMU lab. Both targets are locally tested with mocks;
neither has been boot-tested through Pulumi yet.

For Google Cloud, the equivalent target is **Compute Engine VMs**, and for AWS
it is **EC2**. These would get separate cloud definitions around the same node
provisioning layer. They are not implemented yet. Managed GKE, AKS, and EKS own
their Kubernetes control planes and are a different architecture.

Pulumi provides a common provisioning workflow, not identical cloud resources.
An Azure VMSS, Google instance group, and AWS Auto Scaling Group have different
APIs and capabilities. The current per-node credentials and fixed addresses
require explicit node management; turning on unattended autoscaling would need
a bootstrap/identity service and a separate stateful-node lifecycle design.

Why Pulumi Go: typed Azure resource definitions, ordinary Go validation/tests,
and reusable components when adding other clouds. State can live in Pulumi
Cloud or a DIY backend such as Azure Blob, S3, Google Cloud Storage, or a local
directory. There is no required Pulumi Cloud account for this deployment.
See [Go support](https://www.pulumi.com/docs/iac/languages-sdks/go/) and
[state backends](https://www.pulumi.com/docs/concepts/state/).

OpenTofu's advantages are a smaller declarative language, fewer programming
language dependencies, and the Terraform-compatible module ecosystem. Pulumi
adds Go SDK dependencies and asynchronous inputs/outputs to learn, in exchange
for ordinary language tooling and greater flexibility.

Alternatives:

| Approach | Fit for this project |
| --- | --- |
| Terraform / OpenTofu | HCL plan/apply workflow with cloud-specific resources. No management Kubernetes cluster required. |
| Pulumi | The same infrastructure lifecycle expressed in Go, Python, TypeScript, or other supported languages. |
| Crossplane | Kubernetes APIs and controllers reconcile cloud resources; needs a management cluster. |
| Cluster API | Kubernetes cluster/machine lifecycle; our K3s + host CockroachDB bootstrap needs suitable provider integration. |
| Bicep / ARM | Azure-specific definitions; other clouds need separate tools or languages. |

Terraform's current CLI license is BUSL-1.1, with a production-use grant that
permits internal use and restricts competing hosted/embedded offerings. Each
release changes to MPL-2.0 after four years. OpenTofu is MPL-2.0; Pulumi's core
and Crossplane are Apache-2.0. Provider and hosted-service terms are separate.
See the [Terraform license](https://github.com/hashicorp/terraform/blob/main/LICENSE),
[OpenTofu license](https://github.com/opentofu/opentofu/blob/main/LICENSE),
[Pulumi source](https://github.com/pulumi/pulumi),
[Crossplane documentation](https://docs.crossplane.io/latest/managed-resources/managed-resources/),
and [Cluster API providers](https://cluster-api.sigs.k8s.io/reference/providers).
