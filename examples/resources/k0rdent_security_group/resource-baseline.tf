# A baseline for every VPC, bound after the more specific groups. Egress only,
# so it lets nothing in.
resource "k0rdent_security_group" "baseline" {
  id          = "baseline"
  description = "Outbound DNS and HTTPS for every VPC."

  egress {
    name               = "dns-out"
    action             = "permit"
    protocol           = "udp"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"

    destination_port_range = "53"
  }

  egress {
    name               = "https-out"
    action             = "permit"
    protocol           = "tcp"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"

    destination_port_range = "443"
  }
}
