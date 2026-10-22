# Takes every id form `terraform import` does. The first command below uses
# the provider's region and project. The second names them, for a store
# outside the provider's region and project.
import {
  to = k0rdent_file_store.datasets
  id = "us-texas-1/cp-production/training-datasets"
}
