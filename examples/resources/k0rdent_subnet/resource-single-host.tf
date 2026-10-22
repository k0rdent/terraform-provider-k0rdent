# One address, such as a control-plane VIP. A /32 has no gateway.
resource "k0rdent_subnet" "api_vip" {
  id      = "api-vip"
  network = k0rdent_network.training.id

  address_pool  = "/v1/regions/us-texas-1/networking/address-pools/tenant"
  prefix_length = 32
}

output "api_vip" {
  value = k0rdent_subnet.api_vip.cidr
}
