package acceptance

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

var (
	networkDataAddr       = "data." + networkType + ".read"
	subnetDataAddr        = "data." + subnetType + ".read"
	securityGroupDataAddr = "data." + securityGroupType + ".read"
	defaultGroupDataAddr  = "data." + securityGroupType + ".vpc_default"
	peeringDataAddr       = "data." + peeringType + ".read"
	vpcType               = provider.ResourceName("vpc")
)

func TestAccNetworkAndSubnetDataSources(t *testing.T) {
	const network, subnet = "acc-fabric-read", "acc-sub-read"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(subnetType, subnetPath),
			checkGone(networkType, networkPath),
		),
		Steps: []resource.TestStep{{
			Config: subnetConfig(network, subnet) + fmt.Sprintf(`
data %q "read" {
  id         = %q
  depends_on = [%s.test]
}

data %q "read" {
  id         = %q
  network    = %q
  depends_on = [%s.test]
}
`, networkType, network, networkType, subnetType, subnet, network, subnetType),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(networkDataAddr, "type", "evpn"),
				resource.TestCheckResourceAttr(networkDataAddr, "tags.team", "ml"),
				resource.TestCheckResourceAttr(networkDataAddr, "mtu", "9000"),
				// The allocated value, never the "auto" that was asked for.
				resource.TestMatchResourceAttr(networkDataAddr, "vlan_id", regexp.MustCompile(`^[0-9]+$`)),
				resource.TestCheckNoResourceAttr(networkDataAddr, "cluster_id"),
				resource.TestCheckResourceAttr(subnetDataAddr, "state", "active"),
				resource.TestCheckResourceAttrPair(subnetDataAddr, "cidr", subnetAddr, "cidr"),
				resource.TestCheckResourceAttr(subnetDataAddr, "prefix_length", "24"),
				// The data source reports what the platform sends, and kind
				// sends a subnet's tags as {}.
				resource.TestCheckResourceAttr(subnetDataAddr, "tags.%", "0"),
				resource.TestCheckNoResourceAttr(subnetDataAddr, "state_reason"),
			),
		}},
	})
}

// vpcDefaultGroupConfig reads the default group of a cluster's VPC. The id is
// random, so it comes off the VPC's binding, which holds only that group
// until something else is bound.
func vpcDefaultGroupConfig() string {
	return fmt.Sprintf(`
data %q "owned" {
  id = %s.test.vpc
}

data %q "vpc_default" {
  id = one(data.%s.owned.security_groups)
}
`, vpcType, clusterType, securityGroupType, vpcType)
}

// A tenant group has no owner. A VPC's default group names the VPC that owns
// it, which is the one place it can be bound. Both read back their rules, in
// order, with an absent optional field null.
func TestAccSecurityGroupDataSourceTellsOwnedFromTenant(t *testing.T) {
	const cluster, group = "acc-sg-read-owner", "acc-sg-read"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(securityGroupType, securityGroupPath),
			checkGone(clusterType, clusterPath),
		),
		Steps: []resource.TestStep{{
			Config: vpcOwnerConfig(cluster) + securityGroupConfig(group, true) + vpcDefaultGroupConfig() + fmt.Sprintf(`
data %q "read" {
  id         = %q
  depends_on = [%s.test]
}
`, securityGroupType, group, securityGroupType),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(securityGroupDataAddr, "state", "active"),
				resource.TestCheckResourceAttr(securityGroupDataAddr, "rule_count", "3"),
				resource.TestCheckNoResourceAttr(securityGroupDataAddr, "owner_kind"),
				resource.TestCheckNoResourceAttr(securityGroupDataAddr, "owner_id"),
				// The tenant's rules, in the order they were written.
				resource.TestCheckResourceAttr(securityGroupDataAddr, "ingress.#", "2"),
				resource.TestCheckResourceAttr(securityGroupDataAddr, "ingress.0.name", "allow-ssh-from-office"),
				resource.TestCheckResourceAttr(securityGroupDataAddr, "ingress.0.source_prefix", "203.0.113.0/24"),
				resource.TestCheckResourceAttr(securityGroupDataAddr, "ingress.0.destination_port_range", "22"),
				resource.TestCheckNoResourceAttr(securityGroupDataAddr, "ingress.0.source_port_range"),
				resource.TestCheckResourceAttr(securityGroupDataAddr, "ingress.1.name", "allow-metrics"),
				resource.TestCheckResourceAttr(securityGroupDataAddr, "ingress.1.destination_port_range", "9090"),
				resource.TestCheckResourceAttr(securityGroupDataAddr, "egress.#", "1"),
				resource.TestCheckNoResourceAttr(securityGroupDataAddr, "egress.0.name"),
				resource.TestCheckResourceAttr(securityGroupDataAddr, "egress.0.protocol", "any"),
				resource.TestCheckResourceAttr(securityGroupDataAddr, "egress.0.action", "permit"),
				resource.TestCheckResourceAttr(securityGroupDataAddr, "egress.0.destination_prefix", "0.0.0.0/0"),

				resource.TestCheckResourceAttr(defaultGroupDataAddr, "owner_kind", "vpc"),
				resource.TestCheckResourceAttrWith(defaultGroupDataAddr, "owner_id", isUIDOf("vpc-"+cluster)),
				resource.TestCheckResourceAttrWith(defaultGroupDataAddr, "id", isDefaultGroupOf("vpc-"+cluster)),
				resource.TestMatchResourceAttr(defaultGroupDataAddr, "id", regexp.MustCompile(`^vpc-`+cluster+`-[0-9a-f]{8}-default$`)),
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "display_name", "Default (vpc-"+cluster+")"),
				resource.TestMatchResourceAttr(defaultGroupDataAddr, "description", regexp.MustCompile(`^Platform-managed default security group for VPC vpc-`+cluster+`\. `)),
				// The fake's VPC has one subnet prefix: a permit from it to
				// itself each way, and no allow-all egress.
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "rule_count", "2"),
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "ingress.#", "1"),
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "ingress.0.name", "intra-vpc-0"),
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "ingress.0.protocol", "any"),
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "ingress.0.source_prefix", "10.90.0.0/24"),
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "ingress.0.destination_prefix", "10.90.0.0/24"),
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "egress.#", "1"),
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "egress.0.name", "intra-vpc-egress-0"),
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "egress.0.source_prefix", "0.0.0.0/0"),
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "egress.0.destination_prefix", "10.90.0.0/24"),
			),
		}},
	})
}

