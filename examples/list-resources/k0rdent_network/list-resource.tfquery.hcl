# Every network in the provider's project, except those a cluster made for
# itself.
list "k0rdent_network" "all" {
  provider = k0rdent
}

# Including the networks clusters made.
list "k0rdent_network" "everything" {
  provider = k0rdent

  config {
    include_platform_owned = true
  }
}
