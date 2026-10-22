# The same address with each part named. Leave out region and project to
# use the provider's.
import {
  to = k0rdent_security_group.example
  identity = {
    region  = "us-texas-1"
    project = "cp-production"
    id      = "office-access"
  }
}
