# Share types differ between regions, so pick one by capability instead of
# hardcoding its id.
data "k0rdent_share_types" "available" {}

locals {
  # The id of the share type whose `tier` capability is "performance". one()
  # fails the plan unless exactly one matches.
  fast = one([
    for s in data.k0rdent_share_types.available.share_types :
    s.id if lookup(s.capabilities, "tier", "") == "performance"
  ])
}

# A store two hosts mount, keyed by address. Removing an entry detaches it.
resource "k0rdent_file_store" "datasets" {
  id           = "training-datasets"
  display_name = "Training datasets"
  share_type   = local.fast

  # Can only grow. The plan refuses a lower value.
  capacity_gib = 4096

  consumers = {
    "10.0.1.20" = { mount_path = "/mnt/datasets" }
    "10.0.1.21" = { mount_path = "/mnt/datasets" }
  }
}

# A store every host in an instance group mounts, keyed by the group's id.
# `network` says which fabric's addresses to grant.
resource "k0rdent_file_store" "checkpoints" {
  id           = "training-checkpoints"
  share_type   = "general"
  capacity_gib = 16384

  consumers = {
    # Parentheses make the key an expression, not the literal text.
    (k0rdent_instance_group.trainers.id) = {
      type       = "instanceGroup"
      mount_path = "/mnt/checkpoints"

      network = {
        type = "ethernet"
      }
    }
  }
}

# Where a tenant host mounts from.
output "checkpoint_mount" {
  value = "${k0rdent_file_store.checkpoints.preferred_export_location.server}:${k0rdent_file_store.checkpoints.preferred_export_location.path}"
}

# The addresses each entry was granted, keyed by entry. If the group scales and
# the grant falls behind, the entry shows `failed` and the next plan resends it.
output "checkpoint_grants" {
  value = {
    for ref, c in k0rdent_file_store.checkpoints.consumer_status :
    ref => c.resolved_addresses
  }
}
