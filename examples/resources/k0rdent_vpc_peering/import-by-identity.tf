# The same address with each part named. Leave out region and project to
# use the provider's.
import {
  to = k0rdent_vpc_peering.example
  identity = {
    region  = "us-texas-1"
    project = "cp-production"
    vpc     = "vpc-training"
    id      = "to-inference"
  }
}
