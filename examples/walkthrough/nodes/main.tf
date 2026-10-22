# Read only: lists a cluster's nodes, with their addresses and an ssh command
# for each. Apply it once the cluster is up. Set the values in
# terraform.tfvars, next to this file.
#
# Node addresses come from the cluster itself, through its kubeconfig. The
# kubeconfig is ephemeral, so it's never written to state or to disk.

variable "prefix" {
  type        = string
  description = "The prefix the cluster was made with. The cluster is <prefix>-walkthrough."

  validation {
    condition     = can(regex("^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$", var.prefix))
    error_message = "Set prefix in terraform.tfvars to the one the cluster was made with."
  }
}

variable "region" {
  type        = string
  description = "The cluster's region."

  validation {
    condition     = var.region != ""
    error_message = "Set region in terraform.tfvars to the cluster's region."
  }
}

variable "project" {
  type        = string
  description = "The cluster's project. In cluster-own-project/ that's <prefix>-walkthrough."

  validation {
    condition     = var.project != ""
    error_message = "Set project in terraform.tfvars to the cluster's project."
  }
}

terraform {
  required_providers {
    k0rdent = {
      source = "k0rdent/k0rdent"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = "~> 2.36"
    }
  }
}

# The endpoint and the credential come from K0RDENT_* in the environment.
# See setup.sh.
provider "k0rdent" {
  region = var.region
}

ephemeral "k0rdent_cluster_kubeconfig" "this" {
  cluster_id = "${var.prefix}-walkthrough"
  project    = var.project

  lifecycle {
    # Without a host and a credential, the kubernetes provider falls back to
    # your own ~/.kube/config and would list some other cluster's nodes.
    postcondition {
      condition     = self.host != null && (self.token != null || self.client_certificate != null)
      error_message = "The cluster's kubeconfig has no usable host and credential, so this stops rather than read another cluster."
    }
  }
}

provider "kubernetes" {
  host                   = ephemeral.k0rdent_cluster_kubeconfig.this.host
  cluster_ca_certificate = ephemeral.k0rdent_cluster_kubeconfig.this.cluster_ca_certificate
  token                  = ephemeral.k0rdent_cluster_kubeconfig.this.token
  client_certificate     = ephemeral.k0rdent_cluster_kubeconfig.this.client_certificate
  client_key             = ephemeral.k0rdent_cluster_kubeconfig.this.client_key
}

data "kubernetes_nodes" "all" {}

locals {
  # For each node name, its addresses keyed by type, such as InternalIP.
  nodes = {
    for n in data.kubernetes_nodes.all.nodes : n.metadata[0].name => {
      for a in n.status[0].addresses : a.type => a.address
    }
  }
}

# Each node's addresses, as the cluster reports them.
output "nodes" {
  value = local.nodes
}

# An ssh command per node, as the user cluster/ sets up. try() uses the
# ExternalIP if there is one, and the InternalIP otherwise.
output "ssh" {
  value = {
    for name, addr in local.nodes : name => "ssh ops@${try(addr.ExternalIP, addr.InternalIP)}"
  }
}
