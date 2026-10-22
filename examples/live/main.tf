# The live configuration. `make live` runs Terraform against this directory,
# pointed at a real control plane: a local `kind` stack or a deployed
# landscape. It creates real resources and destroys them again.
#
# Unlike examples/demo, it keeps the provider's default timeouts, has no
# default region or project, has no instance group (that needs hardware), and
# reads the kubeconfig only behind a switch. Everything comes from the
# environment by default, so it runs with no `-var`.

terraform {
  required_providers {
    k0rdent = {
      source = "k0rdent/k0rdent"
    }
  }
}

# Empty: endpoint, region and project come from K0RDENT_*.
provider "k0rdent" {}

# ---------------------------------------------------------------------------
# 0. Where the run builds
# ---------------------------------------------------------------------------
# Two variables pick the project:
#
#   organization_id set  The run creates its own project there (named
#                        `project_id` if set), and the destroy deletes it.
#                        Needs an organization-scoped credential.
#   project_id set       The run uses that existing project.
#   neither              The provider's project, from K0RDENT_PROJECT.
#
# Blocks below take `project = local.project`. Null means the provider's.

resource "k0rdent_project" "run" {
  # count = 1 or 0 turns the resource on or off.
  count = var.organization_id != null ? 1 : 0

  # Named after the run, so a leftover project says which run made it.
  id              = coalesce(var.project_id, "${var.cluster_name}-project")
  organization_id = var.organization_id

  display_name = "Terraform live test project"
  description  = "Created by examples/live for one run, with everything in it. Safe to delete."
}

data "k0rdent_project" "existing" {
  count = var.organization_id == null && var.project_id != null ? 1 : 0
  id    = var.project_id
}

# The project in use. [0] is the one instance when count is 1.
locals {
  project = var.organization_id != null ? k0rdent_project.run[0].id : var.project_id
}

# Warn if the existing project isn't active. try() covers a failed read.
check "project_is_usable" {
  assert {
    condition = (
      length(data.k0rdent_project.existing) == 0 ||
      try(data.k0rdent_project.existing[0].status, "") == "active"
    )
    error_message = "Project ${coalesce(var.project_id, "-")} is ${try(data.k0rdent_project.existing[0].status, "unreadable")}, so nothing can be built in it."
  }
}

# ---------------------------------------------------------------------------
# 1. Look up a cluster type
# ---------------------------------------------------------------------------
# The cluster types in the region. A cluster's `cluster_type` takes an
# entry's `self_link`.

data "k0rdent_cluster_types" "available" {}

# For the scorecard's regions row.
data "k0rdent_regions" "all" {}

# The list doesn't include node pools, so read the chosen one. coalesce()
# takes var.cluster_type_id, or the first type if that's null.
data "k0rdent_cluster_type" "chosen" {
  id = coalesce(var.cluster_type_id, data.k0rdent_cluster_types.available.cluster_types[0].id)
}

locals {
  # The pools you can resize. Control-plane pools can't be.
  worker_pools = [for p in data.k0rdent_cluster_type.chosen.node_pools : p if p.role == "worker"]
  # The first of them. This works with any cluster type.
  worker_pool = local.worker_pools[0]
  # The pool's minimum, or 0 if it sets none.
  worker_floor = coalesce(local.worker_pool.node_count_min, 0)
  # var.worker_nodes, or the minimum if that's null.
  worker_nodes = coalesce(var.worker_nodes, local.worker_floor)
}

# ---------------------------------------------------------------------------
# 2. Create a cluster
# ---------------------------------------------------------------------------

resource "k0rdent_kubernetes_cluster" "live" {
  id      = var.cluster_name
  project = local.project

  # The unversioned reference, unless `cluster_type_version` pins one.
  # scripts/live.sh creates on the oldest version, then sets the newest to
  # upgrade in place.
  cluster_type = var.cluster_type_version == null ? data.k0rdent_cluster_type.chosen.self_link : "${data.k0rdent_cluster_type.chosen.self_link}/versions/${var.cluster_type_version}"

  display_name = "Terraform live test cluster"
  description  = "Created by examples/live. Safe to delete."

  tags = {
    owner = "terraform-provider-live-test"
  }

  node_pool = {
    (local.worker_pool.id) = {
      node_count = local.worker_nodes
    }
  }
}

# ---------------------------------------------------------------------------
# 3. Networking
# ---------------------------------------------------------------------------
# Off unless `capabilities.vpc_and_subnets` is set, since it needs an
# AddressPool in the region.

data "k0rdent_address_pools" "all" {
  count = local.exercise.vpc_and_subnets ? 1 : 0
}

resource "k0rdent_network" "live" {
  count = local.exercise.vpc_and_subnets ? 1 : 0

  id           = "${var.cluster_name}-fabric"
  project      = local.project
  display_name = "Live check"

  evpn = {
    service_type = "l3vpn"

    # "auto" lets the platform pick. The next plan should still be empty.
    vlan_id = "auto"
  }
}

