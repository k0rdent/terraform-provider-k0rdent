# A subscription another configuration owns, read for its delivery health.

data "k0rdent_webhook_subscription" "cluster_events" {
  id = "0f8fad5b-d9cb-469f-a165-70867728950e"

  # Fail the plan if the endpoint is refusing deliveries. Failed deliveries
  # aren't retried.
  lifecycle {
    postcondition {
      condition     = self.consecutive_failures == 0
      error_message = "The endpoint has refused ${self.consecutive_failures} deliveries since ${coalesce(self.failing_since, "an unknown time")}."
    }
  }
}
