# The first matching rule wins, in the order written. On the nico fabric,
# traffic that no rule permits is denied, so the group needs no final deny.
# One would also block what the VPC's default group, checked after this one,
# permits between the VPC's subnets.
resource "k0rdent_security_group" "office_access" {
  id           = "office-access"
  display_name = "Office access"

  # Merged with the provider's `default_tags` into `tags_all`.
  tags = {
    team = "it"
  }

  ingress {
    name               = "ssh-from-office"
    action             = "permit"
    protocol           = "tcp"
    source_prefix      = "203.0.113.0/24"
    destination_prefix = "0.0.0.0/0"

    destination_port_range = "22"
  }

  # On the nico fabric, return traffic for this rule is allowed back in
  # without an ingress rule.
  egress {
    name               = "https-out"
    action             = "permit"
    protocol           = "tcp"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"

    destination_port_range = "443"
  }

  # Ingress rules are stateless, so the SSH replies need a rule of their own.
  egress {
    name               = "ssh-replies"
    action             = "permit"
    protocol           = "tcp"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "203.0.113.0/24"

    source_port_range = "22"
  }
}
