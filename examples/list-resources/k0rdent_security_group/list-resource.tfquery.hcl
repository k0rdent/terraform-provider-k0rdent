# Every security group in the provider's project. VPC default groups are left
# out unless `include_platform_owned` is set.
list "k0rdent_security_group" "all" {
  provider = k0rdent
}
