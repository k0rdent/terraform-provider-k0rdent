# Read only: lists what this credential can see, and what it can build.
# Nothing is created, so `terraform apply` reports 0 added, 0 changed,
# 0 destroyed.
#
# It also reads up to three entries of each list by id, as another
# configuration would, so a read this credential can't make fails here. Those
# reads are in the state, not the outputs: `terraform state list`.

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

# A credential scoped to one project can't read its organization. With one,
# run `terraform apply -var read_organization=false`.
variable "read_organization" {
  type        = bool
  default     = true
  description = "Read the organization's roles, quotas and webhooks."
}

data "k0rdent_regions" "all" {}
data "k0rdent_organizations" "all" {}
data "k0rdent_projects" "all" {}

locals {
  regions  = toset([for r in data.k0rdent_regions.all.regions : r.id])
  projects = data.k0rdent_projects.all.projects
  orgs     = data.k0rdent_organizations.all.organizations

  # The project and organization the reads below look in: the first active
  # project that has a region, and its organization. With no project, the
  # first organization. The reads for each are skipped when there's none.
  project = try(
    [for p in local.projects : p if p.region != null && p.status == "active"][0],
    [for p in local.projects : p if p.region != null][0],
    local.projects[0],
    null,
  )
  organization = var.read_organization ? try(
    local.project == null ? local.orgs[0] : [for o in local.orgs : o if o.id == local.project.organization_id][0],
    null,
  ) : null
  # A project made before regions were required has none. Read it in the first
  # region that hosts projects.
  region     = try(coalesce(local.project.region, [for r in data.k0rdent_regions.all.regions : r.id if r.hosts_projects]...), null)
  in_org     = local.organization == null ? 0 : 1
  in_project = local.project == null || local.region == null ? 0 : 1
}

data "k0rdent_webhook_subscriptions" "all" {
  count = local.in_org
}

# The catalog is per region, so read it once for each region id.
data "k0rdent_cluster_types" "available" {
  for_each = local.regions

  region = each.key
  status = "available"
}

data "k0rdent_address_pools" "all" {
  for_each = local.regions
  region   = each.key
}

data "k0rdent_share_types" "all" {
  for_each = local.regions
  region   = each.key
}

data "k0rdent_organization" "this" {
  count = local.in_org
  id    = local.organization.id
}

data "k0rdent_roles" "all" {
  count           = local.in_org
  organization_id = local.organization.id
}

data "k0rdent_tenant_quota" "this" {
  count           = local.in_org
  organization_id = local.organization.id
}

data "k0rdent_project" "this" {
  count           = local.in_project
  id              = local.project.id
  organization_id = local.project.organization_id
}

data "k0rdent_project_quota" "this" {
  count           = local.in_project * local.in_org
  organization_id = local.project.organization_id
  project_id      = local.project.id
}

data "k0rdent_instances" "all" {
  count   = local.in_project
  region  = local.region
  project = local.project.id
}

data "k0rdent_networks" "all" {
  count   = local.in_project
  region  = local.region
  project = local.project.id
}

data "k0rdent_security_groups" "all" {
  count   = local.in_project
  region  = local.region
  project = local.project.id
}

data "k0rdent_vpcs" "all" {
  count   = local.in_project
  region  = local.region
  project = local.project.id
}

locals {
  # Up to three entries of each list, for the reads by id, so a big account
  # stays fast. The catalog's are keyed region/id.
  catalog = {
    for kind, by_region in {
      cluster_types = { for r, d in data.k0rdent_cluster_types.available : r => [for t in d.cluster_types : t.id] }
      address_pools = { for r, d in data.k0rdent_address_pools.all : r => d.ids }
      share_types   = { for r, d in data.k0rdent_share_types.all : r => d.ids }
    } :
    kind => merge({}, [
      for r, ids in by_region : { for id in slice(ids, 0, min(3, length(ids))) : "${r}/${id}" => { region = r, id = id } }
    ]...)
  }

  first = {
    for kind, ids in {
      subscriptions   = [for s in flatten(data.k0rdent_webhook_subscriptions.all[*].subscriptions) : s.id]
      instances       = flatten(data.k0rdent_instances.all[*].ids)
      instance_groups = distinct([for i in flatten(data.k0rdent_instances.all[*].instances) : i.instance_group if i.instance_group != null])
      networks        = flatten(data.k0rdent_networks.all[*].ids)
      security_groups = flatten(data.k0rdent_security_groups.all[*].ids)
      vpcs            = flatten(data.k0rdent_vpcs.all[*].ids)
    } :
    kind => toset(slice(ids, 0, min(3, length(ids))))
  }
}

