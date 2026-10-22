# A project another configuration owns, to build in without managing it.

data "k0rdent_project" "platform" {
  id = "platform-shared"

  # Optional. Without it, the organization is looked up, which fails if two
  # organizations use this id.
  organization_id = "org-acme"

  # Fail the plan, not partway through the apply, if the project isn't active.
  lifecycle {
    postcondition {
      condition     = self.status == "active"
      error_message = "platform-shared is ${self.status}, so anything created inside it will be refused."
    }
  }
}

# A provider for that project. Resources use it with `provider = k0rdent.shared`.
provider "k0rdent" {
  alias   = "shared"
  region  = "us-texas-1"
  project = data.k0rdent_project.platform.id
}
