# The only file you edit. Fill these in from what look printed. Everything
# else has a default, in variables.tf, that you can override here.

prefix       = "" # yours, such as your username. Every name starts with it
environment  = "dev"
region       = "" # a region from look
project      = "" # a project id from look. Left empty with build.own_project
organization = "" # an organization id from look. Only for build.own_project
cluster_type = "" # a cluster type look listed under that region

ssh_public_key_file = "~/.ssh/id_ed25519.pub" # your own public key, for ops on every node
allowed_cidrs       = []                      # where you connect from, such as ["203.0.113.0/24", "198.51.100.0/25"]
webhook_url         = ""                      # where cluster events go, such as https://hooks.example.com/k0rdent

# Optional. Uncomment to change the default.
# address_pool           = ""                           # a pool that isn't routable. Empty takes the first active one
# subnets                = { nodes = 28, storage = 28 } # a subnet per purpose, and its prefix length
# worker_counts          = { workers = 1 }              # nodes per worker pool. Left out, each starts at its floor
# cluster_type_version   = ""                           # a version id from the cluster_type output, to pin it
# webhook_secret_version = 2                            # one more, with a new TF_VAR_webhook_secret, rotates the webhook's secret
# quota_checks           = false                        # skip the organization and quota reads, for a credential scoped to a project
# ingress_services = {
#   ssh      = { protocol = "tcp", port = "22" }
#   kube-api = { protocol = "tcp", port = "6443" }
#   ping     = { protocol = "icmp" }
# }
# ssh_users = {
#   ops = { sudo = true }
#   ci  = { public_key_files = ["~/.ssh/ci.pub"] }
# }

# What to build. Turn on what your landscape supports, and off what it
# doesn't. The scorecard output lists both.
build = {
  network             = true  # a network and a subnet per purpose. Needs an address pool that isn't routable
  security_group      = true  # a group on the cluster. Needs nodes on a nico-backed VPC
  vpc_security_groups = true  # the same group on every VPC this makes, before its default group. Needs security_group too
  webhook             = true  # cluster events to webhook_url. Needs TF_VAR_webhook_secret
  instance_group      = false # one bare host. Needs real hardware
  file_store          = false # 100 GiB of shared storage. Needs a share type in the region
  vpc_peering         = false # the cluster's VPC to the hosts' and back. Needs instance_group
  own_project         = false # a project of its own. Needs a credential scoped to the organization
}
