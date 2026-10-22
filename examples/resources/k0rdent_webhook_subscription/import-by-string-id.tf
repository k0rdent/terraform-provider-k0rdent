# Takes the subscription's UUID, as `terraform import` does. An import can't
# read the signing secret, so if you set secret_wo, the first apply rotates it.
import {
  to = k0rdent_webhook_subscription.cluster_events
  id = "0f8fad5b-d9cb-469f-a165-70867728950e"
}
