# Most other resources are created inside a project, so this usually comes
# first. It sits in an organization, not in a project.

resource "k0rdent_project" "production" {
  # Optional. Left out, the platform picks one. Set it if a provider block
  # here uses this project, since that needs the id at plan time.
  id = "cp-production"

  # This provider can't create an organization. Read the id from the
  # projects data source, or ask an administrator.
  organization_id = "org-ml-platform"

  # Everything in the project uses this region. Left out, it's the provider's.
  # Changing it replaces the project. `k0rdent_regions` lists valid ones in
  # `project_region_ids`.
  region = "us-texas-1"

  display_name = "Control plane production"
  description  = "Production workloads for the ml-platform team."

  # Only delete waits.
  timeouts {
    delete = "15m"
  }
}

# The value for another configuration's provider block.
output "project_id" {
  value = k0rdent_project.production.id
}

# Warn if an administrator has suspended or disabled the project. Nothing can
# be built in it then.
check "project_is_usable" {
  assert {
    condition     = k0rdent_project.production.status == "active"
    error_message = "Project ${k0rdent_project.production.id} is ${k0rdent_project.production.status}, so nothing can be created in it."
  }
}
