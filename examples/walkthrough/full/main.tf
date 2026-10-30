# Everything a tenant can build, in one apply. Inputs are in variables.tf,
# outputs in outputs.tf.
#
# By default: a network with subnets, a security group, a cluster filtered by
# that group with your users on its nodes, and a webhook. Bare hosts, a file
# store, VPC peering and a project of its own are off until you turn them on
# in terraform.tfvars. The scorecard output says what ran.
#
# Not here:
# - The k0rdent_cluster_kubeconfig ephemeral resource. nodes/ uses it.
# - The k0rdent_cluster_cancel_operation action. You run it by hand.
# - List resources. They go in a .tfquery.hcl file for terraform query.
# - Data sources whose answer the resources here already hold, and the ones
#   look/ uses.
# - Service accounts. Only a person can create them.

terraform {
  required_version = ">= 1.11"

  required_providers {
    k0rdent = {
      source = "k0rdent/k0rdent"
    }
  }
}

# The endpoint and the credential come from K0RDENT_* in the environment.
# See setup.sh. Every resource that takes tags gets these.
provider "k0rdent" {
  region       = var.region
  default_tags = local.tags
}

# ---------------------------------------------------------------------------
# Names, tags and descriptions, from prefix and environment
# ---------------------------------------------------------------------------

locals {
  # The cluster keeps this name, so nodes/ finds it as <prefix>-walkthrough.
  name         = "${var.prefix}-walkthrough"
  display_name = "${var.prefix} walkthrough"
  description  = "${var.environment} walkthrough for ${var.prefix}, from examples/walkthrough/full. Safe to delete."

  tags = {
    app         = "walkthrough"
    environment = var.environment
    owner       = var.prefix
    managed_by  = "terraform"
  }

  # In prod, a destroy waits for a running operation instead of cancelling it.
  on_destroy_busy = var.environment == "prod" ? "wait" : "cancel"

  # One entry per user in var.ssh_users, with that user's keys, or your key
  # if it names none. Each key file is read and trimmed.
  ssh_access = [
    for username, u in var.ssh_users : {
      username        = username
      ssh_public_keys = [for f in(length(u.public_key_files) > 0 ? u.public_key_files : [var.ssh_public_key_file]) : trimspace(file(pathexpand(f)))]
      sudo            = u.sudo ? "ALL=(ALL) NOPASSWD:ALL" : null
    }
  ]
}

# ---------------------------------------------------------------------------
# Where it builds
# ---------------------------------------------------------------------------

# With build.own_project, a project of its own, deleted last on destroy. That
# needs organizations-admin on the organization: organizations-member can't
# delete a project.
resource "k0rdent_project" "own" {
  # count = 1 or 0 turns the resource on or off.
  count = var.build.own_project ? 1 : 0

  id              = local.name
  organization_id = var.organization
  region          = var.region
  display_name    = local.display_name
  description     = local.description
}

# Otherwise one you already have, read first so a wrong id stops the plan.
data "k0rdent_project" "existing" {
  count = var.build.own_project ? 0 : 1

  id = var.project

  lifecycle {
    # Fail the plan if nothing can be built in the project.
    postcondition {
      condition     = !contains(["suspended", "disabled", "deleting"], coalesce(self.status, "active"))
      error_message = "Project ${var.project} is ${coalesce(self.status, "in no status")}, so nothing can be built in it."
    }
  }
}

# The project in use, whichever of the two above is on. [0] is the one
# instance when count is 1.
locals {
  project      = var.build.own_project ? k0rdent_project.own[0].id : data.k0rdent_project.existing[0].id
  organization = var.build.own_project ? var.organization : data.k0rdent_project.existing[0].organization_id
}

# ---------------------------------------------------------------------------
# Networking
# ---------------------------------------------------------------------------

data "k0rdent_address_pools" "active" {
  count = var.build.network ? 1 : 0

  state = "active"
}

