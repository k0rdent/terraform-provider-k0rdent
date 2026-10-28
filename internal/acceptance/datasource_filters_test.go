package acceptance

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// The plural data sources' `tags`, `display_name_regex` and `state` filters,
// read through Terraform against the fake. Each test tags what it creates
// with a value no other test uses, because the suite shares one fake and a
// list of a project's networks holds every test's networks.

var (
	securityGroupsType = provider.ResourceName("security_groups")
	peeringsType       = provider.ResourceName("vpc_peerings")
)

func filterNetworkConfig(label, id, displayName, env string) string {
	return fmt.Sprintf(`
resource %q %q {
  id           = %q
  display_name = %q
  tags         = { suite = "acc-filters", env = %q }
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
    mtu          = 9000
  }
}
`, networkType, label, id, displayName, env)
}

// Three networks, one matching all three filters. Each of the other two
// fails exactly one of them, so a filter that did nothing would show up as a
// second row.
func TestAccNetworksDataSourceFiltersOnTagsNameAndState(t *testing.T) {
	list := "data." + networksType + ".prod"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(networkType, networkPath),
		Steps: []resource.TestStep{{
			Config: filterNetworkConfig("a", "acc-filter-net-a", "prod-a", "prod") +
				filterNetworkConfig("b", "acc-filter-net-b", "lab-b", "prod") +
				filterNetworkConfig("c", "acc-filter-net-c", "prod-c", "lab") + fmt.Sprintf(`
data %q "prod" {
  tags               = { suite = "acc-filters", env = "prod" }
  display_name_regex = "^prod-"
  state              = "active"
  depends_on         = [%s.a, %s.b, %s.c]
}

data %q "none" {
  tags       = { suite = "acc-filters" }
  state      = "failed"
  depends_on = [%s.a, %s.b, %s.c]
}
`, networksType, networkType, networkType, networkType, networksType, networkType, networkType, networkType),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(list, "networks.#", "1"),
				resource.TestCheckResourceAttr(list, "ids.#", "1"),
				resource.TestCheckResourceAttr(list, "ids.0", "acc-filter-net-a"),
				resource.TestCheckResourceAttr(list, "networks.0.display_name", "prod-a"),
				// Two: the configuration's own, and not the create token the
				// provider adds to find a create whose answer was lost.
				resource.TestCheckResourceAttr(list, "networks.0.tags.%", "2"),
				resource.TestCheckResourceAttr(list, "networks.0.tags.env", "prod"),
				resource.TestCheckResourceAttr(list, "networks.0.tags.suite", "acc-filters"),
				resource.TestCheckResourceAttr("data."+networksType+".none", "networks.#", "0"),
			),
		}},
	})
}

