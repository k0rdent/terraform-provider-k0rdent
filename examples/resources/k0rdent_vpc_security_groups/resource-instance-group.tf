# An instance group has its own VPC. Take it from the group, not a data
# source, so a change to the group doesn't replace this binding.
resource "k0rdent_vpc_security_groups" "hosts" {
  vpc = k0rdent_instance_group.hosts.vpc

  security_groups = [
    k0rdent_security_group.office_access.id,
  ]
}