locals {
  # Routable pools are kept for cluster endpoints and load balancers, so use
  # address_pool if set, else the first active pool that isn't routable.
  unroutable_pools = var.build.network ? [for p in data.k0rdent_address_pools.active[0].address_pools : p if !p.routable] : []
  pool_id          = var.address_pool != "" ? var.address_pool : try(local.unroutable_pools[0].id, "")
}

data "k0rdent_address_pool" "chosen" {
  count = var.build.network && local.pool_id != "" ? 1 : 0

  id = local.pool_id

  # Fail the plan if the chosen pool can't give out subnets.
  lifecycle {
    postcondition {
      condition     = !self.routable
      error_message = "Address pool ${self.id} is routable. It holds clusters' endpoint and load balancer addresses, so subnets don't come from it. Set address_pool to one that isn't, or leave it empty."
    }

    postcondition {
      condition     = self.state == "active"
      error_message = "Address pool ${self.id} is ${coalesce(self.state, "in no state")}, and only an active pool gives out subnets."
    }
  }
}

locals {
  # The pool, or null if none was read. [*] makes a list of the 0 or 1
  # instances, and one() turns that into the item or null.
  pool = one(data.k0rdent_address_pool.chosen[*])

  # Each subnet's prefix length, kept within what the pool allows.
  subnet_prefix_lengths = {
    for purpose, want in var.subnets :
    purpose => local.pool == null ? want : min(max(want, local.pool.min_prefix_length), local.pool.max_prefix_length)
  }
}

resource "k0rdent_network" "this" {
  count = var.build.network ? 1 : 0

  id           = local.name
  project      = local.project
  display_name = local.display_name
  description  = local.description

  # An Ethernet fabric. "auto" lets the platform pick the VLAN.
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
  }
}

# One subnet per entry in var.subnets. The platform picks each CIDR from the
# pool. Adding an entry leaves the others alone.
resource "k0rdent_subnet" "this" {
  for_each = var.build.network ? var.subnets : {}

  id            = "${local.name}-${each.key}"
  project       = local.project
  network       = k0rdent_network.this[0].id
  display_name  = "${local.display_name} ${each.key}"
  description   = "${each.key} subnet. ${local.description}"
  address_pool  = try(local.pool.self_link, null)
  prefix_length = local.subnet_prefix_lengths[each.key]

  # No gateway: only a routable pool gives one.

  lifecycle {
    # Fail the plan if there's no pool to use.
    precondition {
      condition     = local.pool != null
      error_message = "Region ${var.region} has no active address pool that isn't routable, so no subnet can be made. Set address_pool to one, set build.network = false, or ask whoever runs the landscape for one."
    }

    # Fail if the CIDR the platform gave isn't inside one of the pool's
    # prefixes. It compares the network address at each prefix's length.
    postcondition {
      condition = self.cidr != "" && anytrue([
        for p in local.pool.prefixes : try(cidrsubnet("${split("/", self.cidr)[0]}/${split("/", p)[1]}", 0, 0) == cidrsubnet(p, 0, 0), false)
      ])
      error_message = "Subnet ${self.id} came back with ${coalesce(self.cidr, "no block")}, which isn't inside pool ${local.pool.id} (${join(", ", local.pool.prefixes)})."
    }
  }
}

# Rules match first to last, so the order is fixed: services by name, then
# ranges as listed.
locals {
  # One permit rule per service and range. The nested for makes a list of
  # lists, and flatten() makes it one list.
  ingress_rules = flatten([
    for service, s in var.ingress_services : [
      for cidr in var.allowed_cidrs : {
        name     = "${service} from ${cidr}"
        protocol = s.protocol
        port     = s.port
        source   = cidr
      }
    ]
  ])

  # One deny per service, after the permits. The nico fabric denies what no
  # rule permits, but the VPC's default group is checked after this group and
  # permits traffic between the VPC's subnets, so these keep each service
  # closed between nodes too. Not 6443, so nodes can still reach each other's
  # Kubernetes API.
  ingress_denies = [
    for service, s in var.ingress_services : {
      name     = "${service} from anywhere else"
      protocol = s.protocol
      port     = s.port
    } if s.port != "6443"
  ]

  # What nodes reach out to. Each rule names a destination port and no source
  # port, the only shape whose replies the nico fabric lets back in, so no
  # "any" rule.
  egress_out = [
    { name = "dns", protocol = "udp", port = "53" },
    { name = "dns over tcp", protocol = "tcp", port = "53" },
    { name = "ntp", protocol = "udp", port = "123" },
    { name = "http", protocol = "tcp", port = "80" },
    { name = "https", protocol = "tcp", port = "443" },
    { name = "kubernetes api", protocol = "tcp", port = "6443" },
    { name = "konnectivity", protocol = "tcp", port = "8132" },
    { name = "nfs", protocol = "tcp", port = "2049" },
  ]

  # Ingress rules are stateless, so each service's replies need a rule from
  # its own port.
  egress_replies = [
    for service, s in var.ingress_services : {
      name        = "${service} replies"
      protocol    = s.protocol
      source_port = s.port
    }
  ]

  egress_rules = concat(local.egress_out, local.egress_replies)
}

