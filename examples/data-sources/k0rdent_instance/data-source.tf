# One host, by the id k0rdent_instances reports.
data "k0rdent_instance" "head" {
  id = "inst-0123456789abcdef0123456789abcdef"
}

# Its InfiniBand GUIDs, for a file store grant's `guid`.
output "infiniband_guids" {
  value = [for nic in data.k0rdent_instance.head.network_interfaces : nic.guid if nic.type == "infiniband"]
}
