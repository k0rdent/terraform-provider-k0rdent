data "k0rdent_share_types" "available" {}

# Capability keys vary, so lookup() gives a default when one is missing.
output "classes_by_protocol" {
  value = {
    for s in data.k0rdent_share_types.available.share_types :
    s.id => lookup(s.capabilities, "protocol", "unknown")
  }
}

# The share types in another region.
data "k0rdent_share_types" "eu" {
  region = "eu-frankfurt-1"
}
