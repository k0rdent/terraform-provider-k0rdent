# Cancels the cluster's current operation, such as a stuck upgrade, and keeps
# the cluster. It only runs when you invoke it by name:
#
#   terraform apply -invoke=action.k0rdent_cluster_cancel_operation.training
action "k0rdent_cluster_cancel_operation" "training" {
  config {
    cluster_id = k0rdent_kubernetes_cluster.training.id

    timeouts {
      invoke = "10m"
    }
  }
}

# Cancel only the operation seen at plan time. If another one has started by
# then, the invoke refuses.
data "k0rdent_kubernetes_cluster" "training" {
  id = k0rdent_kubernetes_cluster.training.id

  lifecycle {
    # Stop the plan if there's no operation id. A null execution_id would
    # cancel whatever is running. try() returns null when `operation` is null.
    postcondition {
      condition     = try(self.operation.execution_id, null) != null
      error_message = "The cluster reports no operation id, so there's nothing to pin the cancel to."
    }
  }
}

action "k0rdent_cluster_cancel_operation" "pinned" {
  config {
    cluster_id   = k0rdent_kubernetes_cluster.training.id
    execution_id = data.k0rdent_kubernetes_cluster.training.operation.execution_id
  }
}

# A cluster outside the provider's scope needs its scope named here too.
action "k0rdent_cluster_cancel_operation" "elsewhere" {
  config {
    cluster_id = "training-02"
    region     = "eu-west-1"
    project    = "cp-research"
  }
}
