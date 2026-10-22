# Fail the plan if the organization isn't active, instead of partway through
# the apply.
data "k0rdent_organization" "acme" {
  id = "org-acme"

  lifecycle {
    postcondition {
      condition     = self.status == "active"
      error_message = "org-acme is ${self.status}, so a project cannot be created in it."
    }
  }
}

resource "k0rdent_project" "research" {
  id              = "research"
  organization_id = data.k0rdent_organization.acme.id
}
