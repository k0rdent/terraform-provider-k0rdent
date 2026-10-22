data "k0rdent_projects" "all" {}

# The provider's `project` takes a project's id.
output "project_slugs" {
  value = data.k0rdent_projects.all.ids
}

# Only the active projects whose display name starts "ml-".
data "k0rdent_projects" "ml" {
  status             = "active"
  display_name_regex = "^ml-"
}
