# Read only: lists what this credential can see, and what it can build.
# Nothing is created, so `terraform apply` reports 0 added, 0 changed,
# 0 destroyed.

terraform {
  required_providers {
    k0rdent = {
      source = "k0rdent/k0rdent"
    }
  }
}

# The endpoint and the credential come from K0RDENT_* in the environment.
# See setup.sh.
provider "k0rdent" {}

data "k0rdent_regions" "all" {}
data "k0rdent_organizations" "all" {}
data "k0rdent_projects" "all" {}

# The catalog is per region, so read it once for each region id.
data "k0rdent_cluster_types" "available" {
  for_each = toset([for r in data.k0rdent_regions.all.regions : r.id])

  region = each.key
  status = "available"
}

output "regions" {
  value = [for r in data.k0rdent_regions.all.regions : "${r.id} (${r.type})"]
}

output "organizations" {
  value = [for o in data.k0rdent_organizations.all.organizations : "${o.id} (${o.status})"]
}

output "projects" {
  value = [for p in data.k0rdent_projects.all.projects : "${p.id} in ${p.organization_id}, region ${coalesce(p.region, "none")} (${p.status})"]
}

# Cluster type ids by region. An empty list means nothing to build there yet.
output "cluster_types" {
  value = {
    for region, catalog in data.k0rdent_cluster_types.available :
    region => [for ct in catalog.cluster_types : ct.id]
  }
}
