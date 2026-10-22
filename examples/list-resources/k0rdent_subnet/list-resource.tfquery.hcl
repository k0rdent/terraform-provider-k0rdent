# Every subnet on every network in the provider's project.
list "k0rdent_subnet" "all" {
  provider = k0rdent
}

# The subnets on one network.
list "k0rdent_subnet" "training" {
  provider = k0rdent

  config {
    network = "training-fabric"
  }
}
