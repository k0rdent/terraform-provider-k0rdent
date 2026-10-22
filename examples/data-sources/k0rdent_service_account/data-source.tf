# A service account another configuration owns. Fails the plan if it isn't
# active.

data "k0rdent_service_account" "deployer" {
  organization_id = "org-ml-platform"
  id              = "sa-ml-platform-deployer"

  lifecycle {
    postcondition {
      condition     = self.status == "active"
      error_message = "${self.id} is ${self.status}, so nothing can authenticate as it."
    }
  }
}

output "deployer_client_id" {
  value = data.k0rdent_service_account.deployer.client_id
}
