# Read a cluster another configuration owns, without managing it.

data "k0rdent_kubernetes_cluster" "training" {
  id = "ml-training"

  # Both default to the provider's scope. Name them to read a cluster in
  # another region or project.
  # region  = "us-texas-1"
  # project = "cp-production"
}

# Health, control plane state and the current operation are only here, not on
# the resource.
output "is_degraded" {
  value = data.k0rdent_kubernetes_cluster.training.health_state != "healthy"
}

# An upgrade's progress, or null when none is running. try() gives null when
# there's no operation.
output "upgrade" {
  value = data.k0rdent_kubernetes_cluster.training.upgrade_target_version_id == null ? null : {
    target        = data.k0rdent_kubernetes_cluster.training.upgrade_target_version_id
    stage         = try(data.k0rdent_kubernetes_cluster.training.operation.current_stage, null)
    percent       = try(data.k0rdent_kubernetes_cluster.training.operation.percent_complete, null)
    started_at    = try(data.k0rdent_kubernetes_cluster.training.operation.started_at, null)
    control_plane = data.k0rdent_kubernetes_cluster.training.control_plane_message
  }
}

# Warn in the plan if the cluster isn't active. A check doesn't stop the run.
# coalesce() fills in text for a null state or message.
check "cluster_is_up" {
  assert {
    condition     = data.k0rdent_kubernetes_cluster.training.state == "active"
    error_message = "Cluster ml-training is ${coalesce(data.k0rdent_kubernetes_cluster.training.state, "in no reported state")}: ${coalesce(data.k0rdent_kubernetes_cluster.training.state_message, "no reason given")}."
  }
}

# The API server URL. For the CA certificate, use the ephemeral kubeconfig.
output "api_endpoint" {
  value = data.k0rdent_kubernetes_cluster.training.api_endpoint
}
