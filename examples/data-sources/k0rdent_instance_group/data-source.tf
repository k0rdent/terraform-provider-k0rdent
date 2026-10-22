# Read a group of bare hosts another configuration owns.

data "k0rdent_instance_group" "inference" {
  id = "inference-fleet"
}

# Pools with fewer ready hosts than asked for. Normal for a while after a
# create or a scale.
output "pools_short_of_hosts" {
  value = [
    for name, p in data.k0rdent_instance_group.inference.node_pools : name
    if p.ready_count != null && p.ready_count < p.desired_count
  ]
}

# Quote this when asking support about a failed group.
output "execution_id" {
  value = data.k0rdent_instance_group.inference.execution_id
}
