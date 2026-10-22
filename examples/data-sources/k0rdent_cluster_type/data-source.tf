data "k0rdent_cluster_type" "gpu" {
  id = "gpu-training"
}

# Pass self_link to a cluster to bind it to this type.
output "kubernetes_version" {
  value = data.k0rdent_cluster_type.gpu.kubernetes_version
}