data "k0rdent_cluster_type" "each" {
  for_each = local.catalog.cluster_types
  region   = each.value.region
  id       = each.value.id
}

data "k0rdent_cluster_type_versions" "each" {
  for_each     = local.catalog.cluster_types
  region       = each.value.region
  cluster_type = each.value.id
  status       = "available"
}

# The newest available version of each. The list is oldest first.
data "k0rdent_cluster_type_version" "newest" {
  for_each = { for k, v in data.k0rdent_cluster_type_versions.each : k => reverse(v.versions)[0].version_id if length(v.versions) > 0 }

  region       = local.catalog.cluster_types[each.key].region
  cluster_type = local.catalog.cluster_types[each.key].id
  version_id   = each.value
}

data "k0rdent_address_pool" "each" {
  for_each = local.catalog.address_pools
  region   = each.value.region
  id       = each.value.id
}

data "k0rdent_share_type" "each" {
  for_each = local.catalog.share_types
  region   = each.value.region
  id       = each.value.id
}

data "k0rdent_webhook_subscription" "each" {
  for_each = local.first.subscriptions
  id       = each.key
}

data "k0rdent_webhook_deliveries" "each" {
  for_each        = local.first.subscriptions
  subscription_id = each.key
  limit           = 5
}

data "k0rdent_instance" "each" {
  for_each = local.first.instances
  region   = local.region
  project  = local.project.id
  id       = each.key
}

data "k0rdent_instance_group" "each" {
  for_each = local.first.instance_groups
  region   = local.region
  project  = local.project.id
  id       = each.key
}

data "k0rdent_network" "each" {
  for_each = local.first.networks
  region   = local.region
  project  = local.project.id
  id       = each.key
}

data "k0rdent_subnets" "each" {
  for_each = local.first.networks
  region   = local.region
  project  = local.project.id
  network  = each.key
}

data "k0rdent_security_group" "each" {
  for_each = local.first.security_groups
  region   = local.region
  project  = local.project.id
  id       = each.key
}

data "k0rdent_vpc" "each" {
  for_each = local.first.vpcs
  region   = local.region
  project  = local.project.id
  id       = each.key
}

data "k0rdent_vpc_peerings" "each" {
  for_each = local.first.vpcs
  region   = local.region
  project  = local.project.id
  vpc      = each.key
}

data "k0rdent_effective_security_groups" "each" {
  for_each    = local.first.vpcs
  region      = local.region
  project     = local.project.id
  object_kind = "vpc"
  object_id   = each.key
}

locals {
  # The same for the lists read per network and per VPC, keyed network/id
  # and vpc/id.
  subnets = merge({}, [
    for n, d in data.k0rdent_subnets.each : { for s in slice(d.subnets, 0, min(3, length(d.subnets))) : "${n}/${s.id}" => { network = n, id = s.id } }
  ]...)
  peerings = merge({}, [
    for v, d in data.k0rdent_vpc_peerings.each : { for p in slice(d.peerings, 0, min(3, length(d.peerings))) : "${v}/${p.id}" => { vpc = v, id = p.id } }
  ]...)
}

data "k0rdent_subnet" "each" {
  for_each = local.subnets
  region   = local.region
  project  = local.project.id
  network  = each.value.network
  id       = each.value.id
}

data "k0rdent_vpc_peering" "each" {
  for_each = local.peerings
  region   = local.region
  project  = local.project.id
  vpc      = each.value.vpc
  id       = each.value.id
}

