# Reading the share type fails the plan if it doesn't exist in this region,
# instead of failing the apply.
data "k0rdent_share_type" "fast" {
  id = "high-speed"
}

resource "k0rdent_file_store" "datasets" {
  share_type   = data.k0rdent_share_type.fast.id
  capacity_gib = 4096
}
