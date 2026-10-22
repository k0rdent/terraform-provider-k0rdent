# Workload storage through the kubeconfig hand-off. `make live-storage` runs
# Terraform against this directory, pointed at a real control plane. It
# creates a cluster, connects the kubernetes provider to it through
# `k0rdent_cluster_kubeconfig`, claims a volume from the cluster's own storage
# class, mounts it in a pod and destroys all of it again.
#
# The cluster provisions the storage itself. The provider only supplies the
# connection the kubernetes provider needs, in the same run.
#
# Needs a real deployment, not kind: the cluster needs a default
# StorageClass and a worker for the pod. TESTING.md, "make live-storage",
# lists what a run needs. Endpoint, region, project and credential come from
# K0RDENT_*.

terraform {
  # Ephemeral values in a provider block arrived in 1.10.
  required_version = ">= 1.10"

  required_providers {
    k0rdent = {
      source = "k0rdent/k0rdent"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.36"
    }
  }
}

provider "k0rdent" {}

# ---------------------------------------------------------------------------
# 1. A cluster with a worker to run the pod on
# ---------------------------------------------------------------------------

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

resource "k0rdent_kubernetes_cluster" "storage" {
  id           = var.cluster_name
  cluster_type = data.k0rdent_cluster_type.chosen.self_link

  display_name = "Terraform live storage check"
  description  = "Created by examples/live/storage. Safe to delete."

  tags = {
    owner = "terraform-provider-live-test"
  }

  # At least one node, so the pod has somewhere to run.
  node_pool = {
    (local.worker_pool.id) = {
      node_count = var.worker_nodes
    }
  }
}

# ---------------------------------------------------------------------------
# 2. The hand-off
# ---------------------------------------------------------------------------

ephemeral "k0rdent_cluster_kubeconfig" "storage" {
  # Read the kubeconfig after the cluster exists. `id` is known at plan time,
  # but `state` isn't until the create has run.
  cluster_id = k0rdent_kubernetes_cluster.storage.state != null ? k0rdent_kubernetes_cluster.storage.id : null

  timeouts {
    open = "15m"
  }

  # Stop the run if the kubeconfig has nothing the provider below can use.
  # Otherwise it'd fall back to KUBE_* settings and maybe another cluster.
  lifecycle {
    postcondition {
      condition     = self.host != null && self.token != null
      error_message = "The kubeconfig for ${var.cluster_name} carries no host or no token to hand over, so the kubernetes provider would fall back to KUBE_* settings and reach some other cluster."
    }
  }
}

# `cluster_ca_certificate` is already PEM, so it needs no base64decode. The
# token is short-lived. Each plan or destroy gets a new one.
provider "kubernetes" {
  host                   = ephemeral.k0rdent_cluster_kubeconfig.storage.host
  cluster_ca_certificate = ephemeral.k0rdent_cluster_kubeconfig.storage.cluster_ca_certificate
  token                  = ephemeral.k0rdent_cluster_kubeconfig.storage.token
}

# ---------------------------------------------------------------------------
# 3. A claim on the cluster's storage, and a pod that mounts it
# ---------------------------------------------------------------------------

locals {
  claim_name = "${var.cluster_name}-probe"
}

resource "kubernetes_persistent_volume_claim_v1" "probe" {
  metadata {
    name      = local.claim_name
    namespace = var.namespace
    labels = {
      "app.kubernetes.io/managed-by" = "terraform-provider-live-test"
    }
  }

  spec {
    access_modes = ["ReadWriteOnce"]

    # Null uses the cluster's default StorageClass.
    storage_class_name = var.storage_class_name

    resources {
      requests = {
        storage = var.claim_size
      }
    }
  }

  # The create fails if the claim isn't Bound within the timeout.
  wait_until_bound = true

  timeouts {
    create = "10m"
  }
}

# The pod names the claim through the local, not the resource, so both are
# created together. With a WaitForFirstConsumer class the claim only binds once
# the pod is scheduled, so waiting on each other would deadlock.
resource "kubernetes_pod_v1" "probe" {
  metadata {
    name      = "${var.cluster_name}-probe"
    namespace = var.namespace
    labels = {
      "app.kubernetes.io/managed-by" = "terraform-provider-live-test"
    }
  }

  spec {
    restart_policy = "Never"

    container {
      name  = "probe"
      image = var.probe_image

      # Write to the volume and read it back, then stay Running.
      command = ["sh", "-c", "echo bound > /data/probe && cat /data/probe && exec sleep 3600"]

      volume_mount {
        name       = "data"
        mount_path = "/data"
      }
    }

    volume {
      name = "data"
      persistent_volume_claim {
        claim_name = local.claim_name
      }
    }
  }

  # The create waits for the pod to run, with the volume mounted.
  timeouts {
    create = "10m"
  }
}

