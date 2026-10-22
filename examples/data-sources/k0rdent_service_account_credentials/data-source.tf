# Warns in the plan two weeks before the account's last credential expires.

data "k0rdent_service_account_credentials" "deployer" {
  organization_id    = "org-ml-platform"
  service_account_id = "sa-ml-platform-deployer"
}

check "deployer_credential_lasts" {
  assert {
    # True if any credential expires more than 336 hours (14 days) from now.
    condition = anytrue([
      for c in data.k0rdent_service_account_credentials.deployer.issued :
      timecmp(c.expires_at, timeadd(plantimestamp(), "336h")) > 0
    ])
    error_message = "Every credential of sa-ml-platform-deployer expires within 14 days. Ask an administrator for a new one."
  }
}
