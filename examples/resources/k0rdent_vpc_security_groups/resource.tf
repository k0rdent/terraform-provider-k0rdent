# Order matters: groups apply in list order and the first matching rule wins.
resource "k0rdent_vpc_security_groups" "training" {
  vpc = local.training_vpc

  security_groups = [
    k0rdent_security_group.office_access.id,
    k0rdent_security_group.baseline.id,
  ]
}

# Destroying this puts back the VPC's default group, which allows traffic
# inside the VPC and all egress. `attached` shows what's bound.
output "actually_attached" {
  value = k0rdent_vpc_security_groups.training.attached
}
