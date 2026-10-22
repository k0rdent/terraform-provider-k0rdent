# Every project in one organization.
list "k0rdent_project" "acme" {
  provider = k0rdent

  config {
    organization_id = "org-acme"
  }
}