# Warns if the claim has no volume or no storage class. A check only warns,
# so scripts/live-storage.sh fails the run on it.
check "claim_bound" {
  data "kubernetes_persistent_volume_claim_v1" "probe" {
    metadata {
      name      = kubernetes_persistent_volume_claim_v1.probe.metadata[0].name
      namespace = var.namespace
    }
  }

  assert {
    condition     = data.kubernetes_persistent_volume_claim_v1.probe.spec[0].volume_name != ""
    error_message = "The claim ${local.claim_name} isn't bound to a volume. The cluster's storage class didn't provision one."
  }

  assert {
    condition     = data.kubernetes_persistent_volume_claim_v1.probe.spec[0].storage_class_name != ""
    error_message = "The claim ${local.claim_name} names no storage class, so the cluster has no default class and none was given."
  }
}

# Warns if the pod isn't Running. Its command fails the pod if the write or
# the read fails.
check "pod_running" {
  data "kubernetes_pod_v1" "probe" {
    metadata {
      name      = kubernetes_pod_v1.probe.metadata[0].name
      namespace = var.namespace
    }
  }

  assert {
    condition     = data.kubernetes_pod_v1.probe.status == "Running"
    error_message = "The pod ${kubernetes_pod_v1.probe.metadata[0].name} isn't Running, so it couldn't write through the claim."
  }
}

# ---------------------------------------------------------------------------
# Inputs
# ---------------------------------------------------------------------------

variable "cluster_name" {
  type        = string
  description = "Cluster id, which also names the claim and the pod. scripts/live-storage.sh generates a unique one per run."
  default     = "tf-live-storage"
}

variable "cluster_type_id" {
  type        = string
  description = "Which cluster type to use. Null picks the first in the catalog."
  default     = null
}

variable "worker_nodes" {
  type        = number
  description = "Nodes in the worker pool. At least one, because the pod has to be scheduled for the claim to be used."
  default     = 1

  validation {
    condition     = var.worker_nodes >= 1
    error_message = "worker_nodes must be at least 1: a pod can't mount the claim with nowhere to run."
  }
}

variable "namespace" {
  type        = string
  description = "Namespace for the claim and the pod. It has to exist, and the caller's token has to be allowed to create both in it."
  default     = "default"
}

variable "storage_class_name" {
  type        = string
  description = "StorageClass to claim from. Null uses the cluster's default class, which is how a workload gets storage."
  default     = null
}

variable "claim_size" {
  type        = string
  description = "Storage the claim requests."
  default     = "1Gi"
}

variable "probe_image" {
  type        = string
  description = "Image for the pod that mounts the claim. It needs `sh`. Point it at a mirror where the cluster can't pull from Docker Hub."
  default     = "busybox:1.36"
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------

# From the claim's state. Nothing here comes from the kubeconfig, so it can be
# an output.
output "claim" {
  description = "The claim as the cluster bound it: its class and the volume provisioned for it."
  value = {
    name          = kubernetes_persistent_volume_claim_v1.probe.metadata[0].name
    namespace     = kubernetes_persistent_volume_claim_v1.probe.metadata[0].namespace
    storage_class = kubernetes_persistent_volume_claim_v1.probe.spec[0].storage_class_name
    volume        = kubernetes_persistent_volume_claim_v1.probe.spec[0].volume_name
  }
}

output "pod" {
  description = "The pod that mounts the claim."
  value = {
    name      = kubernetes_pod_v1.probe.metadata[0].name
    namespace = kubernetes_pod_v1.probe.metadata[0].namespace
  }
}

output "cluster" {
  description = "The cluster the claim was made in."
  value = {
    id        = k0rdent_kubernetes_cluster.storage.id
    state     = k0rdent_kubernetes_cluster.storage.state
    self_link = k0rdent_kubernetes_cluster.storage.self_link
  }
}
