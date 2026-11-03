data "k0rdent_cluster_type" "gpu" {
  id = "gpu-training"
}

resource "k0rdent_instance_group" "hosts" {
  id           = "training-01-hosts"
  cluster_type = data.k0rdent_cluster_type.gpu.self_link
  display_name = "Training hosts"

  node_pool = {
    gpu-workers = {
      size = 4
    }
  }

  # Set when hosts are built. Changing it replaces the group, which
  # needs `allow_replace = true`.
  ssh_access = [
    {
      username = "ops"
      sudo     = "ALL=(ALL) NOPASSWD:ALL"
    },
  ]
  # Write-only, so never stored, and an import plans clean. Needs Terraform
  # 1.11 or later.
  ssh_public_keys_wo = {
    ops = [trimspace(file("~/.ssh/id_ed25519.pub"))]
  }
}
