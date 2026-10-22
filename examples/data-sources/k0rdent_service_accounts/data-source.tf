# Every service account in an organization, and the roles each holds.

data "k0rdent_service_accounts" "all" {
  organization_id = "org-ml-platform"
}

output "roles_by_account" {
  value = {
    for sa in data.k0rdent_service_accounts.all.service_accounts :
    "${sa.display_name} (${sa.scope} ${sa.scope_id})" => sa.roles
  }
}
