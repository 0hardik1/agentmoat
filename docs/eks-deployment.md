# EKS deployment

> Status: STUB. Filled in by Phase 5 (Packer + EKS deployment path).

## Table of contents (planned)

1. Prerequisites (AWS account, EKS cluster, IAM, region setup)
2. Building the Packer AMI (`packer build ./packer`)
3. Wiring the AMI to a self-managed node group (CloudFormation / Terraform)
4. Wiring the AMI to Karpenter via `EC2NodeClass`
5. Applying `deploy/runtimeclass.yaml`
6. Labelling the gVisor nodes (`kubectl label node ... runtime=gvisor`)
7. Tainting the gVisor nodes (`kubectl taint node ... runtime=gvisor:NoSchedule`)
8. Validating: `agentmoat verify` against a test workload

## EKS Auto Mode is not supported (today, and by design)

EKS Auto Mode nodes are AWS-managed Bottlerocket instances. AWS owns the AMI
and the container runtime; there is no SSH, no SSM, and no supported way to
install software on them. `runsc` can therefore never be present, and a
RuntimeClass pointing at handler `gvisor` has nothing to run on.

`agentmoat preflight` detects these nodes by the
`eks.amazonaws.com/compute-type=auto` label and reports
`eks-auto-mode-nodes` (an error when every candidate node is Auto Mode, a
warning when only some are). `agentmoat apply` runs the same check and
refuses to patch anything (exit `5`) until a real gVisor node pool exists.

The supported shape is a mixed cluster:

1. Keep Auto Mode for the `runc` workloads.
2. Add a self-managed node group or a Karpenter `EC2NodeClass` that uses the
   agentmoat AL2023 AMI (built from `packer/`), labeled `runtime=gvisor` and
   tainted `runtime=gvisor:NoSchedule`.
3. Apply `deploy/runtimeclass.yaml`; its `scheduling` block steers gVisor
   pods to that pool and tolerates the taint.
4. Re-run `agentmoat preflight` and expect exit `0`.

See [`preflight.md`](preflight.md) for the full finding list.

## See also (today)

- The Packer template: [`packer/eks-gvisor-al2023.pkr.hcl`](../packer/eks-gvisor-al2023.pkr.hcl)
- The RuntimeClass: [`deploy/runtimeclass.yaml`](../deploy/runtimeclass.yaml)
- The RBAC manifests: [`deploy/clusterrole-readonly.yaml`](../deploy/clusterrole-readonly.yaml) and [`deploy/clusterrole-apply.yaml`](../deploy/clusterrole-apply.yaml)

The full end-to-end recipe is not written yet (tracked for Phase 5); the links
above are the working pieces today.
