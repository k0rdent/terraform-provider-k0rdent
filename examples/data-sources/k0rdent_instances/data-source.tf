# The hosts one instance group holds, and a file store grant narrowed to one
# host's Ethernet interface.
data "k0rdent_instances" "trainers" {
  instance_group = k0rdent_instance_group.trainers.id
}

locals {
  # Every Ethernet MAC address in the group, keyed by instance id.
  ethernet_macs = {
    for i in data.k0rdent_instances.trainers.instances : i.id => [
      for nic in i.network_interfaces : nic.mac_address if nic.type == "ethernet"
    ]
  }
}

resource "k0rdent_file_store" "datasets" {
  id           = "datasets"
  share_type   = "general"
  capacity_gib = 1024

  consumers = {
    (k0rdent_instance_group.trainers.id) = {
      type = "instanceGroup"
      network = {
        type = "ethernet"
        # The first Ethernet MAC of the group's first host.
        mac_address = local.ethernet_macs[data.k0rdent_instances.trainers.ids[0]][0]
      }
    }
  }
}

# Only the group's running hosts.
data "k0rdent_instances" "trainers_running" {
  instance_group = k0rdent_instance_group.trainers.id
  state          = "running"
}
