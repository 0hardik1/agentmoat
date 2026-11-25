# gVisor and EKS Nitro Sandboxes

Both put an untrusted pod behind a boundary that a plain `runc` container
does not have. They draw that boundary in different places, and neither one
hides a pod's AWS credentials from the node it runs on. This page compares
them on the questions that decide a migration: what each isolates, who can
reach a pod's credentials, and when to prefer which.

Amazon EKS Nitro Sandboxes launched on November 9, 2026 (AWS Containers
blog). Facts about them below come from the EKS user guide and the
`awslabs/amazon-eks-ami` repository as of late November 2026; re-check them
when AWS updates the feature.

## What each isolates

| | gVisor (`runsc`) | EKS Nitro Sandbox, isolated mode | EKS Nitro Sandbox, confidential mode |
| --- | --- | --- | --- |
| How a pod runs | A user-space kernel (the Sentry) answers the pod's system calls; a Gofer serves files; seccomp confines both | Its own Firecracker microVM with its own guest kernel, carved out by the Nitro Hypervisor | Same microVM, with a Cedar policy that pins what the node may do to it |
| Selected by | `runtimeClassName: gvisor` (agentmoat's RuntimeClass) | `runtimeClassName: isolated-sandbox` | `runtimeClassName: confidential-sandbox` |
| Protects the host from the pod | Yes: no system call reaches the host kernel directly | Yes: hardware virtualization boundary | Yes |
| Protects the pod from the node | No. The host is trusted; the pod's memory is an ordinary host memfd | No. AWS: "The parent instance is trusted" | Partly. AWS: "The parent instance is untrusted"; the node cannot read guest memory or CPU state |
| Node needs | A Linux node with `runsc` installed (agentmoat's AL2023 AMI, or kind). Not EKS Auto Mode or Bottlerocket | Kubernetes 1.36+, a supported instance type with Nitro Enclaves enabled, the EKS pod-isolation AMI or EKS Auto Mode | Same as isolated |
| Devices and GPUs | NVIDIA GPUs through `nvproxy` on supported cards ([gpu-nvproxy.md](gpu-nvproxy.md)) | Pods that need devices must run without a sandbox RuntimeClass | Same as isolated |
| Compatibility cost | Some syscalls and kernel features are missing ([compatibility-checklist.md](compatibility-checklist.md)) | A real Linux guest kernel, so fewer syscall gaps; other limits apply (for example, no user namespaces on isolated-sandbox nodes) | Same, plus a Cedar policy to generate and maintain |

## Who can reach a pod's credentials

The short answer for all three: anyone with root on the node.

- **EKS Pod Identity on any node.** An agent on each node exchanges the
  pod's projected service account token for temporary credentials through
  `eks-auth:AssumeRoleForPodIdentity`, using the node's IAM role. The API
  takes only the cluster name and the token.
- **gVisor.** The kubelet still writes projected tokens to a tmpfs volume
  under `/var/lib/kubelet/pods/<uid>/volumes/kubernetes.io~projected/` on
  the host, and `runsc` bind-mounts them in. gVisor changes how the pod
  reaches the host kernel, not how the kubelet stores the pod's secrets. So
  node root can read the token and exchange it, as on a `runc` node.
- **Nitro Sandboxes, both modes.** The EKS security model says the parent
  instance "can mint Pod Identity credentials for any pod scheduled on the
  node" and tells you to "treat the pod identity role's permissions as
  available to the parent instance". Root on the parent can also connect to
  ports inside the guest over the Firecracker vsock socket. Confidential
  mode keeps guest memory away from the node, but it does not keep the IAM
  role away from it.

The EKS Best Practices Guide states the general rule: an attacker who gains
access to a host "can retrieve all Secrets, ConfigMaps, and Volumes, mounted
on that host".

### The weakest pod on the node sets the bar

The cheapest way to become node root is usually a container escape from a
plain `runc` pod, the class of bug a sandbox exists to remove. Neither
sandbox keeps `runc` pods off its nodes by default:

- agentmoat's RuntimeClass selects `runtime: gvisor` nodes; a `runc` pod
  lands there too unless the nodes are tainted (the shipped RuntimeClass
  tolerates `runtime=gvisor:NoSchedule`).
- The EKS pod-isolation AMI keeps `runc` as the default runtime ("Every
  other pod keeps running under runc") and only labels its nodes
  `eks.amazonaws.com/nitro-sandbox=true`. It adds no taint.

So one `runc` escape on a shared node yields the IAM roles of every
sandboxed pod there. `agentmoat preflight` reports this as
`sandbox-node-runc-colocation`, and lists sandboxed pods that carry AWS
credentials as `sandbox-pod-aws-credentials` (see [preflight.md](preflight.md)).
agentmoat does not read IAM, so it cannot tell how broad each role is.

What helps, in both cases:

1. Dedicate the sandbox nodes: taint them and tolerate the taint only from
   sandboxed pods, so that only sandboxed pods and the DaemonSets the node
   needs run there.
2. Give each sandboxed workload its own least-privilege role.
3. Restrict who can create pods that name a sandbox RuntimeClass. AWS calls
   pod spec submission for Nitro Sandboxes "a privileged operation", and
   the same holds for gVisor: the pod spec picks the service account, and
   so the role.

## When to prefer which

- **gVisor** fits when you run your own node AMIs, need NVIDIA GPUs in the
  sandbox, run Kubernetes older than 1.36, or want the same runtime on kind,
  on other clouds, and on EKS. You pay in syscall compatibility, which
  `agentmoat scan` measures workload by workload.
- **Nitro isolated mode** fits when you want a hardware boundary and a real
  guest kernel, are on Kubernetes 1.36 or later with supported instance
  types, or use EKS Auto Mode, where `runsc` cannot be installed. It does not
  run device or GPU pods.
- **Nitro confidential mode** fits when the data in the pod must stay secret
  from node operators or a compromised node, backed by attestation and KMS
  key policies. It still does not protect the pod's IAM role from the node.

None of the three replaces least privilege on the pod's role or a node pool
that runs only sandboxed pods.

## Sources

- EKS user guide, Nitro Sandboxes overview: https://docs.aws.amazon.com/eks/latest/userguide/sandboxing.md
- EKS user guide, Nitro Sandbox security model: https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-security-model.md
- EKS user guide, Nitro Sandbox limitations: https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-limitations.md
- EKS user guide, metadata and credentials in Nitro Sandboxes: https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-metadata-credentials.md
- EKS user guide, confidential sandbox setup: https://docs.aws.amazon.com/eks/latest/userguide/sandboxing-confidential-setup.md
- AWS Containers blog, Nov 9, 2026: https://aws.amazon.com/blogs/containers/aws-nitro-sandboxes-hardware-enforced-workload-isolation-on-amazon-eks/
- AWS Compute blog, Nov 10, 2026: https://aws.amazon.com/blogs/compute/nitro-sandboxes-hardware-isolation-for-every-eks-pod-ecs-task-and-ec2-workload/
- `awslabs/amazon-eks-ami`, pod isolation images (`doc/usage/al2023.md`) and `templates/al2023/runtime/pod-isolation/10-pod-isolation.yaml`, added in PR #2884: https://github.com/awslabs/amazon-eks-ami
- EKS user guide, how Pod Identity works: https://docs.aws.amazon.com/eks/latest/userguide/pod-id-how-it-works.html
- EKS user guide, Pod Identity agent setup: https://docs.aws.amazon.com/eks/latest/userguide/pod-id-agent-setup.html
- EKS API reference, AssumeRoleForPodIdentity: https://docs.aws.amazon.com/eks/latest/APIReference/API_auth_AssumeRoleForPodIdentity.html
- EKS Best Practices Guide, tenant isolation: https://docs.aws.amazon.com/eks/latest/best-practices/tenant-isolation.html
- gVisor security model: https://gvisor.dev/docs/architecture_guide/security/
- gVisor resource model: https://gvisor.dev/docs/architecture_guide/resources/
- gVisor containerd configuration (volume handling): https://gvisor.dev/docs/user_guide/containerd/configuration/
- Kubernetes, projected volume plugin source: https://github.com/kubernetes/kubernetes/blob/master/pkg/volume/projected/projected.go
