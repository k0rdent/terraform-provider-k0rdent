data "k0rdent_cluster_types" "all" {}

# Available types with "gpu" in the name. `(?i)` makes the match
# case-insensitive. The order isn't fixed, so look entries up by id.
data "k0rdent_cluster_types" "gpu" {
  status             = "available"
  display_name_regex = "(?i)gpu"
}

output "gpu_cluster_types" {
  value = { for t in data.k0rdent_cluster_types.gpu.cluster_types : t.id => t.self_link }
}
