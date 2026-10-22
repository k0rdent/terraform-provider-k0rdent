# Tenant networking on its own: a network, a subnet in it, and a security
# group that lets SSH and the Kubernetes API in from one range. No cluster, so
# it costs nothing to run and takes seconds. Set the values in
# terraform.tfvars, next to this file.

variable "prefix" {
  type        = string
  description = "Starts every name here, so two people running the walkthrough don't collide. Your own, such as your username."

  validation {
    condition     = can(regex("^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$", var.prefix))
    error_message = "Set prefix in terraform.tfvars to something of your own, such as your username: lower-case letters, digits and hyphens."
  }
}

variable "region" {
  type        = string
  description = "A region from look."

  validation {
    condition     = var.region != ""
    error_message = "Set region in terraform.tfvars to a region from look."
  }
}

variable "project" {
  type        = string
  description = "A project id from look."

  validation {
    condition     = var.project != ""
    error_message = "Set project in terraform.tfvars to a project id from look."
  }
}

variable "allowed_cidr" {
  type        = string
  description = "The range SSH and the Kubernetes API are let in from, such as your office or VPN."

  validation {
    condition     = can(cidrnetmask(var.allowed_cidr)) && try(cidrsubnet(var.allowed_cidr, 0, 0) == var.allowed_cidr, false)
    error_message = "Set allowed_cidr in terraform.tfvars to the range you connect from, such as 203.0.113.0/24, with the host bits zero."
  }
}

locals {
  name = "${var.prefix}-walkthrough"
}

terraform {
  required_providers {
    k0rdent = {
      source = "k0rdent/k0rdent"
    }
  }
}

# The endpoint and the credential come from K0RDENT_* in the environment.
# See setup.sh.
provider "k0rdent" {
  region = var.region
}

# Read first, so a wrong id stops the plan before anything is created.
data "k0rdent_project" "this" {
  id = var.project
}

# Subnets come from an address pool. Use an active pool that isn't routable,
# since routable pools are small and kept for cluster endpoints.
data "k0rdent_address_pools" "active" {
  state = "active"
}

locals {
  pools = data.k0rdent_address_pools.active.address_pools
  # The first pool that isn't routable, else the first pool, else null. try()
  # takes the first of these that doesn't fail.
  pool = try([for p in local.pools : p if !p.routable][0], local.pools[0], null)

  # A /28 (16 addresses), or the nearest size the pool allows.
  prefix_length = local.pool == null ? 28 : min(max(28, local.pool.min_prefix_length), local.pool.max_prefix_length)
}

resource "k0rdent_network" "this" {
  id           = local.name
  project      = data.k0rdent_project.this.id
  display_name = "${var.prefix} walkthrough"

  # An Ethernet fabric. "auto" lets the platform pick the VLAN.
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
  }
}

resource "k0rdent_subnet" "this" {
  id      = local.name
  project = data.k0rdent_project.this.id
  network = k0rdent_network.this.id

  # The platform picks the block from the pool. It's in the output.
  address_pool     = try(local.pool.self_link, null)
  prefix_length    = local.prefix_length
  allocate_gateway = try(local.pool.routable, false)

  lifecycle {
    # Fail the plan if there's no pool to use.
    precondition {
      condition     = local.pool != null
      error_message = "Region ${var.region} has no active address pool, so no subnet can be made. Ask whoever runs the landscape for one."
    }
  }
}

# Rules match first to last. The platform adds a permit-all ingress rule
# after these, so the denies keep everyone else out. 6443 has no deny,
# because a cluster's own API traffic may come from outside allowed_cidr.
resource "k0rdent_security_group" "this" {
  id           = local.name
  project      = data.k0rdent_project.this.id
  display_name = "${var.prefix} walkthrough"
  description  = "SSH and the Kubernetes API from ${var.allowed_cidr}."

  ingress {
    name                   = "ssh"
    action                 = "permit"
    protocol               = "tcp"
    source_prefix          = var.allowed_cidr
    destination_prefix     = "0.0.0.0/0"
    destination_port_range = "22"
  }

  ingress {
    name                   = "kubernetes-api"
    action                 = "permit"
    protocol               = "tcp"
    source_prefix          = var.allowed_cidr
    destination_prefix     = "0.0.0.0/0"
    destination_port_range = "6443"
  }

  ingress {
    name               = "ping"
    action             = "permit"
    protocol           = "icmp"
    source_prefix      = var.allowed_cidr
    destination_prefix = "0.0.0.0/0"
  }

  ingress {
    name                   = "ssh from anywhere else"
    action                 = "deny"
    protocol               = "tcp"
    source_prefix          = "0.0.0.0/0"
    destination_prefix     = "0.0.0.0/0"
    destination_port_range = "22"
  }

  ingress {
    name               = "ping from anywhere else"
    action             = "deny"
    protocol           = "icmp"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"
  }

  egress {
    name               = "everything-out"
    action             = "permit"
    protocol           = "any"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"
  }
}

output "network" {
  value = {
    id    = k0rdent_network.this.id
    type  = k0rdent_network.this.type
    state = k0rdent_network.this.state
    vlan  = "asked for ${k0rdent_network.this.evpn.vlan_id}, given ${k0rdent_network.this.evpn.vlan_id_resolved}"
  }
}

output "subnet" {
  value = {
    id        = k0rdent_subnet.this.id
    cidr      = k0rdent_subnet.this.cidr
    gateway   = k0rdent_subnet.this.gateway
    state     = k0rdent_subnet.this.state
    from_pool = "${local.pool.id} (${join(", ", local.pool.prefixes)}, ${local.pool.routable ? "routable" : "not routable"})"
  }
}

output "security_group" {
  value = {
    id    = k0rdent_security_group.this.id
    state = k0rdent_security_group.this.state
    # One line per rule, ingress then egress.
    rules = concat(
      [for r in k0rdent_security_group.this.ingress : "in:  ${r.action} ${r.protocol}${r.destination_port_range == null ? "" : " ${r.destination_port_range}"} from ${r.source_prefix} (${r.name})"],
      [for r in k0rdent_security_group.this.egress : "out: ${r.action} ${r.protocol} to ${r.destination_prefix} (${r.name})"],
    )
  }
}

output "next" {
  value = "To filter a cluster with this group, set security_groups = [\"${k0rdent_security_group.this.id}\"] on it. The full/ directory does that."
}
