# Takes every id form `terraform import` does. The first command below uses
# the provider's region and project, the second names them.
import {
  to = k0rdent_security_group.example
  id = "us-texas-1/cp-production/office-access"
}
