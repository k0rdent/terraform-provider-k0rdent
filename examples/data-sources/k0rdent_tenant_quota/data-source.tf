data "k0rdent_tenant_quota" "org" {
  organization_id = "org-example"
}

# Size the pool to fit the organization's quota. This doesn't check hardware
# is free.
resource "k0rdent_kubernetes_cluster" "training" {
  id           = "training"
  cluster_type = data.k0rdent_cluster_type.chosen.self_link

  node_pool = {
    "gpu-workers" = {
      # At most 8, or less if the quota allows less. `headroom` is null when
      # there's no limit, so coalesce() treats that as 8.
      node_count = min(8, coalesce(data.k0rdent_tenant_quota.org.dimensions["node_count"].headroom, 8))
    }
  }
}
