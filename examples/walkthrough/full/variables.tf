# Every input full/ takes. Set them in terraform.tfvars, next to this file.
# A wrong value stops the plan with a message saying what to set. In the
# checks, alltrue([for ...]) means every entry has to pass.

variable "build" {
  type = object({
    network             = optional(bool, true)
    security_group      = optional(bool, true)
    vpc_security_groups = optional(bool, true)
    webhook             = optional(bool, true)
    instance_group      = optional(bool, false)
    file_store          = optional(bool, false)
    vpc_peering         = optional(bool, false)
    own_project         = optional(bool, false)
  })
  description = "What to build. Each one is explained in terraform.tfvars."
  default     = {}

  validation {
    condition     = !var.build.vpc_security_groups || var.build.security_group
    error_message = "build.vpc_security_groups binds the security group to the VPCs this makes, so it needs build.security_group = true too."
  }

  validation {
    condition     = !var.build.vpc_peering || var.build.instance_group
    error_message = "build.vpc_peering peers the cluster's VPC with the instance group's, so it needs build.instance_group = true too."
  }
}

# ---------------------------------------------------------------------------
# Who and where
# ---------------------------------------------------------------------------

variable "prefix" {
  type        = string
  description = "Starts every name here, so two people running the walkthrough don't collide. Your own, such as your username."

  validation {
    condition     = can(regex("^[a-z]([-a-z0-9]{0,30}[a-z0-9])?$", var.prefix))
    error_message = "Set prefix in terraform.tfvars to something of your own, such as your username: lower-case letters, digits and hyphens."
  }
}

