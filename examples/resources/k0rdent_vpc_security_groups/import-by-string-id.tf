# Takes every id form `terraform import` does. The VPC's id is the address.
# The first command below gives that id, the second names the scope too.
#
# A VPC with only its default group imports as security_groups = [].
import {
  to = k0rdent_vpc_security_groups.example
  id = "us-texas-1/cp-production/vpc-training"
}
