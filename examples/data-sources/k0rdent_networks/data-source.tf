# Includes networks a cluster made for itself. Those have a `cluster_id` and
# can't be managed.
data "k0rdent_networks" "all" {
  # Read after the cluster exists, so its networks are listed.
  depends_on = [k0rdent_kubernetes_cluster.training]
}

output "cluster_networks" {
  value = [for n in data.k0rdent_networks.all.networks : n.id if n.cluster_id != null]
}

# Only the ones a k0rdent_network could manage.
data "k0rdent_networks" "mine" {
  only_tenant_owned = true
}

# The production network, found by tags instead of a hardcoded id. The
# postcondition fails the plan unless exactly one matches.
data "k0rdent_networks" "prod" {
  tags               = { env = "prod", team = "ml" }
  display_name_regex = "^prod-"
  state              = "active"

  lifecycle {
    postcondition {
      condition     = length(self.ids) == 1
      error_message = "Expected exactly one active network tagged env=prod, team=ml."
    }
  }
}

output "prod_network" {
  value = one(data.k0rdent_networks.prod.ids)
}
