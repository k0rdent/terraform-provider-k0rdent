# The lifecycle scripts/real-api-check.sh records against the fake and against
# the real API, so fakedrift can compare the two: a cluster and an instance
# group, each with an SSH user whose key is write-only. The cluster names
# every pool its type has, so the plan after an import is empty on both.

terraform {
  required_version = ">= 1.11"
  required_providers {
    k0rdent = {
      source = "k0rdent/k0rdent"
    }
  }
}

variable "cluster_type" {
  type = string
}

variable "ssh_public_key" {
  type = string
}

data "k0rdent_cluster_type" "chosen" {
  id = var.cluster_type
}

locals {
  workers = [for p in data.k0rdent_cluster_type.chosen.node_pools : p if p.role == "worker"][0]
}

resource "k0rdent_kubernetes_cluster" "c" {
  id                 = "real-api-check-c"
  cluster_type       = data.k0rdent_cluster_type.chosen.self_link
  node_pool          = { for p in data.k0rdent_cluster_type.chosen.node_pools : p.id => { node_count = coalesce(p.node_count_min, 0) } }
  ssh_access         = [{ username = "ops", sudo = "ALL=(ALL) NOPASSWD:ALL" }]
  ssh_public_keys_wo = { ops = [var.ssh_public_key] }
}

resource "k0rdent_instance_group" "g" {
  id                 = "real-api-check-g"
  cluster_type       = data.k0rdent_cluster_type.chosen.self_link
  node_pool          = { (local.workers.id) = { size = coalesce(local.workers.node_count_min, 0) } }
  ssh_access         = [{ username = "ops" }]
  ssh_public_keys_wo = { ops = [var.ssh_public_key] }
}
