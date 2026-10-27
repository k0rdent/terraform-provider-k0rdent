# The failed deliveries among one subscription's newest 100, and which of
# them the platform has stopped retrying.

data "k0rdent_webhook_deliveries" "failed" {
  subscription_id = "0f8fad5b-d9cb-469f-a165-70867728950e"
  limit           = 100
  status          = "failed"
}

output "dead_lettered" {
  value = [
    for d in data.k0rdent_webhook_deliveries.failed.deliveries : {
      id            = d.id
      response_code = d.response_code
      last_error    = d.last_error
    } if !d.retry_scheduled
  ]
}
