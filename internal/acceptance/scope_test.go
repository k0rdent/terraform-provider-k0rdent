package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

var networksType = provider.ResourceName("networks")

// Two projects, one network id in each, and every request has to reach the
// project its configuration names. The fake answers a request for another
// project exactly as for an object that does not exist, so a provider that
// sent the provider block's project, or either one of these, to both would
// read, list or rename the wrong network and fail here.
func TestAccTheSameIDInTwoProjectsStaysApart(t *testing.T) {
	const network = "acc-same-network"
	config := func(displayA string) string {
		return fmt.Sprintf(`
resource %[1]q "a" {
  id              = "acc-scope-a"
  organization_id = %[2]q
}

resource %[1]q "b" {
  id              = "acc-scope-b"
  organization_id = %[2]q
}

resource %[3]q "a" {
  id           = %[4]q
  project      = %[1]s.a.id
  display_name = %[5]q
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
  }
}

resource %[3]q "b" {
  id           = %[4]q
  project      = %[1]s.b.id
  display_name = "in b"
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
  }
}

data %[6]q "a" {
  project    = %[1]s.a.id
  depends_on = [%[3]s.a]
}

data %[6]q "b" {
  project    = %[1]s.b.id
  depends_on = [%[3]s.b]
}
`, projectType, accOrg, networkType, network, displayA, networksType)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(networkType, networkPath),
			checkGone(projectType, projectPath),
		),
		Steps: []resource.TestStep{
			{
				Config: config("in a"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(networkType+".a", "project", "acc-scope-a"),
					resource.TestCheckResourceAttr(networkType+".a", "display_name", "in a"),
					resource.TestCheckResourceAttr(networkType+".b", "project", "acc-scope-b"),
					resource.TestCheckResourceAttr(networkType+".b", "display_name", "in b"),
					resource.TestCheckResourceAttr("data."+networksType+".a", "networks.#", "1"),
					resource.TestCheckResourceAttr("data."+networksType+".a", "networks.0.display_name", "in a"),
					resource.TestCheckResourceAttr("data."+networksType+".b", "networks.#", "1"),
					resource.TestCheckResourceAttr("data."+networksType+".b", "networks.0.display_name", "in b"),
				),
			},
			{Config: config("in a"), PlanOnly: true},
			{
				// The rename has to land in a and leave b alone.
				Config: config("renamed in a"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(networkType+".a", "display_name", "renamed in a"),
					checkFakeDisplayName(networkType+".a", networkPath, "renamed in a"),
					checkFakeDisplayName(networkType+".b", networkPath, "in b"),
				),
			},
		},
	})
}

// The same for a cluster, the object most of the provider's project-scoped
// operations act on.
func TestAccTheSameClusterIDInTwoProjectsStaysApart(t *testing.T) {
	const cluster = "acc-same-cluster"
	config := func(displayA string) string {
		return fmt.Sprintf(`
resource %[1]q "a" {
  id              = "acc-scope-cluster-a"
  organization_id = %[2]q
}

resource %[1]q "b" {
  id              = "acc-scope-cluster-b"
  organization_id = %[2]q
}

resource %[3]q "a" {
  id           = %[4]q
  project      = %[1]s.a.id
  cluster_type = %[5]q
  display_name = %[6]q
  node_pool = {
    "gpu-workers" = { node_count = 1 }
  }
}

resource %[3]q "b" {
  id           = %[4]q
  project      = %[1]s.b.id
  cluster_type = %[5]q
  display_name = "in b"
  node_pool = {
    "gpu-workers" = { node_count = 1 }
  }
}
`, projectType, accOrg, clusterType, cluster, clusterTypeRef, displayA)
	}

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(clusterType, clusterPath),
			checkGone(projectType, projectPath),
		),
		Steps: []resource.TestStep{
			{
				Config: config("in a"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterType+".a", "display_name", "in a"),
					resource.TestCheckResourceAttr(clusterType+".b", "display_name", "in b"),
				),
			},
			{Config: config("in a"), PlanOnly: true},
			{
				Config: config("renamed in a"),
				Check: resource.ComposeAggregateTestCheckFunc(
					checkFakeDisplayName(clusterType+".a", clusterPath, "renamed in a"),
					checkFakeDisplayName(clusterType+".b", clusterPath, "in b"),
				),
			},
		},
	})
}

// checkFakeDisplayName reads the object behind a resource straight from the
// fake, at the path its own project names.
func checkFakeDisplayName(addr string, pathOf func(map[string]string) string, want string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		rs, ok := state.RootModule().Resources[addr]
		if !ok {
			return fmt.Errorf("%s is not in state", addr)
		}
		status, body, err := readFake(pathOf(rs.Primary.Attributes))
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("%s: the fake answered %d", addr, status)
		}
		var got struct {
			DisplayName string `json:"displayName"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			return err
		}
		if got.DisplayName != want {
			return fmt.Errorf("%s: the fake holds display name %q, want %q", addr, got.DisplayName, want)
		}
		return nil
	}
}