# Filters node interfaces on a nico VPC. It doesn't filter the cluster's
# Kubernetes API address.
resource "k0rdent_security_group" "this" {
  count = var.build.security_group ? 1 : 0

  id           = local.name
  project      = local.project
  display_name = local.display_name
  description  = "${join(", ", keys(var.ingress_services))} from ${join(", ", var.allowed_cidrs)} only. ${local.description}"

  # One ingress block per entry in local.ingress_rules. `ingress.value` is
  # the current entry.
  dynamic "ingress" {
    for_each = local.ingress_rules

    content {
      name                   = ingress.value.name
      action                 = "permit"
      protocol               = ingress.value.protocol
      source_prefix          = ingress.value.source
      destination_prefix     = "0.0.0.0/0"
      destination_port_range = ingress.value.port
    }
  }

  # After the permits. local.ingress_denies says what they're for.
  dynamic "ingress" {
    for_each = local.ingress_denies

    content {
      name                   = ingress.value.name
      action                 = "deny"
      protocol               = ingress.value.protocol
      source_prefix          = "0.0.0.0/0"
      destination_prefix     = "0.0.0.0/0"
      destination_port_range = ingress.value.port
    }
  }

  dynamic "egress" {
    for_each = local.egress_rules

    content {
      name                   = egress.value.name
      action                 = "permit"
      protocol               = egress.value.protocol
      source_prefix          = "0.0.0.0/0"
      destination_prefix     = "0.0.0.0/0"
      source_port_range      = try(egress.value.source_port, null)
      destination_port_range = try(egress.value.port, null)
    }
  }
}

# ---------------------------------------------------------------------------
# The cluster
# ---------------------------------------------------------------------------

data "k0rdent_cluster_types" "available" {
  status = "available"
}

data "k0rdent_cluster_type" "chosen" {
  id = var.cluster_type
}

# The available versions, listed in the outputs.
data "k0rdent_cluster_type_versions" "chosen" {
  cluster_type = var.cluster_type
  status       = "available"
}

# With cluster_type_version set, that version. A wrong id or an unavailable
# version fails the plan.
data "k0rdent_cluster_type_version" "pinned" {
  count = var.cluster_type_version != "" ? 1 : 0

  cluster_type = var.cluster_type
  version_id   = var.cluster_type_version

  lifecycle {
    postcondition {
      condition     = self.status == "available"
      error_message = "Version ${var.cluster_type_version} of ${var.cluster_type} is ${coalesce(self.status, "in no status")}. Only an available version backs a new cluster."
    }
  }
}

# The pinned version if one is set, else the cluster type's newest.
locals {
  cluster_type_ref = var.cluster_type_version != "" ? data.k0rdent_cluster_type_version.pinned[0].self_link : data.k0rdent_cluster_type.chosen.self_link
  node_pools       = var.cluster_type_version != "" ? data.k0rdent_cluster_type_version.pinned[0].node_pools : data.k0rdent_cluster_type.chosen.node_pools

  # The pools you can resize. Control-plane pools can't be.
  worker_pools = [for p in local.node_pools : p if p.role == "worker"]
  # Each pool's node count, keyed by pool id: from var.worker_counts, else
  # the pool's minimum.
  worker_counts = {
    for p in local.worker_pools : p.id => lookup(var.worker_counts, p.id, coalesce(p.node_count_min, 0))
  }
  # The pools to set: the first always, the others only if their count is
  # above 0.
  cluster_node_pool = {
    for i, p in local.worker_pools : p.id => { node_count = local.worker_counts[p.id] }
    if i == 0 || local.worker_counts[p.id] > 0
  }
}

