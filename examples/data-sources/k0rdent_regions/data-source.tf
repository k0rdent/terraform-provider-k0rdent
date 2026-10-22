# The regions you can use.
data "k0rdent_regions" "all" {}

output "region_ids" {
  value = data.k0rdent_regions.all.ids
}
