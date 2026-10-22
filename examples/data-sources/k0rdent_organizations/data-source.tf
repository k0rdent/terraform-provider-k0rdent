# The organizations the credential can see. k0rdent_project takes one of these
# ids in organization_id.
data "k0rdent_organizations" "active" {
  status = "active"
}

output "organization_ids" {
  value = data.k0rdent_organizations.active.ids
}
