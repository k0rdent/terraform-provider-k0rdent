package acceptance

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// A change that would replace a resource holding tenant
// state fails the plan unless the configuration sets `allow_replace = true`,
// and a change the platform makes in place is never refused. Each test below
// makes the same change three times: refused, permitted, and then with the
// permission removed again, which has to be an in-place update.

// permitReplace adds `allow_replace = true` to the resource block named
// `<resourceType>.test`.
func permitReplace(config, resourceType string) string {
	open := fmt.Sprintf("resource %q \"test\" {\n", resourceType)
	if !strings.Contains(config, open) {
		panic("no " + resourceType + ".test block in the configuration")
	}
	return strings.Replace(config, open, open+"  allow_replace = true\n", 1)
}

// refusal matches the guard's message for one attribute of one resource: the
// attribute, and how to permit the replacement, so a reader can act on it.
func refusal(attribute, noun string) *regexp.Regexp {
	return regexp.MustCompile("(?s)Changing `" + regexp.QuoteMeta(attribute) + "` would replace this " + noun +
		".*add\\s+`allow_replace\\s+=\\s+true`\\s+to\\s+this\\s+resource")
}

func expect(addr string, action plancheck.ResourceActionType) resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectResourceAction(addr, action)}}
}

// The subnet is the case the address-allocation concern is about: the API
// has no update for one, so every attribute replaces it and a replacement can
// be allocated a different CIDR. A changed description is the mildest edit that
// does that.
func TestAccSubnetReplacementNeedsThePermission(t *testing.T) {
	const network, name = "acc-guard-net", "acc-guard-sub"
	before := subnetConfig(network, name)
	after := strings.Replace(before, "prefix_length    = 24", "prefix_length    = 24\n  description      = \"renamed\"", 1)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(subnetType, subnetPath),
			checkGone(networkType, networkPath),
		),
		Steps: []resource.TestStep{
			{Config: before},
			{
				Config:      after,
				PlanOnly:    true,
				ExpectError: refusal("description", "subnet"),
			},
			{
				Config:           permitReplace(after, subnetType),
				ConfigPlanChecks: expect(subnetAddr, plancheck.ResourceActionReplace),
				Check:            resource.TestCheckResourceAttr(subnetAddr, "description", "renamed"),
			},
			{
				// Removing the permission is an update with no request behind
				// it, and the guard is back for the next change.
				Config:           after,
				ConfigPlanChecks: expect(subnetAddr, plancheck.ResourceActionUpdate),
				Check:            resource.TestCheckNoResourceAttr(subnetAddr, "allow_replace"),
			},
			{Config: after, PlanOnly: true},
		},
	})
}

// The address allocation is what a replaced subnet can lose, so the refusal
// has to hold for the attribute that sizes it as well.
func TestAccSubnetPrefixChangeIsRefused(t *testing.T) {
	const network, name = "acc-guard-prefix-net", "acc-guard-prefix"
	before := subnetConfig(network, name)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(subnetType, subnetPath),
			checkGone(networkType, networkPath),
		),
		Steps: []resource.TestStep{
			{Config: before},
			{
				Config:      strings.Replace(before, "prefix_length    = 24", "prefix_length    = 25", 1),
				PlanOnly:    true,
				ExpectError: refusal("prefix_length", "subnet"),
			},
		},
	})
}

func TestAccClusterReplacementNeedsThePermission(t *testing.T) {
	const name, renamed = "acc-guard-cluster", "acc-guard-cluster-2"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{Config: clusterConfig(name, 1)},
			{
				Config:      clusterConfig(renamed, 1),
				PlanOnly:    true,
				ExpectError: refusal("id", "cluster"),
			},
			{
				// A change the platform makes in place is not refused.
				Config:           strings.Replace(clusterConfig(name, 2), `"acceptance"`, `"renamed"`, 1),
				ConfigPlanChecks: expect(clusterAddr, plancheck.ResourceActionUpdate),
				Check:            resource.TestCheckResourceAttr(clusterAddr, "display_name", "renamed"),
			},
			{
				Config:           permitReplace(clusterConfig(renamed, 1), clusterType),
				ConfigPlanChecks: expect(clusterAddr, plancheck.ResourceActionReplace),
				Check:            resource.TestCheckResourceAttr(clusterAddr, "id", renamed),
			},
		},
	})
}

