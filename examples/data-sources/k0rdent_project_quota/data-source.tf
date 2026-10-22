data "k0rdent_project_quota" "team" {
  organization_id = "org-example"
  project_id      = "team-a"
}

# `headroom` is the smaller of the organization's and the project's limits.
# `binding_scope` says which.
output "node_headroom" {
  value = {
    nodes      = data.k0rdent_project_quota.team.dimensions["node_count"].headroom
    binding_on = data.k0rdent_project_quota.team.dimensions["node_count"].binding_scope
  }
}
