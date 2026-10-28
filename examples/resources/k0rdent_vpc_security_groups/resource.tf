# Order matters: groups apply in list order and the first matching rule wins.
resource "k0rdent_vpc_security_groups" "training" {
  vpc = local.training_vpc

  security_groups = [
    k0rdent_security_group.office_access.id,
    k0rdent_security_group.baseline.id,
  ]
}

# Destroying this puts back the VPC's default group. From API v1.3.0-rc.3 it
# allows traffic inside the VPC and nothing else. `attached` shows what's
# bound.
output "actually_attached" {
  value = k0rdent_vpc_security_groups.training.attached
}
