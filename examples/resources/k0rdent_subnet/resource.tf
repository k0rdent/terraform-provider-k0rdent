resource "k0rdent_subnet" "pods" {
  id      = "pods"
  network = k0rdent_network.training.id

  address_pool  = "/v1/regions/us-texas-1/networking/address-pools/tenant"
  prefix_length = 24

  # Sets `gateway` to the last usable address, if the pool is routable.
  allocate_gateway = true

  exclude_ranges = ["10.0.0.10-10.0.0.20"]
}

# The platform picks the CIDR.
output "pod_cidr" {
  value = k0rdent_subnet.pods.cidr
}