resource "k0rdent_subnet" "live" {
  count = local.exercise.vpc_and_subnets ? 1 : 0

  id      = "${var.cluster_name}-pods"
  project = local.project
  network = k0rdent_network.live[0].id

  # The region's first pool, at its smallest size.
  address_pool  = data.k0rdent_address_pools.all[0].address_pools[0].self_link
  prefix_length = data.k0rdent_address_pools.all[0].address_pools[0].max_prefix_length
}

# ---------------------------------------------------------------------------
# 4. Config lookup: the version catalogue
# ---------------------------------------------------------------------------
# The Kubernetes versions on offer come with the ClusterType's versions, so
# read its version history.

data "k0rdent_cluster_type_versions" "history" {
  count        = local.exercise.version_catalogue ? 1 : 0
  cluster_type = data.k0rdent_cluster_type.chosen.id
}

# ---------------------------------------------------------------------------
# 5. Cluster connection
# ---------------------------------------------------------------------------
# The kubeconfig is an ephemeral resource, so its token isn't saved to state.

ephemeral "k0rdent_cluster_kubeconfig" "live" {
  count = local.exercise.kubeconfig ? 1 : 0

  # Read the kubeconfig after the cluster exists. `id` is known at plan time,
  # but `state` isn't until the create has run.
  cluster_id = k0rdent_kubernetes_cluster.live.state != null ? k0rdent_kubernetes_cluster.live.id : null
  project    = local.project
}

# ---------------------------------------------------------------------------
# 6. Filter rules
# ---------------------------------------------------------------------------
# These filter node interfaces, not the Kubernetes API endpoint. Only a VPC on
# the `nico` backend takes security groups. `vpc_backends` shows each VPC's.

resource "k0rdent_security_group" "live" {
  count = local.exercise.security_groups ? 1 : 0

  id           = "${var.cluster_name}-filter"
  project      = local.project
  display_name = "Live check"

  ingress {
    name               = "allow-ssh-from-office"
    action             = "permit"
    protocol           = "tcp"
    source_prefix      = "203.0.113.0/24"
    destination_prefix = "0.0.0.0/0"

    destination_port_range = "22"
  }

  egress {
    action             = "permit"
    protocol           = "any"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"
  }
}

data "k0rdent_vpcs" "live" {
  count   = local.exercise.security_groups ? 1 : 0
  project = local.project
  # Read after the cluster, so its VPC is listed.
  depends_on = [k0rdent_kubernetes_cluster.live]
}

# ---------------------------------------------------------------------------
# Capability switches, one per capability the API supports
# ---------------------------------------------------------------------------
# One switch per capability a landscape may or may not support.

variable "capabilities" {
  type = object({
    vpc_and_subnets   = optional(bool, false)
    kubeconfig        = optional(bool, false)
    security_groups   = optional(bool, false)
    version_catalogue = optional(bool, true)
  })
  description = <<-EOT
    Which capabilities to exercise on this run.

      vpc_and_subnets    Networks and subnets. Needs an operator-authored
                         AddressPool in the region.
      kubeconfig         The ephemeral kubeconfig. Needs the landscape to
                         create one when the cluster becomes active. Off by
                         default.
      security_groups    Tenant filter rules. Needs a VPC on the `nico`
                         fabric backend.
      version_catalogue  The ClusterType version history. Needs nothing beyond
                         the catalog, so it is on by default.
  EOT
  default     = {}
}

locals {
  exercise = {
    vpc_and_subnets   = var.capabilities.vpc_and_subnets
    kubeconfig        = var.capabilities.kubeconfig
    security_groups   = var.capabilities.security_groups
    version_catalogue = var.capabilities.version_catalogue
  }
}

variable "organization_id" {
  type        = string
  description = <<-EOT
    Set it and the run creates its own project in this organization, and the
    destroy deletes it. Needs an organization-scoped credential. `make
    live-preflight` lists the organizations the credential can see. Section 0
    has the three ways to pick a project.
  EOT
  default     = null
}

variable "project_id" {
  type        = string
  description = "An existing project to build in. With `organization_id` set, the id to give the new one instead. Null with both null uses the provider's project."
  default     = null
}

variable "cluster_name" {
  type        = string
  description = "Cluster id. scripts/live.sh generates a unique one per run."
  default     = "tf-live"
}

variable "cluster_type_id" {
  type        = string
  description = "Which cluster type to use. Null picks the first in the catalog."
  default     = null
}

variable "cluster_type_version" {
  type        = string
  description = "Version id (a UUID) to pin `cluster_type` to. Null uses the unversioned reference, which binds the latest. scripts/live.sh sets it to run the upgrade step."
  default     = null
}

variable "worker_nodes" {
  type        = number
  description = <<-EOT
    Nodes in the cluster's worker pool. The scale step raises this.

    Null by default, which means the fewest the pool accepts: zero unless the
    cluster type sets a floor. Zero is not an accident: growing a pool from
    empty is a supported path that needs no node reservation, while resizing a
    pool that already holds nodes does. A landscape with no server inventory
    can do the first and not the second, so starting as low as the pool allows
    is what makes the scale step testable on the most landscapes.
  EOT
  default     = null
}

