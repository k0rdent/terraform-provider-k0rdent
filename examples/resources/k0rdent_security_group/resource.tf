# The first matching rule wins, in the order written.
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

  ingress {
    name               = "deny-everything-else"
    action             = "deny"
    protocol           = "any"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"
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
}
