# An Ethernet fabric. The `evpn`, `infiniband` or `nvlink` block picks the kind.
resource "k0rdent_network" "training" {
  id           = "training-fabric"
  display_name = "Training fabric"

  # Merged with the provider's `default_tags` into `tags_all`. These win.
  tags = {
    team = "ml-platform"
  }

  evpn = {
    service_type = "l3vpn"
    mtu          = 9000
    dns_servers  = ["10.0.0.2", "10.0.0.3"]

    # "auto" lets the platform pick. The chosen id is in `vlan_id_resolved`.
    vlan_id = "auto"
  }
}

# An InfiniBand fabric. Without `pkey`, the platform picks one and reports it
# in `pkey_allocated`.
resource "k0rdent_network" "gpu_interconnect" {
  id = "gpu-interconnect"

  infiniband = {
    partition  = "training"
    ip_over_ib = true
  }
}

# An NVLink fabric. With no partition named, the fabric picks one.
resource "k0rdent_network" "nvlink" {
  id     = "nvlink-domain"
  nvlink = {}
}