resource "k0rdent_kubernetes_cluster" "this" {
  id           = local.name
  project      = local.project
  cluster_type = local.cluster_type_ref
  display_name = local.display_name
  description  = local.description

  node_pool = local.cluster_node_pool

  # Null leaves the cluster's groups alone. A list attaches exactly those.
  security_groups = var.build.security_group ? [k0rdent_security_group.this[0].id] : null

  # On every node. Set when nodes are built, so a change later replaces the
  # cluster.
  ssh_access = local.ssh_access

  on_destroy_busy = local.on_destroy_busy

  # A change that would replace the cluster fails the plan instead. Set true
  # only if you mean it.
  allow_replace = false

  # Building or freeing hardware can take more than 2 hours.
  timeouts {
    create = "3h"
    update = "3h"
    delete = "3h"
  }

  # Fail the plan on a cluster type, pool name or worker count that can't
  # work. [*].id is the list of every entry's id.
  lifecycle {
    precondition {
      condition     = contains(data.k0rdent_cluster_types.available.cluster_types[*].id, var.cluster_type)
      error_message = "Cluster type ${var.cluster_type} isn't available in region ${var.region}. Run look again for the ones that are."
    }

    precondition {
      condition     = length(local.worker_pools) > 0
      error_message = "Cluster type ${var.cluster_type} has no worker pool to put nodes in."
    }

    precondition {
      condition     = alltrue([for id in keys(var.worker_counts) : contains(local.worker_pools[*].id, id)])
      error_message = "worker_counts names a pool ${var.cluster_type} doesn't have. Its worker pools are ${join(", ", local.worker_pools[*].id)}."
    }

    precondition {
      condition = alltrue([
        for p in local.worker_pools :
        local.worker_counts[p.id] >= coalesce(p.node_count_min, 0) && local.worker_counts[p.id] <= coalesce(p.node_count_max, local.worker_counts[p.id])
      ])
      error_message = "A worker count is outside its pool's range: ${join(", ", [for p in local.worker_pools : "${p.id} takes ${coalesce(p.node_count_min, 0)} to ${p.node_count_max == null ? "any number" : p.node_count_max}"])}."
    }

    # Fail the apply if the cluster didn't get every SSH user.
    postcondition {
      condition     = length(self.ssh_access) == length(local.ssh_access)
      error_message = "The cluster came back with ${length(self.ssh_access)} SSH users, not the ${length(local.ssh_access)} asked for."
    }
  }
}

# ---------------------------------------------------------------------------
# VPCs: the group on each one this makes, and peering between them
# ---------------------------------------------------------------------------

locals {
  # Every VPC this makes, by what made it: the cluster's, and the instance
  # group's if it's on. A VPC can't be created on its own.
  owners = merge(
    {
      cluster = {
        kind = "cluster"
        id   = k0rdent_kubernetes_cluster.this.id
        vpc  = k0rdent_kubernetes_cluster.this.vpc
      }
    },
    var.build.instance_group ? {
      hosts = {
        kind = "instance_group"
        id   = k0rdent_instance_group.this[0].id
        vpc  = k0rdent_instance_group.this[0].vpc
      }
    } : {},
  )
}

# Each VPC, read for its backend, since only a nico VPC takes security groups.
data "k0rdent_vpc" "owned" {
  for_each = local.owners

  id      = each.value.vpc
  project = local.project
}

