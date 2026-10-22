# One Kubernetes cluster, in a project of its own. Terraform creates the
# project, and deletes it last on destroy. This needs a credential scoped to
# the organization. Set the values in terraform.tfvars, next to this file.

variable "prefix" {
  type        = string
  description = "Starts every name here, so two people running the walkthrough don't collide. Your own, such as your username."

  validation {
    condition     = can(regex("^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$", var.prefix))
    error_message = "Set prefix in terraform.tfvars to something of your own, such as your username: lower-case letters, digits and hyphens."
  }
}

variable "region" {
  type        = string
  description = "A region from look."

  validation {
    condition     = var.region != ""
    error_message = "Set region in terraform.tfvars to a region from look."
  }
}

variable "organization" {
  type        = string
  description = "An organization id from look."

  validation {
    condition     = var.organization != ""
    error_message = "Set organization in terraform.tfvars to an organization id from look."
  }
}

variable "cluster_type" {
  type        = string
  description = "A cluster type from look, in that region."

  validation {
    condition     = var.cluster_type != ""
    error_message = "Set cluster_type in terraform.tfvars to a cluster type look listed under your region."
  }
}

locals {
  name = "${var.prefix}-walkthrough"
}

terraform {
  required_providers {
    k0rdent = {
      source = "k0rdent/k0rdent"
    }
  }
}

# The endpoint and the credential come from K0RDENT_* in the environment.
# See setup.sh.
provider "k0rdent" {
  region = var.region
}

resource "k0rdent_project" "this" {
  id              = local.name
  organization_id = var.organization
  display_name    = "${var.prefix} walkthrough"
}

data "k0rdent_cluster_type" "chosen" {
  id = var.cluster_type
}

data "k0rdent_cluster_types" "available" {
  status = "available"
}

locals {
  # The pools you can resize. Control-plane pools can't be.
  worker_pools = [for p in data.k0rdent_cluster_type.chosen.node_pools : p if p.role == "worker"]
  # The first of them. This works with any cluster type.
  workers = local.worker_pools[0]
}

resource "k0rdent_kubernetes_cluster" "walkthrough" {
  id           = local.name
  project      = k0rdent_project.this.id
  cluster_type = data.k0rdent_cluster_type.chosen.self_link
  display_name = "${var.prefix} walkthrough"

  # Start the pool as small as the cluster type allows. coalesce() gives 0
  # when the type sets no minimum.
  node_pool = {
    (local.workers.id) = {
      node_count = coalesce(local.workers.node_count_min, 0)
    }
  }

  # A create on real hardware takes as long as provisioning the nodes.
  timeouts {
    create = "3h"
    delete = "3h"
  }

  lifecycle {
    # Fail the plan if the cluster type isn't available in this region.
    precondition {
      condition     = contains([for ct in data.k0rdent_cluster_types.available.cluster_types : ct.id], var.cluster_type)
      error_message = "Cluster type ${var.cluster_type} isn't available in region ${var.region}. Run look again for the ones that are."
    }
  }
}

output "cluster" {
  value = {
    id                 = k0rdent_kubernetes_cluster.walkthrough.id
    state              = k0rdent_kubernetes_cluster.walkthrough.state
    kubernetes_version = k0rdent_kubernetes_cluster.walkthrough.kubernetes_version
    written            = k0rdent_kubernetes_cluster.walkthrough.cluster_type
    bound              = k0rdent_kubernetes_cluster.walkthrough.cluster_type_resolved
  }
}
