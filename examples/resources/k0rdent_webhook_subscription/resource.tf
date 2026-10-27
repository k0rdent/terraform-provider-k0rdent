# Sends cluster events to an endpoint, each signed with a secret.
#
# The secret stays out of state and plans: it comes from an ephemeral variable
# (set TF_VAR_webhook_signing_secret) into the write-only `secret_wo`. Needs
# Terraform 1.11 or later.
variable "webhook_signing_secret" {
  type      = string
  sensitive = true
  ephemeral = true
}

resource "k0rdent_webhook_subscription" "cluster_events" {
  url = "https://hooks.example.com/k0rdent"

  # Leave it out to receive every event.
  event_types = [
    "io.k0rdent.workflow.cluster.create.v1",
    "io.k0rdent.workflow.cluster.create.failed.v1",
  ]

  secret_wo = var.webhook_signing_secret

  # Terraform can't see a write-only value change, so bump this with every new
  # secret. The old secret keeps working for 24 hours.
  secret_wo_version = 1

  # For a leaked secret, switch at once instead.
  # rotation_overlap = false
}

# Compare with each delivery's X-K0r-Signature-Version header to check a
# receiver has the new secret.
output "signing_secret_version" {
  value = k0rdent_webhook_subscription.cluster_events.secret_version
}
