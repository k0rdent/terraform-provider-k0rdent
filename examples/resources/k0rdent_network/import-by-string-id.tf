# Takes every id form `terraform import` does. The first command below uses
# the provider's region and project. The second names them, for a network
# outside the provider's region and project.
#
# With vlan_id = "auto", the plan after an import shows an in-place update
# that changes nothing on the platform. An InfiniBand pkey is the same.
import {
  to = k0rdent_network.example
  id = "us-texas-1/cp-production/training-fabric"
}
