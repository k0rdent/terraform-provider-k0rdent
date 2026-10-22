package acceptance

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// clusterGroupsConfig is two groups and a cluster on typeRef whose
// `security_groups` line is groups, written as HCL. An empty groups leaves the
// attribute out.
func clusterGroupsConfig(name, typeRef, groups string) string {
	line := ""
	if groups != "" {
		line = "security_groups = " + groups
	}
	return fmt.Sprintf(`
resource %[1]q "a" {
  id = "acc-cg-a"
  ingress {
    action             = "permit"
    protocol           = "tcp"
    source_prefix      = "203.0.113.0/24"
    destination_prefix = "0.0.0.0/0"

    destination_port_range = "6443"
  }
}

resource %[1]q "b" {
  id = "acc-cg-b"
  ingress {
    action             = "permit"
    protocol           = "tcp"
    source_prefix      = "2001:db8::/32"
    destination_prefix = "::/0"

    destination_port_range = "6443"
  }
}

resource %[2]q "test" {
  id           = %[3]q
  cluster_type = %[4]q
  %[5]s
}
`, securityGroupType, clusterType, name, typeRef, line)
}

var (
	groupsAB = fmt.Sprintf("[%[1]s.a.id, %[1]s.b.id]", securityGroupType)
	groupsBA = fmt.Sprintf("[%[1]s.b.id, %[1]s.a.id]", securityGroupType)
)

// The whole lifecycle of a cluster's own groups: set at create, reordered in
// place, left alone when the attribute is removed, detached with `[]`.
func TestAccClusterSecurityGroups(t *testing.T) {
	const name = "acc-cluster-groups"
	ab := clusterGroupsConfig(name, clusterTypeRef, groupsAB)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(clusterType, clusterPath),
			checkGone(securityGroupType, securityGroupPath),
		),
		Steps: []resource.TestStep{
			{
				Config: ab,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterAddr, "security_groups.#", "2"),
					resource.TestCheckResourceAttr(clusterAddr, "security_groups.0", "acc-cg-a"),
					resource.TestCheckResourceAttr(clusterAddr, "security_groups.1", "acc-cg-b"),
				),
			},
			{
				Config:           ab,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
			{
				// Order is precedence, so a reorder is a change, and it is
				// made in place.
				Config: clusterGroupsConfig(name, clusterTypeRef, groupsBA),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterAddr, "security_groups.0", "acc-cg-b"),
					resource.TestCheckResourceAttr(clusterAddr, "security_groups.1", "acc-cg-a"),
					resource.TestCheckResourceAttr(clusterAddr, "state", "active"),
				),
			},
			{
				// Removed from the configuration: nothing to do, and the
				// groups stay attached.
				Config:           clusterGroupsConfig(name, clusterTypeRef, ""),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
				Check:            resource.TestCheckResourceAttr(clusterAddr, "security_groups.#", "2"),
			},
			{
				Config: clusterGroupsConfig(name, clusterTypeRef, "[]"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.TestCheckResourceAttr(clusterAddr, "security_groups.#", "0"),
			},
			{
				Config:           clusterGroupsConfig(name, clusterTypeRef, "[]"),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}

// A cluster type whose node pools have no interface on a NICo-backed VPC
// can carry no group, and the platform's 422 reaches the user.
func TestAccClusterSecurityGroupsNeedAnInterface(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		// The refused create leaves no cluster in state, so only the groups
		// are checked.
		CheckDestroy: checkGone(securityGroupType, securityGroupPath),
		Steps: []resource.TestStep{{
			Config: clusterGroupsConfig("acc-cluster-no-nic",
				"/v1/regions/us-texas-1/compute/cluster-types/cpu-general", groupsAB),
			ExpectError: regexp.MustCompile(`no node pool has an interface`),
		}},
	})
}
