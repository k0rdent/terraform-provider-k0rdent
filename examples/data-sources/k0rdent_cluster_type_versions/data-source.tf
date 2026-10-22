# The Kubernetes versions a ClusterType offers. You get a Kubernetes version by
# picking a ClusterType version.
data "k0rdent_cluster_type_versions" "training" {
  cluster_type = "gpu-training"
  status       = "available"
}

# The list is oldest first, so reverse it and take the first for the newest.
locals {
  newest = reverse(data.k0rdent_cluster_type_versions.training.versions)[0]
}

output "kubernetes_versions" {
  value = distinct([for v in data.k0rdent_cluster_type_versions.training.versions : v.kubernetes_version])
}

# A version's `self_link` keeps the cluster on that version. The ClusterType's
# own `self_link` follows the newest.
resource "k0rdent_kubernetes_cluster" "pinned" {
  id           = "training-pinned"
  cluster_type = local.newest.self_link
}
