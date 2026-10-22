# Every VPC in the project, for inspection. To name a cluster's VPC in a
# resource, use the cluster's `vpc` attribute instead.
data "k0rdent_vpcs" "all" {}

output "vpcs_by_owner" {
  value = { for v in data.k0rdent_vpcs.all.vpcs : v.id => v.owner_kind }
}

# VPCs still being updated.
data "k0rdent_vpcs" "settling" {
  state = "updating"
}
