# Takes every id form `terraform import` does. The first command below uses
# the provider's region and project, the second names them. If you name them,
# set both in the resource block too, or the next plan replaces the cluster.
#
# The import adopts every node pool. If node_pool names only some, the next
# plan shows an in-place update that changes nothing on the platform.
import {
  to = k0rdent_kubernetes_cluster.example
  id = "us-texas-1/cp-production/my-cluster"
}