// With the id left to the platform it is computed, and Terraform marks it
// unknown on any update before the plan modifiers put the prior value back.
// The id's RequiresReplace sees that unknown first, so a guard that trusted it
// would refuse every in-place change to such a network. A cluster and an
// instance group take their id from configuration, so a network stands in.
func TestAccNetworkWithAGeneratedIDChangesInPlace(t *testing.T) {
	config := func(displayName string) string {
		return fmt.Sprintf(`
resource %q "test" {
  display_name = %q
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
  }
}
`, networkType, displayName)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(networkType, networkPath),
		Steps: []resource.TestStep{
			{Config: config("first")},
			{
				Config:           config("second"),
				ConfigPlanChecks: expect(networkAddr, plancheck.ResourceActionUpdate),
				Check:            resource.TestCheckResourceAttr(networkAddr, "display_name", "second"),
			},
		},
	})
}

// Growing a pool scales in place. Shrinking one is a replacement, because the
// API scales an instance group up only.
func TestAccInstanceGroupShrinkNeedsThePermission(t *testing.T) {
	const name = "acc-guard-hosts"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{
			{Config: instanceGroupConfig(name, 1)},
			{
				Config:           instanceGroupConfig(name, 2),
				ConfigPlanChecks: expect(instanceGroupAddr, plancheck.ResourceActionUpdate),
			},
			{
				Config:      instanceGroupConfig(name, 1),
				PlanOnly:    true,
				ExpectError: refusal("node_pool", "instance group"),
			},
			{
				Config:           permitReplace(instanceGroupConfig(name, 1), instanceGroupType),
				ConfigPlanChecks: expect(instanceGroupAddr, plancheck.ResourceActionReplace),
				Check:            resource.TestCheckResourceAttr(instanceGroupAddr, "node_pool.gpu-workers.size", "1"),
			},
		},
	})
}

func TestAccNetworkReplacementNeedsThePermission(t *testing.T) {
	const name = "acc-guard-fabric"
	ib := fmt.Sprintf(`
resource %q "test" {
  id = %q
  infiniband = {
    partition = "training"
  }
}
`, networkType, name)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(networkType, networkPath),
		Steps: []resource.TestStep{
			{Config: evpnConfig(name, "acceptance")},
			{
				Config:      ib,
				PlanOnly:    true,
				ExpectError: refusal("evpn", "network"),
			},
			{
				Config:           evpnConfig(name, "renamed"),
				ConfigPlanChecks: expect(networkAddr, plancheck.ResourceActionUpdate),
			},
		},
	})
}

func TestAccProjectReplacementNeedsThePermission(t *testing.T) {
	const id, renamed = "acc-guard-project", "acc-guard-project-2"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(projectType, projectPath),
		Steps: []resource.TestStep{
			{Config: projectConfig(id, "Acceptance", "guarded")},
			{
				Config:      projectConfig(renamed, "Acceptance", "guarded"),
				PlanOnly:    true,
				ExpectError: refusal("id", "project"),
			},
			{
				Config:           projectConfig(id, "Renamed", "guarded"),
				ConfigPlanChecks: expect(projectAddr, plancheck.ResourceActionUpdate),
			},
			{
				Config:           permitReplace(projectConfig(renamed, "Renamed", "guarded"), projectType),
				ConfigPlanChecks: expect(projectAddr, plancheck.ResourceActionReplace),
				Check:            resource.TestCheckResourceAttr(projectAddr, "id", renamed),
			},
		},
	})
}

// A resource that holds no tenant state is replaced without asking, as
// before: the guard is not a blanket refusal.
func TestAccSecurityGroupReplacesWithoutAsking(t *testing.T) {
	config := func(id string) string {
		return fmt.Sprintf(`
resource %q "test" {
  id = %q
  egress {
    action             = "permit"
    protocol           = "any"
    source_prefix      = "0.0.0.0/0"
    destination_prefix = "0.0.0.0/0"
  }
}
`, securityGroupType, id)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{Config: config("acc-guard-sg")},
			{
				Config:           config("acc-guard-sg-2"),
				ConfigPlanChecks: expect(securityGroupType+".test", plancheck.ResourceActionReplace),
			},
		},
	})
}