# Each VPC's default group, which the platform makes with the VPC. Its id is
# random, so it's found by the name the platform gives it, "Default (<VPC
# id>)", and by owner_kind, which only the platform's groups carry. The VPC's
# own security_groups can't be used: it lists the binding, which this
# changes.
data "k0rdent_security_groups" "vpc_default" {
  for_each = var.build.vpc_security_groups ? local.owners : {}

  project            = local.project
  display_name_regex = "^Default \\(${each.value.vpc}\\)$"
}

locals {
  vpc_default_group = {
    for owner, d in data.k0rdent_security_groups.vpc_default :
    owner => [for g in d.security_groups : g.id if g.owner_kind == "vpc"]
  }
}

# The same group on every VPC this makes, ahead of the VPC's default group.
# An instance group has no security_groups of its own, so this is what
# filters the hosts. A list without the default would replace it, and with
# it the default's rules for traffic between the VPC's subnets.
resource "k0rdent_vpc_security_groups" "this" {
  for_each = var.build.vpc_security_groups ? local.owners : {}

  vpc             = each.value.vpc
  project         = local.project
  # Two entries, so a first plan shows ours first even before the default's id
  # is known. The precondition below stops anything but one default.
  security_groups = [k0rdent_security_group.this[0].id, try(one(local.vpc_default_group[each.key]), null)]

  lifecycle {
    # Fail the plan if the VPC can't take security groups.
    precondition {
      condition     = data.k0rdent_vpc.owned[each.key].backend == "nico"
      error_message = "The ${each.key} VPC, ${each.value.vpc}, is on ${coalesce(data.k0rdent_vpc.owned[each.key].backend, "no backend")}, and only a nico VPC takes security groups. Set build.vpc_security_groups = false."
    }

    # Fail rather than drop the default group from the VPC.
    precondition {
      condition     = data.k0rdent_vpc.owned[each.key].backend != "nico" || length(local.vpc_default_group[each.key]) == 1
      error_message = "Found ${length(local.vpc_default_group[each.key])} default groups for the ${each.key} VPC, ${each.value.vpc}, not 1. Either the VPC has none, as from API v1.3.0-rc.4 a VPC with no subnet prefixes or more than 16 doesn't, or no single group with owner_kind = \"vpc\" is named \"Default (${each.value.vpc})\". Without one, this binding would replace the VPC's rules for traffic between its subnets, and on a VPC with none it couldn't be taken off again. Set build.vpc_security_groups = false, or check the VPC's groups with the k0rdent_security_groups data source."
    }
  }
}

# Every group that applies to each owner, from all its bindings.
data "k0rdent_effective_security_groups" "owned" {
  for_each = var.build.security_group ? local.owners : {}

  object_kind = each.value.kind
  object_id   = each.value.id
  project     = local.project

  # Read after the bindings are made. See terraform_data.made below.
  depends_on = [k0rdent_vpc_security_groups.this, terraform_data.made]
}

# A peering needs one on each side, so both are here: the cluster's VPC to the
# hosts', and back. Both VPCs are in this project, so remote_vpc takes the id
# from the owner, which stays known when the cluster changes.
locals {
  peerings = var.build.vpc_peering ? {
    cluster-to-hosts = { id = "${local.name}-to-hosts", from = "cluster", to = "hosts" }
    hosts-to-cluster = { id = "${local.name}-to-cluster", from = "hosts", to = "cluster" }
  } : {}
}

resource "k0rdent_vpc_peering" "this" {
  for_each = local.peerings

  id           = each.value.id
  project      = local.project
  vpc          = local.owners[each.value.from].vpc
  remote_vpc   = local.owners[each.value.to].vpc
  display_name = "${local.display_name} ${each.key}"
}

# Moves a binding made by an earlier version of this directory to its new
# address, so it isn't removed and made again.
moved {
  from = k0rdent_vpc_security_groups.cluster[0]
  to   = k0rdent_vpc_security_groups.this["cluster"]
}

# ---------------------------------------------------------------------------
# Events and automation
# ---------------------------------------------------------------------------

