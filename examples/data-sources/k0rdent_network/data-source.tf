# One network by name, including networks a cluster made for itself, which
# the resource can't manage.

data "k0rdent_network" "training" {
  id = "training-fabric"
}

# The VLAN the network got, even if it was created with vlan_id = "auto".
output "vlan" {
  value = data.k0rdent_network.training.vlan_id
}

# Warn if a cluster made this network, since then it can't be managed.
check "network_is_ours_to_manage" {
  assert {
    condition     = data.k0rdent_network.training.cluster_id == null
    error_message = "training-fabric was provisioned by cluster ${data.k0rdent_network.training.cluster_id} and cannot be managed through the provider."
  }
}
