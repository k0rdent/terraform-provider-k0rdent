# The demo configuration. `make demo` runs Terraform against this directory,
# pointed at a local fake control plane.
#
# Edit this file. Change `worker_nodes`, add a tag or rename the cluster, then
# run `terraform plan` and `terraform apply` here. `make demo-up` leaves the
# fake running between runs.

terraform {
  required_providers {
    k0rdent = {
      source = "k0rdent/k0rdent"
    }
  }
}

provider "k0rdent" {
  endpoint = var.endpoint
  region   = var.region
  project  = var.project

  # Short, because the fake answers at once. Leave both unset for a real
  # endpoint.
  operation_timeout = "2m"
  poll_interval     = "200ms"
}

# ---------------------------------------------------------------------------
# 0. Find out where you can build
# ---------------------------------------------------------------------------
# The regions and projects you can use, for the provider block.

data "k0rdent_regions" "all" {}

data "k0rdent_projects" "mine" {}

# ---------------------------------------------------------------------------
# 1. Look up a cluster type
# ---------------------------------------------------------------------------
# The cluster types in the region. A cluster's `cluster_type` takes an
# entry's `self_link`.

data "k0rdent_cluster_types" "available" {}

# The list doesn't include node pools, so read the chosen one. coalesce()
# takes var.cluster_type_id, or the first type if that's null.
data "k0rdent_cluster_type" "chosen" {
  id = coalesce(var.cluster_type_id, data.k0rdent_cluster_types.available.cluster_types[0].id)
}

locals {
  # The pools you can resize. Control-plane pools can't be.
  worker_pools = [for p in data.k0rdent_cluster_type.chosen.node_pools : p if p.role == "worker"]
  # The first of them. This works with any cluster type.
  worker_pool = local.worker_pools[0]
}

# ---------------------------------------------------------------------------
# 2. Create a cluster
# ---------------------------------------------------------------------------

resource "k0rdent_kubernetes_cluster" "demo" {
  id = var.cluster_name

  # The unversioned reference. The platform binds a specific version, shown in
  # `cluster_type_resolved`, and the plan still shows no change.
  cluster_type = data.k0rdent_cluster_type.chosen.self_link

  display_name = "Terraform demo cluster"
  description  = "Created by examples/demo against the local fake control plane."

  tags = {
    owner = "terraform-demo"
  }

  # Only the pools named here change. The others keep their defaults.
  node_pool = {
    (local.worker_pool.id) = {
      node_count = var.worker_nodes
    }
  }
}

# ---------------------------------------------------------------------------
# 3. Read a kubeconfig
# ---------------------------------------------------------------------------
# Metadata only. The kubeconfig itself comes from the ephemeral resource of
# the same name, which is never saved to state.

data "k0rdent_cluster_kubeconfig" "demo" {
  cluster_id = k0rdent_kubernetes_cluster.demo.id
}

# ---------------------------------------------------------------------------
# 4. An instance group: hosts without a control plane
# ---------------------------------------------------------------------------

resource "k0rdent_instance_group" "demo" {
  id           = "${var.cluster_name}-hosts"
  cluster_type = data.k0rdent_cluster_type.chosen.self_link
  display_name = "Terraform demo hosts"

  node_pool = {
    (local.worker_pool.id) = {
      size = var.host_count
    }
  }
}

# ---------------------------------------------------------------------------
# Variables
# ---------------------------------------------------------------------------

variable "endpoint" {
  type        = string
  description = "Base URL of the API. The demo script sets this to the local fake."
}

variable "region" {
  type        = string
  description = "Region id. k0rdent_regions lists them. The fake serves us-texas-1."
  default     = "us-texas-1"
}

variable "project" {
  type        = string
  description = "Project id, as the k0rdent_projects data source reports it."
  default     = "demo"
}

variable "cluster_name" {
  type        = string
  description = "Cluster id. Edit this and re-apply to see a replacement plan."
  default     = "tf-demo"
}

variable "cluster_type_id" {
  type        = string
  description = "Which cluster type to use. Null picks the first in the catalog."
  default     = null
}

variable "worker_nodes" {
  type        = number
  description = "Nodes in the cluster's worker pool. Change this and re-apply to scale."
  default     = 2
}

variable "host_count" {
  type        = number
  description = "Hosts in the instance group. Growing this scales in place, shrinking it replaces the group."
  default     = 1
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------

output "catalog" {
  description = "Every cluster type the region offers."
  value       = [for ct in data.k0rdent_cluster_types.available.cluster_types : "${ct.id} (${ct.display_name}, ${ct.status})"]
}

output "node_pools" {
  description = "Node pools on the chosen cluster type, with the counts they accept."
  value = [for p in data.k0rdent_cluster_type.chosen.node_pools :
    "${p.id}: ${p.role}, default ${coalesce(p.node_count_default, 0)}, max ${coalesce(p.node_count_max, 0)}"
  ]
}

output "cluster" {
  description = "What the platform reports for the cluster."
  value = {
    id         = k0rdent_kubernetes_cluster.demo.id
    state      = k0rdent_kubernetes_cluster.demo.state
    node_count = k0rdent_kubernetes_cluster.demo.node_count
    node_pool  = k0rdent_kubernetes_cluster.demo.node_pool
    self_link  = k0rdent_kubernetes_cluster.demo.self_link
  }
}

output "cluster_type_round_trip" {
  description = <<-EOT
    `written` is what the configuration asked for, `returned` is the version
    the platform bound. They can differ while a plan still shows no changes.
  EOT
  value = {
    written  = k0rdent_kubernetes_cluster.demo.cluster_type
    returned = k0rdent_kubernetes_cluster.demo.cluster_type_resolved
  }
}

output "kubeconfig" {
  description = "Metadata for the cluster's existing kubeconfig. The body is not downloaded."
  value = {
    id         = data.k0rdent_cluster_kubeconfig.demo.id
    status     = data.k0rdent_cluster_kubeconfig.demo.status
    issued_for = data.k0rdent_cluster_kubeconfig.demo.issued_for
    available  = data.k0rdent_cluster_kubeconfig.demo.available
  }
}

output "instance_group" {
  description = "What the platform reports for the instance group."
  value = {
    id        = k0rdent_instance_group.demo.id
    state     = k0rdent_instance_group.demo.state
    node_pool = k0rdent_instance_group.demo.node_pool
  }
}

output "regions" {
  description = "Region ids this credential can reach."
  value       = data.k0rdent_regions.all.ids
}

output "projects" {
  description = "Project slugs this credential can reach."
  value       = data.k0rdent_projects.mine.ids
}
