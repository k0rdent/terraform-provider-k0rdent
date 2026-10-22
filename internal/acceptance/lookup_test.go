package acceptance

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// The two values the hardcoded values guide listed until a data source read
// them: the organization a first project goes in, and a member machine's
// interface on a file store grant. Each test takes the value from the data
// source and hands it to the resource that used to need it as a literal.

var (
	organizationsType = provider.ResourceName("organizations")
	organizationType  = provider.ResourceName("organization")
	instancesType     = provider.ResourceName("instances")
	instanceType      = provider.ResourceName("instance")
)

// `org-empty` holds no project, so `projects` cannot report it. The list
// does, at one row a page, and the project goes in it by the id read there.
func TestAccOrganizationsGiveTheFirstProjectItsOrganization(t *testing.T) {
	const id = "acc-first-project"
	list, one := "data."+organizationsType+".all", "data."+organizationType+".empty"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(projectType, projectPath),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
data %q "all" {}

data %q "empty" {
  id = [for o in data.%s.all.organizations : o.id if o.display_name == "empty"][0]
}

resource %q "test" {
  id              = %q
  organization_id = data.%s.empty.id
}
`, organizationsType, organizationType, organizationsType, projectType, id, organizationType),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(list, "ids.#", "2"),
				resource.TestCheckResourceAttr(list, "ids.0", "org-demo"),
				resource.TestCheckResourceAttr(list, "ids.1", "org-empty"),
				resource.TestCheckResourceAttr(list, "organizations.1.status", "active"),
				resource.TestCheckResourceAttr(one, "status", "active"),
				resource.TestCheckResourceAttr(one, "self_link", "/v1/regions/global/organizations/org-empty"),
				resource.TestCheckResourceAttr(projectAddr, "organization_id", "org-empty"),
			),
		}},
	})
}

// A filter on status reaches the platform: nothing in the fake is suspended.
func TestAccOrganizationsFilterOnStatus(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
data %q "suspended" {
  status = "suspended"
}
`, organizationsType),
			Check: resource.TestCheckResourceAttr("data."+organizationsType+".suspended", "ids.#", "0"),
		}},
	})
}

// The other half of the test below: a literal that names no member's
// interface is refused, so that test's pass rests on the value it read.
func TestAccAFileStoreGrantNarrowedToNoMemberIsRefused(t *testing.T) {
	const group = "acc-narrowed-literal"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		// The store is refused, so only the group reaches state.
		CheckDestroy: checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{{
			Config: instanceGroupConfig(group, 1) + fmt.Sprintf(`
resource %q "test" {
  id           = "acc-narrowed-literal-store"
  share_type   = "general"
  capacity_gib = 512

  consumers = {
    (%s.test.id) = {
      type    = "instanceGroup"
      network = { type = "ethernet", mac_address = "02:00:00:00:00:00" }
    }
  }
}
`, fileStoreType, instanceGroupType),
			ExpectError: regexp.MustCompile(`matches no interface`),
		}},
	})
}

// The MAC address a grant narrows to comes from the instances the group holds,
// and the grant resolves to that one host's address. The fake refuses a
// narrowed selector that matches no member's interface, as the contract says
// the platform does, so a value that did not come from the group fails here.
func TestAccInstancesNarrowAFileStoreGrant(t *testing.T) {
	const group, store = "acc-narrowed", "acc-narrowed-store"
	list, one := "data."+instancesType+".group", "data."+instanceType+".first"
	status := "consumer_status." + group + "."

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(fileStoreType, fileStorePath),
		Steps: []resource.TestStep{{
			Config: instanceGroupConfig(group, 2) + fmt.Sprintf(`
data %q "group" {
  instance_group = %s.test.id
  depends_on     = [%s.test]
}

data %q "first" {
  id = data.%s.group.ids[0]
}

locals {
  first_mac = [for nic in data.%s.first.network_interfaces : nic.mac_address if nic.type == "ethernet"][0]
}

resource %q "test" {
  id           = %q
  share_type   = "general"
  capacity_gib = 512

  consumers = {
    (%s.test.id) = {
      type    = "instanceGroup"
      network = { type = "ethernet", mac_address = local.first_mac }
    }
  }
}
`, instancesType, instanceGroupType, instanceGroupType,
				instanceType, instancesType,
				instanceType,
				fileStoreType, store, instanceGroupType),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(list, "instances.#", "2"),
				resource.TestCheckResourceAttr(list, "instances.0.instance_group", group),
				resource.TestCheckResourceAttr(list, "instances.0.state", "running"),
				resource.TestCheckResourceAttr(list, "instances.0.network_interfaces.#", "2"),
				resource.TestCheckResourceAttr(list, "instances.0.network_interfaces.0.type", "ethernet"),
				resource.TestCheckResourceAttrSet(list, "instances.0.network_interfaces.0.mac_address"),
				resource.TestCheckNoResourceAttr(list, "instances.0.network_interfaces.0.guid"),
				resource.TestCheckResourceAttr(list, "instances.0.network_interfaces.1.type", "infiniband"),
				resource.TestCheckResourceAttrSet(list, "instances.0.network_interfaces.1.guid"),
				resource.TestCheckResourceAttrPair(one, "id", list, "instances.0.id"),
				resource.TestCheckResourceAttrPair(one, "network_interfaces.0.mac_address",
					list, "instances.0.network_interfaces.0.mac_address"),
				resource.TestCheckResourceAttrPair(fileStoreAddr, "consumers."+group+".network.mac_address",
					one, "network_interfaces.0.mac_address"),
				resource.TestCheckResourceAttr(fileStoreAddr, status+"state", "active"),
				resource.TestCheckResourceAttr(fileStoreAddr, status+"resolved_addresses.#", "1"),
				resource.TestCheckResourceAttrPair(fileStoreAddr, status+"resolved_addresses.0",
					one, "network_interfaces.0.ip_addresses.0"),
			),
		}},
	})
}
