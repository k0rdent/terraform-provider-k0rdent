# Only the groups you can manage, without each VPC's default group.
data "k0rdent_security_groups" "mine" {
  only_tenant_owned = true
}

# Active groups tagged team = "ml".
data "k0rdent_security_groups" "ml" {
  tags  = { team = "ml" }
  state = "active"
}