locals {
  # Every list the outputs show, keyed by name, or by name/key for the ones
  # in a map.
  lists = merge(
    { for r, d in data.k0rdent_address_pools.all : "address_pools/${r}" => [for p in d.address_pools : "${p.id} (${coalesce(p.state, "unknown")}${p.routable == true ? ", routable" : ""})"] },
    { for r, d in data.k0rdent_share_types.all : "share_types/${r}" => [for s in d.share_types : "${s.id} (${coalesce(s.state, "unknown")})"] },
    { for n, d in data.k0rdent_subnets.each : "subnets/${n}" => [for s in d.subnets : "${s.id} ${coalesce(s.cidr, "(no CIDR yet)")}"] },
    { for v, d in data.k0rdent_vpc_peerings.each : "peerings/${v}" => [for p in d.peerings : "${p.id} (${coalesce(p.state, "unknown")})"] },
    {
      roles           = flatten(data.k0rdent_roles.all[*].ids)
      instances       = [for i in flatten(data.k0rdent_instances.all[*].instances) : "${i.id} (${coalesce(i.state, "unknown")})"]
      networks        = [for n in flatten(data.k0rdent_networks.all[*].networks) : "${n.id} (${coalesce(n.type, "unknown")}, ${coalesce(n.state, "unknown")})"]
      security_groups = [for g in flatten(data.k0rdent_security_groups.all[*].security_groups) : "${g.id} (${coalesce(g.rule_count, "?")} rules)"]
      vpcs            = [for v in flatten(data.k0rdent_vpcs.all[*].vpcs) : "${v.id} (${coalesce(v.state, "unknown")})"]
    },
  )
  deliveries = { for id, d in data.k0rdent_webhook_deliveries.each : id => d.deliveries[*].status if length(d.deliveries) > 0 }

  # The first ten of each, then how many more.
  ten = { for k, l in local.lists : k => concat(slice(l, 0, min(10, length(l))), length(l) > 10 ? ["and ${length(l) - 10} more"] : []) }

  # How much more fits, by quota dimension.
  quota = {
    for scope, q in {
      organization = one(data.k0rdent_tenant_quota.this[*].dimensions)
      project      = one(data.k0rdent_project_quota.this[*].dimensions)
    } :
    scope => q == null ? null : {
      for name, d in q : name => d.enforced != true ? "not enforced" : d.headroom == null ? "no limit" : "${d.headroom}${d.unit == null || d.unit == "count" ? "" : " ${d.unit}"} left"
    }
  }
}

output "regions" {
  value = [for r in data.k0rdent_regions.all.regions : "${r.id} (${coalesce(r.type, "unknown")})"]
}

output "organizations" {
  value = [for o in data.k0rdent_organizations.all.organizations : "${o.id} (${coalesce(o.status, "unknown")})"]
}

output "projects" {
  value = [for p in data.k0rdent_projects.all.projects : "${p.id} in ${p.organization_id}, region ${coalesce(p.region, "none")} (${coalesce(p.status, "unknown")})"]
}

# Cluster type ids by region. An empty list means nothing to build there yet.
output "cluster_types" {
  value = {
    for region, catalog in data.k0rdent_cluster_types.available :
    region => [for ct in catalog.cluster_types : ct.id]
  }
}

# The pools a subnet's addresses come from, by region.
output "address_pools" {
  value = { for r in local.regions : r => local.ten["address_pools/${r}"] }
}

# The classes a file store is made from, by region.
output "share_types" {
  value = { for r in local.regions : r => local.ten["share_types/${r}"] }
}

output "in_organization" {
  value = local.in_org == 0 ? null : {
    id    = local.organization.id
    roles = local.ten.roles
    quota = local.quota.organization
  }
}

output "in_project" {
  value = local.in_project == 0 ? null : {
    id              = "${local.project.id} in ${local.region}"
    instances       = local.ten.instances
    networks        = local.ten.networks
    security_groups = local.ten.security_groups
    vpcs            = local.ten.vpcs
    # By network and by VPC, for the first three of each.
    subnets  = { for n in keys(data.k0rdent_subnets.each) : n => local.ten["subnets/${n}"] }
    peerings = { for v in keys(data.k0rdent_vpc_peerings.each) : v => local.ten["peerings/${v}"] }
    quota    = local.quota.project
  }
}

# Each subscription's state, and its last few deliveries for the first three.
output "webhooks" {
  value = {
    for s in flatten(data.k0rdent_webhook_subscriptions.all[*].subscriptions) :
    s.id => "${s.is_active == null ? "unknown" : s.is_active ? "active" : "inactive"}${try(", last deliveries: ${join(", ", local.deliveries[s.id])}", "")}"
  }
}
