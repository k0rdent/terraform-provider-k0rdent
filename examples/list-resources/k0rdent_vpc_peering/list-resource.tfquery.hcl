# Every peering on every VPC in the provider's project.
list "k0rdent_vpc_peering" "all" {
  provider = k0rdent
}

# The peerings on one VPC.
list "k0rdent_vpc_peering" "training" {
  provider = k0rdent

  config {
    vpc = "vpc-ml-training"
  }
}
