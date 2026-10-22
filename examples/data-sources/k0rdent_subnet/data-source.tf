# One subnet by name. Names are unique only within a network, so name the
# network too. Usually read for the CIDR the platform picked.

data "k0rdent_subnet" "pods" {
  id      = "pods"
  network = "training-fabric"
}

output "pod_cidr" {
  value = data.k0rdent_subnet.pods.cidr
}

# For a firewall or route configured elsewhere.
output "route_target" {
  value = {
    cidr    = data.k0rdent_subnet.pods.cidr
    gateway = data.k0rdent_subnet.pods.gateway
  }
}
