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

## See also (today)

- The Packer template: [`packer/eks-gvisor-al2023.pkr.hcl`](../packer/eks-gvisor-al2023.pkr.hcl)
- The RuntimeClass: [`deploy/runtimeclass.yaml`](../deploy/runtimeclass.yaml)
- plan.md section 9 (gitignored locally) for the full design

For Phase 0 + Phase 1 (today), this doc is intentionally empty.