# Cluster events, signed with the secret from TF_VAR_webhook_secret. Raise
# webhook_secret_version in terraform.tfvars to rotate it. The old secret
# keeps working for a day.
resource "k0rdent_webhook_subscription" "this" {
  count = var.build.webhook ? 1 : 0

  url         = var.webhook_url
  event_types = var.webhook_events

  secret_wo         = var.webhook_secret
  secret_wo_version = var.webhook_secret_version
  rotation_overlap  = true
}

# ---------------------------------------------------------------------------
# Hosts and storage. Off by default.
# ---------------------------------------------------------------------------

# Bare hosts from the cluster type's first worker pool, at least one, with
# the same users. This takes real hardware.
resource "k0rdent_instance_group" "this" {
  count = var.build.instance_group ? 1 : 0

  id           = "${local.name}-hosts"
  project      = local.project
  cluster_type = local.cluster_type_ref
  display_name = "${local.display_name} hosts"
  description  = local.description

  node_pool = {
    (local.worker_pools[0].id) = {
      size = max(1, coalesce(local.worker_pools[0].node_count_min, 0))
    }
  }

  ssh_access = local.ssh_access

  timeouts {
    create = "3h"
    update = "3h"
    delete = "3h"
  }
}

data "k0rdent_instance_group" "hosts" {
  count = var.build.instance_group ? 1 : 0

  id      = k0rdent_instance_group.this[0].id
  project = local.project
}

data "k0rdent_instances" "hosts" {
  count = var.build.instance_group ? 1 : 0

  project        = local.project
  instance_group = k0rdent_instance_group.this[0].id
}

# A shared file store from file_store.share_type, or the first active one.
data "k0rdent_share_types" "active" {
  count = var.build.file_store ? 1 : 0

  state = "active"
}

data "k0rdent_share_type" "chosen" {
  count = var.build.file_store ? 1 : 0

  # file_store.share_type if set, else the first active one. try() gives ""
  # when there's none, and the read then fails with a clear error.
  id = var.file_store.share_type != "" ? var.file_store.share_type : try(data.k0rdent_share_types.active[0].ids[0], "")

  lifecycle {
    postcondition {
      condition     = self.state == "active"
      error_message = "Share type ${self.id} is ${coalesce(self.state, "in no state")}, and only an active one gives out storage."
    }
  }
}

# With an instance group, every host in it may mount the store.
resource "k0rdent_file_store" "this" {
  count = var.build.file_store ? 1 : 0

  id           = local.name
  project      = local.project
  display_name = local.display_name
  description  = local.description
  share_type   = data.k0rdent_share_type.chosen[0].id
  capacity_gib = var.file_store.capacity_gib

  # Only with an instance group. null leaves consumers unset.
  consumers = var.build.instance_group ? {
    (k0rdent_instance_group.this[0].id) = {
      type       = "instanceGroup"
      mount_path = var.file_store.mount_path
      network    = { type = "ethernet" }
    }
  } : null

  # Fail the plan if the store is bigger than the share type allows.
  lifecycle {
    precondition {
      condition     = data.k0rdent_share_type.chosen[0].max_capacity_gib == null || var.file_store.capacity_gib <= data.k0rdent_share_type.chosen[0].max_capacity_gib
      error_message = "file_store.capacity_gib is ${var.file_store.capacity_gib}, more than the ${coalesce(data.k0rdent_share_type.chosen[0].max_capacity_gib, 0)} GiB share type ${data.k0rdent_share_type.chosen[0].id} gives one store."
    }
  }
}

data "k0rdent_file_store" "this" {
  count = var.build.file_store ? 1 : 0

  id      = k0rdent_file_store.this[0].id
  project = local.project
}

# ---------------------------------------------------------------------------
# What the platform reports back
# ---------------------------------------------------------------------------
#
# Reads that should stop the run on a wrong answer have a postcondition. The
# rest feed the scorecard output.
#
# Reads of what this made use names from locals and depends_on
# terraform_data.made, so they're read at plan time unless something they
# report on changes.

# Fail the plan if the endpoint doesn't list the region.
data "k0rdent_regions" "all" {
  include_global = true

  lifecycle {
    postcondition {
      condition     = contains(self.ids, var.region)
      error_message = "Region ${var.region} isn't one this endpoint lists: ${join(", ", self.ids)}."
    }
  }
}