// A VPC with no subnet prefixes has a default group with no rules. Both lists
// are read as empty, not null, so length() works on them.
func TestAccSecurityGroupDataSourceReadsAnEmptyDefaultGroup(t *testing.T) {
	const cluster = "acc-sg-read-no-subnets"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{{
			Config: vpcOwnerConfig(cluster) + vpcDefaultGroupConfig() + fmt.Sprintf(`
output "rules" {
  value = length(data.%s.vpc_default.ingress) + length(data.%s.vpc_default.egress)
}
`, securityGroupType, securityGroupType),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "owner_kind", "vpc"),
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "rule_count", "0"),
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "ingress.#", "0"),
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "egress.#", "0"),
				resource.TestCheckOutput("rules", "0"),
			),
		}},
	})
}

func TestAccVPCPeeringDataSource(t *testing.T) {
	const cluster, name = "acc-peer-read-owner", "acc-to-inference-read"
	const remote = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(peeringType, peeringPath),
			checkGone(clusterType, clusterPath),
		),
		Steps: []resource.TestStep{{
			Config: peeringConfig(cluster, name, remote) + fmt.Sprintf(`
data %q "read" {
  id         = %q
  vpc        = "vpc-%s"
  depends_on = [%s.test]
}
`, peeringType, name, cluster, peeringType),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(peeringDataAddr, "state", "active"),
				resource.TestCheckResourceAttr(peeringDataAddr, "remote_vpc", remote),
				resource.TestCheckResourceAttr(peeringDataAddr, "display_name", "acceptance"),
				resource.TestCheckResourceAttr(peeringDataAddr, "tags.%", "0"),
			),
		}},
	})
}

// The suite asks for two rows a page, so three subnets are two pages. A
// provider that read only the first page would report two.
func TestAccSubnetsDataSourceReadsEveryPage(t *testing.T) {
	const network = "acc-fabric-pages"
	subnetsType := provider.ResourceName("subnets")
	subnetsAddr := "data." + subnetsType + ".all"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(subnetType, subnetPath),
			checkGone(networkType, networkPath),
		),
		Steps: []resource.TestStep{{
			Config: evpnConfig(network, "acceptance") + fmt.Sprintf(`
resource %q "page" {
  count         = 3
  id            = "acc-sub-page-${count.index}"
  network       = %s.test.id
  address_pool  = "/v1/regions/us-texas-1/networking/address-pools/tenant"
  prefix_length = 24
}

data %q "all" {
  network    = %s.test.id
  depends_on = [%s.page]
}
`, subnetType, networkType, subnetsType, networkType, subnetType),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(subnetsAddr, "subnets.#", "3"),
				resource.TestCheckResourceAttr(subnetsAddr, "subnets.2.id", "acc-sub-page-2"),
				resource.TestCheckResourceAttr(subnetsAddr, "cidrs.#", "3"),
			),
		}},
	})
}
