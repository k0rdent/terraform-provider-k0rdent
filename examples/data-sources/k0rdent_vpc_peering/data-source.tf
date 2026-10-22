# One VPC peering by name, under one of its VPCs. Warns if it isn't active.

data "k0rdent_vpc_peering" "to_shared_services" {
  id  = "training-to-shared"
  vpc = "training-vpc"
}

check "peering_is_up" {
  assert {
    condition     = data.k0rdent_vpc_peering.to_shared_services.state == "active"
    error_message = "The peering to ${data.k0rdent_vpc_peering.to_shared_services.remote_vpc} is ${data.k0rdent_vpc_peering.to_shared_services.state}, so nothing routes across it yet."
  }
}
