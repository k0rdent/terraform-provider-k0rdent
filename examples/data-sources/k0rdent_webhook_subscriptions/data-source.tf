# Every subscription to one endpoint. Use it to find an id to import.

data "k0rdent_webhook_subscriptions" "hooks" {
  url = "https://hooks.example.com/k0rdent"
}

output "subscription_ids" {
  # [*] takes `id` from every subscription.
  value = data.k0rdent_webhook_subscriptions.hooks.subscriptions[*].id
}
