# Takes every id form `terraform import` does. The network is part of the
# address. The first command below names the network, the second names the
# scope too.
#
# On a pool that isn't routable, the plan after an import may show an in-place
# update to allocate_gateway that changes nothing on the platform.
import {
  to = k0rdent_subnet.example
  id = "us-texas-1/cp-production/training-fabric/pods"
}
