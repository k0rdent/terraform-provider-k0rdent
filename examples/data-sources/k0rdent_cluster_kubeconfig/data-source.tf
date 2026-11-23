# Deprecated. This data source stops working when the platform moves to API 1.4,
# which removes kubeconfig records. It's removed in v1.0.0. Move to the
# ephemeral resource's raw attribute for the kubeconfig now. For readiness, use
# the cluster's state attribute.
data "k0rdent_cluster_kubeconfig" "training" {
  cluster_id = k0rdent_kubernetes_cluster.training.id
}

output "kubeconfig_status" {
  value = data.k0rdent_cluster_kubeconfig.training.status
}
