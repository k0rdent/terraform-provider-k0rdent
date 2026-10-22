data "k0rdent_vpc" "training" {
  id = "vpc-training"
}

# Empty except on the nico fabric.
output "attached_groups" {
  value = data.k0rdent_vpc.training.security_groups
}
