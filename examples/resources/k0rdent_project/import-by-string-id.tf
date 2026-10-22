# Takes every id form `terraform import` does. The first command below gives
# only the project id, which fails if two organizations use it. The second
# names the organization too.
import {
  to = k0rdent_project.production
  id = "org-ml-platform/cp-production"
}
