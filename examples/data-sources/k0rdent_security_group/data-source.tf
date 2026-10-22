# One security group by name.

data "k0rdent_security_group" "shared_ingress" {
  id = "shared-ingress"
}

# Warn if this is a VPC's default group (owner_kind = "vpc"). Those can't be
# changed and only bind to their own VPC.
check "group_is_ours" {
  assert {
    condition     = data.k0rdent_security_group.shared_ingress.owner_kind == null
    error_message = "shared-ingress is the default group of VPC ${coalesce(data.k0rdent_security_group.shared_ingress.owner_id, "unknown")} and is read-only here."
  }
}

# To see every group that applies to a VPC, read this instead.
data "k0rdent_effective_security_groups" "vpc" {
  object_kind = "vpc"
  object_id   = "training-vpc"
}
