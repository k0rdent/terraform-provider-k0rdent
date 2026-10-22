data "k0rdent_cluster_type" "gpu" {
  id = "gpu-training"
}

resource "k0rdent_kubernetes_cluster" "training" {
  id           = "training-01"
  cluster_type = data.k0rdent_cluster_type.gpu.self_link
  display_name = "Training cluster"

  node_pool = {
    gpu-workers = {
      node_count = 4
    }
  }

  # Merged with the provider's `default_tags` into `tags_all`. These win.
  tags = {
    team = "research"
  }

  # Set when nodes are built. Changing it replaces the cluster, which
  # needs `allow_replace = true`.
  ssh_access = [
    {
      username        = "ops"
      ssh_public_keys = [trimspace(file("~/.ssh/id_ed25519.pub"))]
      sudo            = "ALL=(ALL) NOPASSWD:ALL"
    },
  ]

  timeouts {
    create = "45m"
  }
}

# Pinned to a version. Change `version_id` to a later one to upgrade in place.
# The update timeout covers the whole upgrade.
data "k0rdent_cluster_type_version" "inference" {
  cluster_type = "gpu-inference"
  version_id   = "aaaaaaaa-0000-4000-8000-000000000004"
}

resource "k0rdent_kubernetes_cluster" "inference" {
  id           = "inference-01"
  cluster_type = data.k0rdent_cluster_type_version.inference.self_link

  timeouts {
    update = "2h"
  }
}

# Security groups on the cluster, applied in list order. Leave
# `security_groups` out to leave them alone, or set `[]` to remove them all.
resource "k0rdent_security_group" "api" {
  id = "api-from-office"

  ingress {
    action             = "permit"
    protocol           = "tcp"
    source_prefix      = "203.0.113.0/24"
    destination_prefix = "0.0.0.0/0"

    destination_port_range = "6443"
  }
}

resource "k0rdent_kubernetes_cluster" "filtered" {
  id           = "filtered-01"
  cluster_type = data.k0rdent_cluster_type.gpu.self_link

  security_groups = [k0rdent_security_group.api.id]
}
