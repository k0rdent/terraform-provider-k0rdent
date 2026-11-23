# Deprecated. The platform is removing kubeconfig records, so this data source
# is removed in v1.0.0. Existing configurations can keep using it until then.
# For new configurations, use the ephemeral resource's raw attribute for the
# kubeconfig and the cluster's state attribute for readiness.
data "k0rdent_cluster_kubeconfig" "training" {
  cluster_id = k0rdent_kubernetes_cluster.training.id
}

output "kubeconfig_status" {
  value = data.k0rdent_cluster_kubeconfig.training.status
}