// `state` is the one filter the security group list takes itself, so this is
// the path that sends it. The other two still run in the provider. Each
// group but the kept one fails exactly one filter, so dropping any filter
// lets a second group through, and `failed` has to find none of them.
func TestAccSecurityGroupsDataSourceFiltersOnTagsNameAndState(t *testing.T) {
	list := "data." + securityGroupsType + ".mine"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(securityGroupType, securityGroupPath),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
resource %[1]q "kept" {
  id           = "acc-filter-sg-kept"
  display_name = "web-kept"
  tags         = { suite = "acc-filters-sg" }
%[3]s
}

# Fails only the tags filter.
resource %[1]q "other_tag" {
  id           = "acc-filter-sg-other-tag"
  display_name = "web-other-tag"
  tags         = { suite = "acc-filters-sg-other" }
%[3]s
}

# Fails only the regex.
resource %[1]q "other_name" {
  id           = "acc-filter-sg-other-name"
  display_name = "db-other-name"
  tags         = { suite = "acc-filters-sg" }
%[3]s
}

data %[2]q "mine" {
  tags               = { suite = "acc-filters-sg" }
  display_name_regex = "^web-"
  state              = "active"
  depends_on         = [%[1]s.kept, %[1]s.other_tag, %[1]s.other_name]
}

# Fails only the state filter.
data %[2]q "failed" {
  tags               = { suite = "acc-filters-sg" }
  display_name_regex = "^web-"
  state              = "failed"
  depends_on         = [%[1]s.kept, %[1]s.other_tag, %[1]s.other_name]
}
`, securityGroupType, securityGroupsType, oneEgressRule),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(list, "security_groups.#", "1"),
				resource.TestCheckResourceAttr(list, "ids.0", "acc-filter-sg-kept"),
				// One: the create token stays out of what's reported.
				resource.TestCheckResourceAttr(list, "security_groups.0.tags.%", "1"),
				resource.TestCheckResourceAttr(list, "security_groups.0.tags.suite", "acc-filters-sg"),
				resource.TestCheckResourceAttr(list, "security_groups.0.state", "active"),
				resource.TestCheckResourceAttr("data."+securityGroupsType+".failed", "security_groups.#", "0"),
			),
		}},
	})
}

// A peering carries tags and a display name too, and the list now reports
// both, so what the filter matched can be read back. Each peering but the
// tagged one fails exactly one filter, and `failed` has to find none.
func TestAccVPCPeeringsDataSourceFiltersOnTagsAndName(t *testing.T) {
	const cluster = "acc-filter-peer-owner"
	const remote = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-inference"
	const remote2 = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-serving"
	const remote3 = "/v1/regions/us-texas-1/projects/other/networking/vpcs/vpc-batch"
	list := "data." + peeringsType + ".tagged"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(peeringType, peeringPath),
			checkGone(clusterType, clusterPath),
		),
		Steps: []resource.TestStep{{
			Config: vpcOwnerConfig(cluster) + fmt.Sprintf(`
resource %[1]q "tagged" {
  id           = "acc-filter-peer-tagged"
  vpc          = "vpc-%[3]s"
  remote_vpc   = %[5]q
  display_name = "to-inference"
  tags         = { route = "inference" }
  depends_on   = [%[4]s.test]
}

# Fails only the tags filter.
resource %[1]q "plain" {
  id           = "acc-filter-peer-plain"
  vpc          = "vpc-%[3]s"
  remote_vpc   = %[6]q
  display_name = "to-inference-too"
  depends_on   = [%[4]s.test]
}

# Fails only the regex.
resource %[1]q "renamed" {
  id           = "acc-filter-peer-renamed"
  vpc          = "vpc-%[3]s"
  remote_vpc   = %[7]q
  display_name = "batch-link"
  tags         = { route = "inference" }
  depends_on   = [%[4]s.test]
}

data %[2]q "tagged" {
  vpc                = "vpc-%[3]s"
  tags               = { route = "inference" }
  display_name_regex = "^to-"
  state              = "active"
  depends_on         = [%[1]s.tagged, %[1]s.plain, %[1]s.renamed]
}

# Fails only the state filter.
data %[2]q "failed" {
  vpc                = "vpc-%[3]s"
  tags               = { route = "inference" }
  display_name_regex = "^to-"
  state              = "failed"
  depends_on         = [%[1]s.tagged, %[1]s.plain, %[1]s.renamed]
}
`, peeringType, peeringsType, cluster, clusterType, remote, remote2, remote3),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(list, "peerings.#", "1"),
				resource.TestCheckResourceAttr(list, "peerings.0.id", "acc-filter-peer-tagged"),
				resource.TestCheckResourceAttr(list, "peerings.0.display_name", "to-inference"),
				// One: the create token stays out of what's reported.
				resource.TestCheckResourceAttr(list, "peerings.0.tags.%", "1"),
				resource.TestCheckResourceAttr(list, "peerings.0.tags.route", "inference"),
				resource.TestCheckResourceAttr("data."+peeringsType+".failed", "peerings.#", "0"),
			),
		}},
	})
}

// The organizations come from the fake's fixture, so nothing is created:
// `org-empty` is the one whose display name is exactly "empty".
func TestAccOrganizationsDataSourceFiltersOnName(t *testing.T) {
	list := "data." + organizationsType + ".empty"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
data %q "empty" {
  display_name_regex = "^empty$"
}
`, organizationsType),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(list, "ids.#", "1"),
				resource.TestCheckResourceAttr(list, "ids.0", "org-empty"),
			),
		}},
	})
}

