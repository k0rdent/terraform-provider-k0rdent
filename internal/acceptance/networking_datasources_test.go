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

// A tenant group has no owner. A VPC's default group names the VPC that owns
// it, which is the one place it can be bound.
func TestAccSecurityGroupDataSourceTellsOwnedFromTenant(t *testing.T) {
	const cluster, group = "acc-sg-read-owner", "acc-sg-read"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(securityGroupType, securityGroupPath),
			checkGone(clusterType, clusterPath),
		),
		Steps: []resource.TestStep{{
			Config: vpcOwnerConfig(cluster) + securityGroupConfig(group, false) + fmt.Sprintf(`
data %q "read" {
  id         = %q
  depends_on = [%s.test]
}

data %q "vpc_default" {
  id         = "default"
  depends_on = [%s.test]
}
`, securityGroupType, group, securityGroupType, securityGroupType, clusterType),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(securityGroupDataAddr, "state", "active"),
				resource.TestCheckResourceAttr(securityGroupDataAddr, "rule_count", "2"),
				resource.TestCheckNoResourceAttr(securityGroupDataAddr, "owner_kind"),
				resource.TestCheckNoResourceAttr(securityGroupDataAddr, "owner_id"),
				resource.TestCheckResourceAttr(defaultGroupDataAddr, "owner_kind", "vpc"),
				resource.TestCheckResourceAttrSet(defaultGroupDataAddr, "owner_id"),
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
