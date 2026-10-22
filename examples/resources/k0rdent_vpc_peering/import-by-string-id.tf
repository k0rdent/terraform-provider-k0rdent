# Takes every id form `terraform import` does. A peering is addressed under
# its local VPC. The first command below names that VPC, the second names the
# scope too.
import {
  to = k0rdent_vpc_peering.example
  id = "us-texas-1/cp-production/vpc-training/to-inference"
}