// Every host in the fake is running, so `stopped` keeps none of a group's
// hosts and `running` keeps them all. The state goes to the platform as
// `inStates`.
func TestAccInstancesDataSourceFiltersOnState(t *testing.T) {
	const group = "acc-filter-hosts"
	running, stopped := "data."+instancesType+".running", "data."+instancesType+".stopped"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{{
			Config: instanceGroupConfig(group, 2) + fmt.Sprintf(`
data %q "running" {
  instance_group = %s.test.id
  state          = "running"
}

data %q "stopped" {
  instance_group = %s.test.id
  state          = "stopped"
}
`, instancesType, instanceGroupType, instancesType, instanceGroupType),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(running, "instances.#", "2"),
				resource.TestCheckResourceAttr(stopped, "instances.#", "0"),
				resource.TestCheckResourceAttr(stopped, "ids.#", "0"),
			),
		}},
	})
}

// Replacing a group leaves a deleted record of each instance the old one
// held, which an ordinary read leaves out. `state = "deleted"` finds them,
// because the platform turns `showDeleted` on for that state. Shrinking the
// pool is the change that replaces, since the platform has no scale-down.
// The data sources depend on the group so they're read after the
// replacement, not at plan.
func TestAccInstancesDataSourceFindsDeletedRecords(t *testing.T) {
	const group = "acc-filter-released"
	deleted, running := "data."+instancesType+".deleted", "data."+instancesType+".running"
	reads := fmt.Sprintf(`
data %[1]q "deleted" {
  instance_group = %[2]s.test.id
  state          = "deleted"
  depends_on     = [%[2]s.test]
}

data %[1]q "running" {
  instance_group = %[2]s.test.id
  state          = "running"
  depends_on     = [%[2]s.test]
}
`, instancesType, instanceGroupType)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{
			{
				Config: instanceGroupConfig(group, 2) + reads,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(deleted, "instances.#", "0"),
					resource.TestCheckResourceAttr(running, "instances.#", "2"),
				),
			},
			{
				Config: permitReplace(instanceGroupConfig(group, 1), instanceGroupType) + reads,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(deleted, "instances.#", "2"),
					resource.TestCheckResourceAttr(deleted, "instances.0.state", "deleted"),
					resource.TestCheckResourceAttr(running, "instances.#", "1"),
				),
			},
		},
	})
}

// A pattern RE2 can't compile, a state the platform doesn't have, a tag
// filter with a null value, or asking for only deprecated types while
// excluding them, fails the plan on the attribute. Nothing is read: the step is plan-only and the
// fake would answer a read.
func TestAccDataSourceFiltersAreCheckedAtPlan(t *testing.T) {
	for name, tc := range map[string]struct{ config, want string }{
		"regex": {
			config: fmt.Sprintf("data %q \"bad\" {\n  display_name_regex = \"(prod\"\n}\n", vpcsType),
			want:   `Not a regular expression`,
		},
		"state": {
			config: fmt.Sprintf("data %q \"bad\" {\n  state = \"running\"\n}\n", vpcsType),
			want:   `value must be one of`,
		},
		"null tag": {
			config: fmt.Sprintf("data %q \"bad\" {\n  tags = { env = null }\n}\n", networksType),
			want:   `A tag filter value is null`,
		},
		"deprecated excluded": {
			config: fmt.Sprintf("data %q \"bad\" {\n  status             = \"deprecated\"\n  include_deprecated = false\n}\n",
				provider.ResourceName("cluster_types")),
			want: `Deprecated types excluded and asked for`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				Steps: []resource.TestStep{{
					Config:      tc.config,
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(tc.want),
				}},
			})
		})
	}
}
