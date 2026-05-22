// agentmoat: Packer template that builds a gVisor-ready EKS-optimized AMI
// on Amazon Linux 2023.
//
// Recipe sources (see plan.md, section 9.2 for full citations):
//   - https://www.blinkops.com/blog/run-containers-securely-with-gvisor-on-eks
//   - https://www.verygoodsecurity.com/blog/posts/secure-compute-part-2
//   - https://gvisor.dev/docs/user_guide/install/
//   - https://gvisor.dev/docs/user_guide/containerd/quick_start/
//   - https://github.com/awslabs/amazon-eks-ami
//   - https://gardener.cloud/docs/gardener/advanced/custom-containerd-config/
//   - https://developer.hashicorp.com/packer/integrations/hashicorp/amazon
//
// Build:
//   packer init   .
//   packer validate .
//   packer build  -var 'k8s_version=1.31' -var 'gvisor_version=release-20251119.0' .
//
// The resulting AMI:
//   - Has /usr/local/bin/runsc and containerd-shim-runsc-v1 with verified SHA512.
//   - Has /etc/containerd/config.d/gvisor.toml (a drop-in, not an edit to the
//     main config.toml) registering a "gvisor" runtime handler.
//   - Has /etc/containerd/runsc.toml pinning the platform to "systrap"
//     (EKS does not allow nested KVM).
//   - Leaves /var/lib/containerd intact so the pause image cache survives.

packer {
  required_plugins {
    amazon = {
      source  = "github.com/hashicorp/amazon"
      version = "~> 1.3"
    }
  }
}

variable "k8s_version" {
  type        = string
  default     = "1.31"
  description = "Kubernetes minor version line of the parent EKS-optimized AMI to build on."
}

variable "gvisor_version" {
  type        = string
  default     = "release-20251119.0"
  description = "gVisor release tag (https://gvisor.dev/docs/user_guide/install/). Pin a tested release; do not use 'latest'."
}

variable "region" {
  type    = string
  default = "us-east-1"
}

variable "instance_type" {
  type    = string
  default = "t3.large"
}

variable "ssh_username" {
  type        = string
  default     = "ec2-user"
  description = "AL2023 uses ec2-user."
}

variable "ami_owner_account" {
  type        = string
  default     = "602401143452"
  description = "AWS-owned account that publishes the EKS-optimized AMIs."
}

variable "extra_tags" {
  type    = map(string)
  default = {}
}

locals {
  ami_name = "agentmoat-gvisor-al2023-k8s${var.k8s_version}-${var.gvisor_version}-${formatdate("YYYYMMDD-hhmm", timestamp())}"

  base_tags = {
    Name           = local.ami_name
    Project        = "agentmoat"
    Runtime        = "gvisor"
    GVisorVersion  = var.gvisor_version
    BaseAMI        = "amazon-eks-node-al2023"
    K8sVersion     = var.k8s_version
    PackerTemplate = "eks-gvisor-al2023.pkr.hcl"
  }

  tags = merge(local.base_tags, var.extra_tags)
}

source "amazon-ebs" "eks_gvisor_al2023" {
  ami_name      = local.ami_name
  ami_description = "EKS-optimized AL2023 + gVisor (${var.gvisor_version}), built by agentmoat."
  instance_type = var.instance_type
  region        = var.region
  ssh_username  = var.ssh_username

  // Start from the most recent EKS-optimized AL2023 AMI for the chosen K8s line.
  source_ami_filter {
    filters = {
      name                = "amazon-eks-node-al2023-x86_64-standard-${var.k8s_version}-v*"
      root-device-type    = "ebs"
      virtualization-type = "hvm"
    }
    owners      = [var.ami_owner_account]
    most_recent = true
  }

  // Slightly larger than the default root volume so we can stage downloads.
  launch_block_device_mappings {
    device_name           = "/dev/xvda"
    volume_size           = 30
    volume_type           = "gp3"
    delete_on_termination = true
  }

  tags     = local.tags
  run_tags = local.tags

  // Make sure the snapshot itself is tagged the same way (useful for cost/inventory).
  snapshot_tags = local.tags
}

