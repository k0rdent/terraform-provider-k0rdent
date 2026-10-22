# Every cluster in the provider's project. `terraform query
# -generate-config-out=clusters.tf` writes an import block and a resource
# block for each one.
list "k0rdent_kubernetes_cluster" "all" {
  provider = k0rdent
}

# The clusters in another project, with each one's full attributes shown.
list "k0rdent_kubernetes_cluster" "staging" {
  provider         = k0rdent
  include_resource = true

  config {
    project = "cp-staging"
  }
}
