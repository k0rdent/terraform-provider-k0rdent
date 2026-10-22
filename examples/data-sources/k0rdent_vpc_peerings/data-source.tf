# Only this project's peerings. The other side's peering back doesn't appear.
data "k0rdent_vpc_peerings" "training" {
  vpc = local.training_vpc
}
