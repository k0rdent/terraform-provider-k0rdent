# One version of a ClusterType: its Kubernetes version, status and node pools.
data "k0rdent_cluster_type_version" "known_good" {
  cluster_type = "gpu-training"
  version_id   = "22222222-2222-2222-2222-222222222222"
}

# Warn if the version is no longer `available`, so it can't back a new
# cluster.
check "still_available" {
  assert {
    condition     = data.k0rdent_cluster_type_version.known_good.status == "available"
    error_message = "ClusterType version ${data.k0rdent_cluster_type_version.known_good.version} is ${data.k0rdent_cluster_type_version.known_good.status}, so it cannot back a new cluster."
  }
}

output "pool_ids" {
  value = [for p in data.k0rdent_cluster_type_version.known_good.node_pools : p.id]
}
