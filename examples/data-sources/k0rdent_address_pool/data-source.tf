# One AddressPool by name. `k0rdent_address_pools` lists them.

data "k0rdent_address_pool" "tenant" {
  id = "tenant"
}

# A subnet's `address_pool` takes the pool's `self_link`, not its `id`.
resource "k0rdent_subnet" "pods" {
  id            = "pods"
  network       = "training-fabric"
  address_pool  = data.k0rdent_address_pool.tenant.self_link
  prefix_length = 24

  # Fail the plan, not the apply, if the pool isn't active or can't give a /24.
  lifecycle {
    precondition {
      condition     = data.k0rdent_address_pool.tenant.state == "active"
      error_message = "Pool ${data.k0rdent_address_pool.tenant.id} is ${data.k0rdent_address_pool.tenant.state}, not active."
    }
    precondition {
      condition = (
        data.k0rdent_address_pool.tenant.min_prefix_length <= 24 &&
        data.k0rdent_address_pool.tenant.max_prefix_length >= 24
      )
      error_message = "Pool ${data.k0rdent_address_pool.tenant.id} serves /${data.k0rdent_address_pool.tenant.min_prefix_length} to /${data.k0rdent_address_pool.tenant.max_prefix_length}, so it cannot serve a /24."
    }
  }
}

output "reserved_ranges" {
  value = data.k0rdent_address_pool.tenant.exclude_ranges
}
