# The roles a project binding can take, and which service accounts hold a role
# that no longer exists.

data "k0rdent_roles" "project" {
  organization_id = "org-ml-platform"
  scope           = "project"
}

data "k0rdent_service_accounts" "production" {
  organization_id = "org-ml-platform"
  scope           = "project"
  scope_id        = "cp-production"
}

# For each service account, the roles it holds that aren't in the list.
# Accounts with none are left out.
output "unknown_roles" {
  value = {
    for sa in data.k0rdent_service_accounts.production.service_accounts :
    sa.id => setsubtract(sa.roles, data.k0rdent_roles.project.ids)
    if length(setsubtract(sa.roles, data.k0rdent_roles.project.ids)) > 0
  }
}