# ---------------------------------------------------------------------------
# Outputs
# ---------------------------------------------------------------------------

output "project" {
  description = "Where this run built, and whether it made the project itself."
  value = {
    id   = local.project
    from = var.organization_id != null ? "created by this run" : var.project_id != null ? "existing" : "the provider's project, K0RDENT_PROJECT"
  }
}

output "worker_nodes" {
  description = "Nodes the worker pool was given: `worker_nodes`, or the pool's floor when that's null."
  value       = local.worker_nodes
}

output "catalog" {
  description = "Every cluster type the region offers."
  value       = [for ct in data.k0rdent_cluster_types.available.cluster_types : "${ct.id} (${ct.display_name}, ${ct.status})"]
}

output "node_pools" {
  description = "Node pools on the chosen cluster type, with the counts they accept."
  value = [for p in data.k0rdent_cluster_type.chosen.node_pools :
    "${p.id}: ${p.role}, default ${coalesce(p.node_count_default, 0)}, max ${coalesce(p.node_count_max, 0)}"
  ]
}

output "cluster" {
  description = "What the platform reports for the cluster."
  value = {
    id         = k0rdent_kubernetes_cluster.live.id
    state      = k0rdent_kubernetes_cluster.live.state
    node_count = k0rdent_kubernetes_cluster.live.node_count
    node_pool  = k0rdent_kubernetes_cluster.live.node_pool
    self_link  = k0rdent_kubernetes_cluster.live.self_link
  }
}

# The CIDR the platform picked.
output "subnet_cidr" {
  description = "The block the platform allocated. Null unless capabilities.vpc_and_subnets is on."
  value       = local.exercise.vpc_and_subnets ? k0rdent_subnet.live[0].cidr : null
}

output "vlan_round_trip" {
  description = "What was written against what the platform resolved. The fake's ASSUMPTION on `vlan.id = \"auto\"` turns on this pair."
  value = local.exercise.vpc_and_subnets ? {
    written  = k0rdent_network.live[0].evpn.vlan_id
    resolved = k0rdent_network.live[0].evpn.vlan_id_resolved
  } : null
}

output "vpc_backends" {
  description = "Each VPC's fabric backend. Security groups need `nico`. Null unless capabilities.security_groups is on."
  value       = local.exercise.security_groups ? { for v in data.k0rdent_vpcs.live[0].vpcs : v.id => v.backend } : null
}

output "cluster_type_round_trip" {
  description = <<-EOT
    The hazard, made visible. `written` is what the configuration asked for;
    `returned` is what the platform bound. If they differ and a plan still
    reports no changes, the provider is handling the resolution. That is the
    single most valuable thing this run shows.
  EOT
  value = {
    written  = k0rdent_kubernetes_cluster.live.cluster_type
    returned = k0rdent_kubernetes_cluster.live.cluster_type_resolved
  }
}

# ---------------------------------------------------------------------------
# The scorecard
# ---------------------------------------------------------------------------
# One row per capability. "not exercised" means the switch was off, not that
# it failed.

output "scorecard" {
  description = "Each capability, answered by this run."
  value = {
    "VPC and subnets" = local.exercise.vpc_and_subnets ? format(
      "subnet created, CIDR %s allocated; the next plan reads it back and the destroy deletes it",
      k0rdent_subnet.live[0].cidr,
    ) : "not exercised: set capabilities.vpc_and_subnets, needs an AddressPool in the region"

    "Kubernetes cluster" = format(
      "yes. %s is %s, and the plan after this apply is what proves the round trip",
      k0rdent_kubernetes_cluster.live.id, k0rdent_kubernetes_cluster.live.state,
    )

    "Node pool scale" = local.worker_nodes > local.worker_floor ? format(
      "yes. pool %s at %d nodes, up from its floor of %d; scripts/live.sh raises it again and re-plans",
      local.worker_pool.id, local.worker_nodes, local.worker_floor,
      ) : format(
      "not exercised: the pool is at its floor of %d. Apply again with -var worker_nodes=%d to scale it",
      local.worker_floor, local.worker_floor + 1,
    )

    # An output can't hold an ephemeral value, so a successful apply is the
    # evidence.
    "Kubeconfig" = local.exercise.kubeconfig ? "yes once this apply succeeds, as an ephemeral resource" : "not exercised: set capabilities.kubeconfig"

    "Regions" = format(
      "yes. k0rdent_regions read %d: %s",
      length(data.k0rdent_regions.all.ids), join(", ", data.k0rdent_regions.all.ids),
    )

    "ClusterType versions" = local.exercise.version_catalogue ? format(
      "yes. %d versions of %s",
      length(data.k0rdent_cluster_type_versions.history[0].versions),
      data.k0rdent_cluster_type.chosen.id,
    ) : "not exercised: set capabilities.version_catalogue"

    "Security group" = local.exercise.security_groups ? format(
      "yes. security group %s filters node interfaces",
      k0rdent_security_group.live[0].id,
    ) : "not exercised: set capabilities.security_groups"
  }
}
