package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// CheckDestroy helpers.
//
// Terraform removes the state entry whether or not Delete reached the platform,
// so a Delete that quietly returned looks like a clean destroy. These read the
// object back. The framework calls them after the final destroy, with the state
// as it was just before, which is why the ids are still there.
//
// The VPC binding resource is the reason this matters more than usual: its
// destroy is a write, not a delete, so "the object is gone" is the wrong
// question and checkVPCCarriesOnlyDefault asks the right one.

// checkGone asserts that every resource of a type has stopped resolving, and
// that there was at least one. A check that matched nothing passes against any
// destroy, so a typo in the type or a step that dropped the resource before the
// end would make it vacuous.
//
// pathOf builds the read path from the resource's attributes, because each
// resource sits under a different parent and the state is the only place the
// parent's id survives the destroy.
//
// It asks the fake's store, not the API. Once a test's project is destroyed
// the API answers the gateway's 403 for everything that was in it, deleted or
// left behind, as v1.3.0-dev.4 does. The store still answers 200 for an object
// left behind, so the check is no looser.
func checkGone(resourceType string, pathOf func(attrs map[string]string) string) resource.TestCheckFunc {
	return func(state *terraform.State) error {
		matched := 0
		for name, rs := range state.RootModule().Resources {
			// A data source is not destroyed, and the default group one reads
			// outlives every test.
			if rs.Type != resourceType || strings.HasPrefix(name, "data.") {
				continue
			}
			matched++
			status, _, err := readFakeStore(pathOf(rs.Primary.Attributes))
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			if status != http.StatusNotFound {
				return fmt.Errorf("%s still resolves: the fake answered %d, wanted 404. "+
					"Terraform dropped the state entry, so this would have looked like a "+
					"clean destroy", name, status)
			}
		}
		if matched == 0 {
			return fmt.Errorf("no %s was in the state the destroy started from, so this "+
				"checked nothing", resourceType)
		}
		return nil
	}
}

// checkVPCCarriesOnlyDefault asserts a VPC is back on its platform-managed
// DEFAULT group, with none of the tenant's own still attached.
//
// This is the assertion behind the warning on a VPC-binding destroy plan.
// That destroy is a write, not a delete, so "the object is gone"
// is the wrong question: the right one is whether the filter it was managing
// reverted, and it is asked at a step where the VPC's owning cluster is still
// standing. After a full destroy the VPC has gone with its owner and there is
// nothing left to read.
func checkVPCCarriesOnlyDefault(vpc string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		status, body, err := readFake(fmt.Sprintf(
			"/v1/regions/us-texas-1/projects/demo/networking/vpcs/%s", vpc))
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("reading %s answered %d, so the reset cannot be checked", vpc, status)
		}

		var read struct {
			SecurityGroups []string `json:"securityGroups"`
		}
		if err := json.Unmarshal(body, &read); err != nil {
			return err
		}
		// The fake names a VPC's default group <VPC id>-default.
		if len(read.SecurityGroups) != 1 || read.SecurityGroups[0] != vpc+"-default" {
			return fmt.Errorf("%s carries %v, wanted only its own default group. The tenant's "+
				"groups are still enforced on a VPC Terraform has stopped tracking",
				vpc, read.SecurityGroups)
		}
		return nil
	}
}

func readFake(path string) (int, []byte, error) {
	token, err := mintFakeToken()
	if err != nil {
		return 0, nil, err
	}
	return getFake(path, token)
}

// readFakeStore reads an API path from the fake's own store, past the bearer
// check and the gateway, with the owner token.
func readFakeStore(path string) (int, []byte, error) {
	return getFake("/_harness/store"+path, fakeOwnerToken)
}

func getFake(path, bearer string) (int, []byte, error) {
	req, err := http.NewRequest(http.MethodGet, endpoint+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)

	resp, err := harnessHTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body := make([]byte, 0, 512)
	buf := make([]byte, 512)
	for {
		n, rerr := resp.Body.Read(buf)
		body = append(body, buf[:n]...)
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, body, nil
}

// The path builders. One per resource, kept together so the scope order each
// one needs is visible side by side.
func networkPath(a map[string]string) string {
	return fmt.Sprintf("/v1/regions/%s/projects/%s/networking/networks/%s",
		a["region"], a["project"], a["id"])
}

func subnetPath(a map[string]string) string {
	return fmt.Sprintf("/v1/regions/%s/projects/%s/networking/networks/%s/subnets/%s",
		a["region"], a["project"], a["network"], a["id"])
}

func securityGroupPath(a map[string]string) string {
	return fmt.Sprintf("/v1/regions/%s/projects/%s/networking/security-groups/%s",
		a["region"], a["project"], a["id"])
}

func peeringPath(a map[string]string) string {
	return fmt.Sprintf("/v1/regions/%s/projects/%s/networking/vpcs/%s/peerings/%s",
		a["region"], a["project"], a["vpc"], a["id"])
}

func clusterPath(a map[string]string) string {
	return fmt.Sprintf("/v1/regions/%s/projects/%s/compute/clusters/%s",
		a["region"], a["project"], a["id"])
}

func instanceGroupPath(a map[string]string) string {
	return fmt.Sprintf("/v1/regions/%s/projects/%s/compute/instance-groups/%s",
		a["region"], a["project"], a["id"])
}

// A project is not addressed by region and project, so its path is built from
// the organization instead.
func projectPath(a map[string]string) string {
	return fmt.Sprintf("/v1/regions/global/organizations/%s/projects/%s",
		a["organization_id"], a["id"])
}

func fileStorePath(a map[string]string) string {
	return fmt.Sprintf("/v1/regions/%s/projects/%s/storage/file-stores/%s",
		a["region"], a["project"], a["id"])
}

// A check that finds nothing to check has to fail too, or a CheckDestroy naming
// the wrong type passes every destroy. It needs no fake, because it fails before
// reading anything.
func TestCheckGoneFailsWhenItMatchesNothing(t *testing.T) {
	t.Parallel()

	empty := &terraform.State{Modules: []*terraform.ModuleState{{
		Path:      []string{"root"},
		Resources: map[string]*terraform.ResourceState{},
	}}}
	if err := checkGone(networkType, networkPath)(empty); err == nil {
		t.Fatal("checkGone passed against a state with no network in it")
	}
}

// A destroy that leaves the object behind has to fail this, or none of the
// above is worth having. Proved by pointing the check at a live object.
func TestCheckGoneFailsOnSomethingStillThere(t *testing.T) {
	const name = "acc-checkdestroy-proof"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config: evpnConfig(name, "acceptance"),
			Check: func(state *terraform.State) error {
				// The network exists at this point, so the check must object.
				err := checkGone(networkType, networkPath)(state)
				if err == nil {
					return fmt.Errorf("checkGone passed against a network that is still there, " +
						"so it would pass against a destroy that did nothing")
				}
				return nil
			},
		}},
	})
}
