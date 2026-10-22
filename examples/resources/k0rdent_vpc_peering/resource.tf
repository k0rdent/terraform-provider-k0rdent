# Take the VPC from the cluster that made it, not from a data source. A data
# source can become unknown when the cluster changes, which would replace
# this peering.
locals {
  training_vpc = k0rdent_kubernetes_cluster.training.vpc
}

# A VPC in this project goes in remote_vpc by id, such as
# k0rdent_instance_group.hosts.vpc. One in another project needs its
# `self_link`. Read it here if you can read that project, or have its own
# configuration output it.
data "k0rdent_vpc" "inference" {
  project = "inference"
  id      = "vpc-inference"
}

# This is one side. Traffic flows only once the other project adds a peering
# back. Until then this one still shows `active`, with no error.
resource "k0rdent_vpc_peering" "to_inference" {
  id  = "to-inference"
  vpc = local.training_vpc

  remote_vpc = data.k0rdent_vpc.inference.self_link

  display_name = "Training to inference"

  # Set only at creation, like the provider's `default_tags` here. Later
  # changes are ignored, with a warning in the plan.
  tags = {
    team = "ml-platform"
  }
}
