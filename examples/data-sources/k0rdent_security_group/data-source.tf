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

# A VPC's default group, which the platform owns. Its id is random, so take it
# off the VPC's binding, which holds only that group until you bind others.
data "k0rdent_vpc" "training" {
  id = "training-vpc"
}

data "k0rdent_security_group" "training_default" {
  id = one(data.k0rdent_vpc.training.security_groups)
}

# What it allows, one line per rule, in the order they're matched.
output "training_default_rules" {
  value = concat(
    [for r in data.k0rdent_security_group.training_default.ingress : "in:  ${r.action} ${r.protocol} ${r.source_prefix} to ${r.destination_prefix}"],
    [for r in data.k0rdent_security_group.training_default.egress : "out: ${r.action} ${r.protocol} ${r.source_prefix} to ${r.destination_prefix}"],
  )
}

# To see every group that applies to a VPC, read this instead.
data "k0rdent_effective_security_groups" "vpc" {
  object_kind = "vpc"
  object_id   = "training-vpc"
}
