# Metadata only. For the kubeconfig itself, use the ephemeral resource of the
# same name, which isn't saved to state.
data "k0rdent_cluster_kubeconfig" "training" {
  cluster_id = k0rdent_kubernetes_cluster.training.id
}

output "kubeconfig_status" {
  value = data.k0rdent_cluster_kubeconfig.training.status
}
