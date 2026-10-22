# Every security group that applies to the cluster, its own and its VPCs',
# in the order they apply.
data "k0rdent_effective_security_groups" "training" {
  object_kind = "cluster"
  object_id   = k0rdent_kubernetes_cluster.training.id
}

# For each VPC id, the groups that apply and where each was bound. An empty
# list means nothing filters that VPC, which is normal on some fabrics.
output "what_actually_filters_it" {
  value = {
    for v in data.k0rdent_effective_security_groups.training.vpcs :
    v.id => [for g in v.security_groups : "${g.id} (bound by ${g.source} ${g.source_id})"]
  }
}
