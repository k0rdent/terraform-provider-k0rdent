# Read a file store another configuration owns, from the configuration whose
# hosts mount it.

variable "consumer_id" {
  description = "The instance group or cluster that mounts the store."
  type        = string
}

data "k0rdent_file_store" "datasets" {
  id = "training-datasets"
}

# Warn until the grant is `active`. Until then hosts can't mount. try()
# gives "" if there's no entry for this consumer yet.
check "consumer_can_mount" {
  assert {
    condition     = try(data.k0rdent_file_store.datasets.consumer_status[var.consumer_id].state, "") == "active"
    error_message = "The grant for ${var.consumer_id} on training-datasets is not enforced yet."
  }
}

output "mount_source" {
  value = "${data.k0rdent_file_store.datasets.preferred_export_location.server}:${data.k0rdent_file_store.datasets.preferred_export_location.path}"
}