# Reading the organization needs the same organizations role as its quota, so
# quota_checks turns both off, for a credential scoped to a project.
data "k0rdent_organization" "this" {
  count = var.quota_checks ? 1 : 0

  id = local.organization

  lifecycle {
    postcondition {
      condition     = self.status == "active"
      error_message = "Organization ${local.organization} is ${coalesce(self.status, "in no status")}, so nothing can be built in it."
    }
  }
}

# Quota is the likeliest reason a create is refused.
data "k0rdent_project_quota" "this" {
  count = var.quota_checks ? 1 : 0

  organization_id = local.organization
  project_id      = local.project

  # With build.own_project the id is known at plan, so without this the read
  # runs before the project exists and gets a 404.
  depends_on = [k0rdent_project.own]
}

data "k0rdent_tenant_quota" "this" {
  count = var.quota_checks ? 1 : 0

  organization_id = local.organization
}

# What the reads below report on: what this made, its tags, and its rule
# count. When one changes, those reads wait for the apply. Otherwise, such as
# for a new display name, they're read at plan time.
resource "terraform_data" "made" {
  input = {
    cluster        = k0rdent_kubernetes_cluster.this.self_link
    hosts          = k0rdent_instance_group.this[*].self_link
    network        = [for n in k0rdent_network.this : { link = n.self_link, tags = n.tags_all }]
    security_group = [for g in k0rdent_security_group.this : { link = g.self_link, tags = g.tags_all, rules = g.rule_count }]
    webhook        = k0rdent_webhook_subscription.this[*].id
  }
}

# The cluster as the platform reports it, for its API address.
data "k0rdent_kubernetes_cluster" "this" {
  id      = local.name
  project = local.project

  depends_on = [terraform_data.made]
}

# Whether the cluster has a kubeconfig yet. Only the record, not the
# kubeconfig. nodes/ uses the kubeconfig itself.
data "k0rdent_cluster_kubeconfig" "this" {
  cluster_id = local.name
  project    = local.project

  depends_on = [terraform_data.made]
}

# The project's VPCs. The cluster output checks this made ones are listed.
data "k0rdent_vpcs" "all" {
  project = local.project

  depends_on = [terraform_data.made]
}

# A search by the tags from locals, to check the tags reached the platform.
data "k0rdent_networks" "tagged" {
  project = local.project
  tags    = local.tags

  depends_on = [terraform_data.made]
}

data "k0rdent_security_groups" "tagged" {
  project           = local.project
  tags              = local.tags
  only_tenant_owned = true

  depends_on = [terraform_data.made]
}

data "k0rdent_network" "this" {
  count = var.build.network ? 1 : 0

  id      = k0rdent_network.this[0].id
  project = local.project

  # Fail if the network isn't active.
  lifecycle {
    postcondition {
      condition     = self.state == "active"
      error_message = "Network ${self.id} is ${coalesce(self.state, "in no state")}: ${coalesce(self.state_message, "no message")}."
    }
  }
}

# Fails unless every subnet this made is listed under the network.
data "k0rdent_subnets" "this" {
  count = var.build.network ? 1 : 0

  network = k0rdent_network.this[0].id
  project = local.project

  depends_on = [k0rdent_subnet.this]

  lifecycle {
    postcondition {
      condition     = alltrue([for s in values(k0rdent_subnet.this) : contains(self.cidrs, s.cidr)])
      error_message = "A subnet this made isn't listed under network ${local.name}."
    }
  }
}

# Every subscription to the same URL. Delivery problems are only reported.
data "k0rdent_webhook_subscriptions" "same_url" {
  count = var.build.webhook ? 1 : 0

  url = var.webhook_url

  depends_on = [terraform_data.made]
}

# The peerings on the cluster's VPC.
data "k0rdent_vpc_peerings" "cluster" {
  count = var.build.vpc_peering ? 1 : 0

  vpc     = local.owners["cluster"].vpc
  project = local.project

  depends_on = [k0rdent_vpc_peering.this]
}
