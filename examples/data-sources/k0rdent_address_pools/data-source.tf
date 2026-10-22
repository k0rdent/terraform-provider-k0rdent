# The AddressPools a region gives subnet CIDRs from. Set `only_routable` if
# the subnet needs `allocate_gateway`.

data "k0rdent_address_pools" "all" {}

locals {
  # Active pools that can give a /24: each pool's self_link, keyed by id. A
  # map's keys sort, so the pick below is stable.
  pools_for_a_24 = {
    for p in data.k0rdent_address_pools.all.address_pools : p.id => p.self_link
    if p.state == "active" && p.min_prefix_length <= 24 && p.max_prefix_length >= 24
  }
}

# `self_link`, not `id`, is what a subnet's `address_pool` takes.
resource "k0rdent_subnet" "pods" {
  id      = "pods"
  network = "training-fabric"

  # The first matching pool. try() gives "" when there's none, so the
  # precondition below reports it instead of an index error.
  address_pool  = try(values(local.pools_for_a_24)[0], "")
  prefix_length = 24

  lifecycle {
    precondition {
      condition     = length(local.pools_for_a_24) > 0
      error_message = "No active AddressPool in this region serves a /24."
    }
  }
}
