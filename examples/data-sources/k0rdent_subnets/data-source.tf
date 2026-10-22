# The platform picks a cluster's CIDRs, so read them to write rules against.
data "k0rdent_subnets" "pods" {
  # The one network a cluster made for itself. one() fails the plan if there
  # isn't exactly one.
  network = one([for n in data.k0rdent_networks.all.networks : n.id if n.cluster_id != null])
}

resource "k0rdent_security_group" "from_pods" {
  id = "from-pods"

  # One ingress rule per CIDR. `ingress.value` is the current CIDR.
  dynamic "ingress" {
    for_each = data.k0rdent_subnets.pods.cidrs
    content {
      action             = "permit"
      protocol           = "tcp"
      source_prefix      = ingress.value
      destination_prefix = "0.0.0.0/0"

      destination_port_range = "443"
    }
  }
}
