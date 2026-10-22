# Takes every id form `terraform import` does. The first command below uses
# the provider's region and project, the second names them. If you name them,
# set both in the resource block too, or the next plan replaces the group.
import {
  to = k0rdent_instance_group.example
  id = "us-texas-1/cp-production/my-hosts"
}