build {
  name    = "agentmoat-gvisor-al2023"
  sources = ["source.amazon-ebs.eks_gvisor_al2023"]

  // 1. Install runsc + the containerd shim, with SHA512 verification.
  provisioner "shell" {
    inline_shebang = "/bin/bash -eo pipefail"
    inline = [
      "set -euxo pipefail",
      "ARCH=$(uname -m)",
      "BASE=https://storage.googleapis.com/gvisor/releases/release/${var.gvisor_version}/$${ARCH}",
      "TMPDIR=$(mktemp -d)",
      "cd \"$TMPDIR\"",
      "curl --fail --silent --show-error --location --remote-name \"$BASE/runsc\"",
      "curl --fail --silent --show-error --location --remote-name \"$BASE/runsc.sha512\"",
      "curl --fail --silent --show-error --location --remote-name \"$BASE/containerd-shim-runsc-v1\"",
      "curl --fail --silent --show-error --location --remote-name \"$BASE/containerd-shim-runsc-v1.sha512\"",
      "sha512sum -c runsc.sha512",
      "sha512sum -c containerd-shim-runsc-v1.sha512",
      "sudo install --owner=root --group=root --mode=0755 runsc /usr/local/bin/runsc",
      "sudo install --owner=root --group=root --mode=0755 containerd-shim-runsc-v1 /usr/local/bin/containerd-shim-runsc-v1",
      "rm -rf \"$TMPDIR\"",
      "/usr/local/bin/runsc --version",
    ]
  }

  // 2a. Stage the containerd drop-in and the runsc config in /tmp.
  provisioner "file" {
    source      = "files/gvisor-runtime.toml"
    destination = "/tmp/gvisor-runtime.toml"
  }

  provisioner "file" {
    source      = "files/runsc.toml"
    destination = "/tmp/runsc.toml"
  }

  // 2b. Move them into place. We use a drop-in dir (config.d) and a single
  // 'imports' line in the main config.toml. This survives AL2023 nodeadm
  // re-templating; editing the main config.toml directly does not.
  provisioner "shell" {
    inline_shebang = "/bin/bash -eo pipefail"
    inline = [
      "set -euxo pipefail",
      "sudo mkdir -p /etc/containerd/config.d",
      "sudo mv /tmp/gvisor-runtime.toml /etc/containerd/config.d/gvisor.toml",
      "sudo mv /tmp/runsc.toml /etc/containerd/runsc.toml",
      "sudo chown root:root /etc/containerd/config.d/gvisor.toml /etc/containerd/runsc.toml",
      "sudo chmod 0644 /etc/containerd/config.d/gvisor.toml /etc/containerd/runsc.toml",

      // Idempotent insert of the imports line. We grep with a fixed string so
      // a re-run does not append again.
      "if ! sudo grep -Fq 'imports = [\"/etc/containerd/config.d/*.toml\"]' /etc/containerd/config.toml; then",
      "  echo 'imports = [\"/etc/containerd/config.d/*.toml\"]' | sudo tee -a /etc/containerd/config.toml > /dev/null",
      "fi",

      // Validate that containerd can parse the resulting merged config.
      // We deliberately do NOT systemctl restart containerd inside the AMI
      // build; first boot will pick it up cleanly.
      "sudo containerd config dump > /dev/null",
    ]
  }

  // 3. Bootstrap-state cleanup.
  // Important: we intentionally do NOT delete /var/lib/containerd. AL2023
  // ships with the pause image pre-cached there, and removing it makes the
  // first node boot hang while the kubelet waits for the pause image.
  provisioner "shell" {
    inline_shebang = "/bin/bash -eo pipefail"
    inline = [
      "set -euxo pipefail",
      "sudo rm -rf /var/lib/cloud/instances/*",
      "sudo rm -f /var/log/cloud-init*.log",
      "sudo cloud-init clean --logs --seed || true",
    ]
  }

  // 4. Emit a manifest with the source AMI, gVisor version, build timestamp,
  // and the resulting AMI ID. Useful for provenance and for agentmoat's own
  // AMI selection logic.
  post-processor "manifest" {
    output     = "build-manifest.json"
    strip_path = true
    custom_data = {
      gvisor_version = var.gvisor_version
      k8s_version    = var.k8s_version
      base_ami       = "amazon-eks-node-al2023"
    }
  }
}