variable "environment" {
  type        = string
  description = "Which stage this is, such as dev or prod. It goes in every tag and description, and prod makes a destroy wait for a running operation rather than cancel it."
  default     = "dev"

  validation {
    condition     = can(regex("^[a-z][a-z0-9]{0,15}$", var.environment))
    error_message = "Set environment to a short lower-case word, such as dev, staging or prod."
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
  description = "A project id from look. Not used with build.own_project."
  default     = ""

  validation {
    condition     = var.build.own_project || var.project != ""
    error_message = "Set project in terraform.tfvars to a project id from look, or set build.own_project = true and organization."
  }
}

variable "organization" {
  type        = string
  description = "An organization id from look. Only used with build.own_project."
  default     = ""

  validation {
    condition     = !var.build.own_project || var.organization != ""
    error_message = "Set organization in terraform.tfvars to an organization id from look. build.own_project creates the project in it."
  }
}

variable "quota_checks" {
  type        = bool
  description = "Read the project's and the organization's quota, and warn when either is over. Set false for a credential that can't read quota."
  default     = true
}

# ---------------------------------------------------------------------------
# The cluster
# ---------------------------------------------------------------------------

variable "cluster_type" {
  type        = string
  description = "A cluster type from look, in that region."

  validation {
    condition     = var.cluster_type != ""
    error_message = "Set cluster_type in terraform.tfvars to a cluster type look listed under your region."
  }
}

variable "cluster_type_version" {
  type        = string
  description = "A version id of cluster_type to pin. Empty takes the type's current version."
  default     = ""

  validation {
    condition     = var.cluster_type_version == "" || can(regex("^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$", var.cluster_type_version))
    error_message = "Set cluster_type_version to a version id, a lower-case UUID from the cluster_type output, or leave it empty for the current version."
  }
}

variable "worker_counts" {
  type        = map(number)
  description = "Nodes per worker pool, keyed by pool id. A pool left out starts at the smallest count the cluster type allows."
  default     = {}

  validation {
    condition     = alltrue([for n in values(var.worker_counts) : n >= 0 && floor(n) == n])
    error_message = "Each worker_counts value is a whole number of nodes, 0 or more."
  }
}

variable "ssh_public_key_file" {
  type        = string
  description = "Your SSH public key. Every user in ssh_users without keys of its own gets it."

  validation {
    condition     = try(fileexists(pathexpand(var.ssh_public_key_file)), false)
    error_message = "Set ssh_public_key_file in terraform.tfvars to your own public key, such as ~/.ssh/id_ed25519.pub."
  }
}

# Set when nodes are built, so changing this later replaces the cluster,
# which the plan refuses without allow_replace.
variable "ssh_users" {
  type = map(object({
    public_key_files = optional(list(string), [])
    sudo             = optional(bool, false)
  }))
  description = "Linux users put on every node, control plane and workers, and on every host. Keyed by username."
  default = {
    ops = { sudo = true }
  }

  validation {
    condition     = length(var.ssh_users) >= 1 && length(var.ssh_users) <= 32
    error_message = "ssh_users needs 1 to 32 users."
  }

  validation {
    condition     = alltrue([for u in keys(var.ssh_users) : can(regex("^[a-z_][a-z0-9_-]{0,31}$", u))])
    error_message = "Each ssh_users key is a Linux username: a lower-case letter or underscore, then up to 31 lower-case letters, digits, underscores or hyphens."
  }

  validation {
    condition     = alltrue(flatten([for u in values(var.ssh_users) : [for f in u.public_key_files : try(fileexists(pathexpand(f)), false)]]))
    error_message = "Every file in ssh_users' public_key_files has to exist. Leave public_key_files out to use ssh_public_key_file."
  }
}

# ---------------------------------------------------------------------------
# Networking
# ---------------------------------------------------------------------------

variable "address_pool" {
  type        = string
  description = "The address pool subnets come from. Empty takes the first active one that isn't routable."
  default     = ""

  validation {
    condition     = var.address_pool == "" || can(regex("^[a-z]([-a-z0-9]*[a-z0-9])?$", var.address_pool))
    error_message = "Set address_pool to a pool id from look, or leave it empty."
  }
}

variable "subnets" {
  type        = map(number)
  description = "One subnet per purpose, keyed by purpose, each the prefix length to ask for. The pool's limits win where they differ."
  default = {
    nodes   = 28
    storage = 28
  }

  validation {
    condition     = alltrue([for k in keys(var.subnets) : can(regex("^[a-z][a-z0-9]{0,15}$", k))])
    error_message = "Each subnets key is a short lower-case word, such as nodes or storage."
  }

  validation {
    condition     = alltrue([for n in values(var.subnets) : n >= 8 && n <= 32 && floor(n) == n])
    error_message = "Each subnets value is a prefix length from 8 to 32, such as 28 for 16 addresses."
  }
}

variable "allowed_cidrs" {
  type        = list(string)
  description = "The ranges the security group lets ingress_services in from, such as your office and your VPN."
  default     = []

  validation {
    condition     = !var.build.security_group || length(var.allowed_cidrs) > 0
    error_message = "Set allowed_cidrs in terraform.tfvars to the ranges you connect from, such as [\"203.0.113.0/24\"], or set build.security_group = false."
  }

  validation {
    condition     = alltrue([for c in var.allowed_cidrs : can(cidrhost(c, 0)) && !strcontains(c, ":") && try(cidrsubnet(c, 0, 0) == c, false)])
    error_message = "Each allowed_cidrs entry is an IPv4 range with the host bits zero, such as 203.0.113.0/24."
  }

  validation {
    condition     = !contains(var.allowed_cidrs, "0.0.0.0/0")
    error_message = "0.0.0.0/0 in allowed_cidrs opens every service to the internet. Name the ranges you connect from."
  }

  validation {
    condition     = length(distinct(var.allowed_cidrs)) == length(var.allowed_cidrs)
    error_message = "allowed_cidrs lists a range twice."
  }
}

variable "ingress_services" {
  type = map(object({
    protocol = string
    port     = optional(string)
  }))
  description = "What allowed_cidrs may reach, keyed by name. One rule per service and range."
  default = {
    ssh      = { protocol = "tcp", port = "22" }
    kube-api = { protocol = "tcp", port = "6443" }
    ping     = { protocol = "icmp" }
  }

  validation {
    condition     = alltrue([for s in values(var.ingress_services) : contains(["tcp", "udp", "icmp"], s.protocol)])
    error_message = "Each ingress_services protocol is tcp, udp or icmp."
  }

  validation {
    condition = alltrue([
      for s in values(var.ingress_services) :
      contains(["tcp", "udp"], s.protocol) ? can(regex("^[1-9][0-9]{0,4}(-[1-9][0-9]{0,4})?$", s.port)) : s.port == null
    ])
    error_message = "A tcp or udp service needs a port, such as \"22\" or \"8000-8080\". An icmp one takes none."
  }

  validation {
    condition = alltrue([
      for s in values(var.ingress_services) :
      try(tonumber(split("-", s.port)[0]) < tonumber(split("-", s.port)[1]), true)
    ])
    error_message = "A port range runs low to high, and a single port is written alone: \"22\", not \"22-22\"."
  }

  # The platform refuses a group past 256 rules, ingress and egress together.
  validation {
    condition     = length(var.ingress_services) * (length(var.allowed_cidrs) + 1) + length([for s in values(var.ingress_services) : s if s.port != "6443"]) + length(local.egress_out) <= 256
    error_message = "The security group would hold more than the 256 rules a group can: a permit for each service and range, a reply rule for each service, a deny for each service but 6443, and the outbound rules. Use fewer ingress_services or allowed_cidrs."
  }
}

# ---------------------------------------------------------------------------
# Events and automation
# ---------------------------------------------------------------------------

variable "webhook_url" {
  type        = string
  description = "Where the platform sends cluster events."
  default     = ""

  validation {
    condition     = !var.build.webhook || can(regex("^https?://[^/]+", var.webhook_url))
    error_message = "Set webhook_url in terraform.tfvars to an http or https URL that receives events, or set build.webhook = false."
  }
}

variable "webhook_events" {
  type        = set(string)
  description = "The event types the webhook gets."
  default = [
    "io.k0rdent.workflow.cluster.create.v1",
    "io.k0rdent.workflow.cluster.create.failed.v1",
  ]

  validation {
    condition     = length(var.webhook_events) > 0
    error_message = "webhook_events needs at least one event type. Every event is what an empty list would mean, and that's rarely wanted."
  }

  validation {
    condition     = alltrue([for e in var.webhook_events : can(regex("^[a-z0-9]+(\\.[a-z0-9_]+){2,}$", e))])
    error_message = "Each webhook_events entry is dotted lower case with three segments or more, such as io.k0rdent.workflow.cluster.create.v1."
  }
}

# Not in terraform.tfvars: a secret belongs in your shell, never in a file.
# Ephemeral and write-only, so it never reaches state or a plan.
variable "webhook_secret" {
  type        = string
  description = "The webhook's signing secret, from TF_VAR_webhook_secret."
  default     = null
  sensitive   = true
  ephemeral   = true

  validation {
    condition     = !var.build.webhook || try(length(var.webhook_secret) >= 32, false)
    error_message = "Export the webhook's signing secret first, 32 characters or more: export TF_VAR_webhook_secret=$(openssl rand -hex 32). Or set build.webhook = false."
  }
}

variable "webhook_secret_version" {
  type        = number
  description = "Raise it by one, with a new TF_VAR_webhook_secret, to rotate the webhook's secret in place."
  default     = 1

  validation {
    condition     = var.webhook_secret_version >= 1
    error_message = "webhook_secret_version starts at 1."
  }
}

# ---------------------------------------------------------------------------
# Storage
# ---------------------------------------------------------------------------

variable "file_store" {
  type = object({
    share_type   = optional(string, "")
    capacity_gib = optional(number, 100)
    mount_path   = optional(string, "/mnt/shared")
  })
  description = "The file store build.file_store makes. An empty share_type takes the first active one."
  default     = {}

  validation {
    condition     = var.file_store.capacity_gib >= 1 && floor(var.file_store.capacity_gib) == var.file_store.capacity_gib
    error_message = "file_store.capacity_gib is a whole number of GiB, 1 or more. It can grow later, never shrink."
  }

  validation {
    condition     = startswith(var.file_store.mount_path, "/")
    error_message = "file_store.mount_path is an absolute path, such as /mnt/shared."
  }
}
