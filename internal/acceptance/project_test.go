package acceptance

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

var (
	projectType     = provider.ResourceName("project")
	projectAddr     = projectType + ".test"
	projectDataAddr = "data." + projectType + ".read"
)

// The organization the fake serves. A project is created inside one and this
// provider cannot create one, so every configuration here has to name it.
const accOrg = "org-demo"

func projectConfig(id, displayName, description string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id              = %q
  organization_id = %q
  display_name    = %q
  description     = %q
}
`, projectType, id, accOrg, displayName, description)
}

// The project's own lifecycle, and the one import form it accepts.
//
// It is the only resource here whose scope is not region and project, so the
// parts worth asserting are the ones that differ: the id is the value the
// provider block's `project` takes, the import id can name the organization,
// and the destroy is the only operation that waits.
func TestAccProject(t *testing.T) {
	const id = "acc-project"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(projectType, projectPath),
		Steps: []resource.TestStep{
			{
				Config: projectConfig(id, "Acceptance", "created by the acceptance suite"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectAddr, "id", id),
					resource.TestCheckResourceAttr(projectAddr, "organization_id", accOrg),
					resource.TestCheckResourceAttr(projectAddr, "display_name", "Acceptance"),
					// `active` on the first read, with no waiter in between.
					// The contract declares no in-progress create state, so a
					// provider that waited for one would hang here.
					resource.TestCheckResourceAttr(projectAddr, "status", "active"),
					resource.TestCheckResourceAttrSet(projectAddr, "self_link"),
					// The provider's region, sent on the create and read back.
					resource.TestCheckResourceAttr(projectAddr, "region", "us-texas-1"),
					checkProjectField(id, "region", "us-texas-1"),
				),
			},
			{
				Config:   projectConfig(id, "Acceptance", "created by the acceptance suite"),
				PlanOnly: true,
			},
			{
				// Both mutable fields at once. Neither replaces the project.
				Config: projectConfig(id, "Renamed", "still the acceptance suite"),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectAddr, "display_name", "Renamed"),
					resource.TestCheckResourceAttr(projectAddr, "description", "still the acceptance suite"),
					resource.TestCheckResourceAttr(projectAddr, "id", id),
				),
			},
			{
				Config:   projectConfig(id, "Renamed", "still the acceptance suite"),
				PlanOnly: true,
			},
			{
				Config:                  projectConfig(id, "Renamed", "still the acceptance suite"),
				ResourceName:            projectAddr,
				ImportState:             true,
				ImportStateId:           accOrg + "/" + id,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"timeouts"},
			},
		},
	})
}

// A bare id imports a project when one organization holds it.
func TestAccProjectImportsByItsID(t *testing.T) {
	const id = "acc-project-import"

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(projectType, projectPath),
		Steps: []resource.TestStep{
			{Config: projectConfig(id, "Import", "imported by id")},
			{
				Config:                  projectConfig(id, "Import", "imported by id"),
				ResourceName:            projectAddr,
				ImportState:             true,
				ImportStateId:           id,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"timeouts"},
			},
		},
	})
}

// The platform answers both optional fields as "" when unset. Leaving them out,
// and later removing them, must both apply cleanly and plan empty after.
func TestAccProjectWithoutTheOptionalFields(t *testing.T) {
	const id = "acc-project-bare"
	bare := fmt.Sprintf(`
resource %q "test" {
  id              = %q
  organization_id = %q
}
`, projectType, id, accOrg)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(projectType, projectPath),
		Steps: []resource.TestStep{
			{
				Config: bare,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(projectAddr, "display_name"),
					resource.TestCheckNoResourceAttr(projectAddr, "description"),
				),
			},
			{Config: bare, PlanOnly: true},
			{Config: projectConfig(id, "Named", "described")},
			{
				Config: bare,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(projectAddr, "display_name"),
					resource.TestCheckNoResourceAttr(projectAddr, "description"),
					checkProjectField(id, "displayName", ""),
					checkProjectField(id, "description", ""),
				),
			},
			{Config: bare, PlanOnly: true},
		},
	})
}

// A project created without an id gets a generated `tf-` one, and another
// resource can be built inside it through its own `project` argument.
func TestAccProjectGeneratedIDCarriesAnotherResource(t *testing.T) {
	config := fmt.Sprintf(`
resource %q "test" {
  organization_id = %q
}

resource %q "test" {
  id      = "acc-in-generated-project"
  project = %s.test.id
  evpn = {
    service_type = "l3vpn"
    vlan_id      = "auto"
  }
}
`, projectType, accOrg, networkType, projectType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(projectType, projectPath),
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestMatchResourceAttr(projectAddr, "id", regexp.MustCompile(`^tf-[0-9a-f]{16}$`)),
					resource.TestCheckResourceAttrPair(networkAddr, "project", projectAddr, "id"),
				),
			},
			{Config: config, PlanOnly: true},
		},
	})
}

// A destroy that finds the project already `deleting`, as a timed-out earlier
// destroy leaves it, waits out that teardown instead of failing on the 409.
func TestAccProjectDestroyResumesATeardownAlreadyRunning(t *testing.T) {
	const id = "acc-project-resume"
	config := projectConfig(id, "Resume", "")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(projectType, projectPath),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					status, _, err := callFake(http.MethodDelete, projectPath(map[string]string{
						"organization_id": accOrg, "id": id,
					}))
					if err != nil || status != http.StatusNoContent {
						t.Fatalf("starting the teardown out of band: %d %v", status, err)
					}
				},
				Config:  config,
				Destroy: true,
			},
		},
	})
}

// checkProjectField reads a project straight from the fake.
func checkProjectField(id, field, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		status, body, err := readFake(projectPath(map[string]string{"organization_id": accOrg, "id": id}))
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("reading %s answered %d", id, status)
		}
		var got map[string]any
		if err := json.Unmarshal(body, &got); err != nil {
			return err
		}
		if got[field] != want {
			return fmt.Errorf("%s is %q upstream, want %q", field, got[field], want)
		}
		return nil
	}
}

// The singular data source reads a project this configuration did not create.
//
// The seeded project stands in for one another state boundary owns: nothing in
// this configuration manages it, and the read still has to return a usable
// object. `created_at` is asserted because it is the one field the data source
// carries that the resource does not, so a regression that dropped it would
// otherwise be invisible.
func TestAccProjectDataSource(t *testing.T) {
	config := fmt.Sprintf(`
data %q "read" {
  id              = "demo"
  organization_id = %q
}
`, projectType, accOrg)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectDataAddr, "id", "demo"),
					resource.TestCheckResourceAttr(projectDataAddr, "organization_id", accOrg),
					resource.TestCheckResourceAttr(projectDataAddr, "display_name", "Demo"),
					resource.TestCheckResourceAttr(projectDataAddr, "status", "active"),
					resource.TestCheckResourceAttr(projectDataAddr, "region", "us-texas-1"),
					resource.TestCheckResourceAttrSet(projectDataAddr, "created_at"),
					resource.TestCheckResourceAttrSet(projectDataAddr, "self_link"),
					// The platform answers an unset description as "", which reads as null.
					resource.TestCheckNoResourceAttr(projectDataAddr, "description"),
				),
			},
		},
	})
}

// Without an organization the project is found by its id, which is unique
// across organizations.
func TestAccProjectDataSourceFindsTheOrganization(t *testing.T) {
	config := fmt.Sprintf(`
data %q "read" {
  id = "demo"
}
`, projectType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectDataAddr, "organization_id", accOrg),
					resource.TestCheckResourceAttr(projectDataAddr, "status", "active"),
				),
			},
		},
	})
}

// A data source naming a project that is not there fails the run.
//
// This is the difference between the data source and the resource's Read, which
// treats the same 404 as drift and removes the entry from state. A configuration
// that names something absent is broken, and reporting it as an empty read would
// hand the error to whatever referenced it instead.
func TestAccProjectDataSourceFailsOnOneThatIsNotThere(t *testing.T) {
	config := fmt.Sprintf(`
data %q "read" {
  id              = "acc-no-such-project"
  organization_id = %q
}
`, projectType, accOrg)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile(`(?s)404 Not Found.*not\s+found:\s+project\s+"acc-no-such-project"`),
			},
		},
	})
}

// Naming an organization that doesn't exist fails the run with the gateway's
// 403, as the k0rdent API v1.3.0-dev.4 answered it: the caller holds no role
// there, so no service is asked and nothing says whether the project exists.
// The provider has to pass the upstream code through.
func TestAccProjectDataSourceFailsOnAnUnknownOrganization(t *testing.T) {
	config := fmt.Sprintf(`
data %q "read" {
  id              = "demo"
  organization_id = %q
}
`, projectType, "org-not-ours")

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{
				Config:      config,
				ExpectError: regexp.MustCompile(`(?s)403 Forbidden.*forbidden`),
			},
		},
	})
}

// An identifier that would rewrite the request URL is refused before anything
// is sent.
//
// The unit test beside the validator proves the rule. This proves it is wired
// to the attributes, which is the half a unit test cannot reach: a validator
// nobody attached passes its own tests forever. Both halves are needed, and the
// resource and the data source are asserted separately because they are two
// schemas and attaching it to one is the likely mistake.
func TestAccProjectRefusesAnIdentifierThatRewritesTheURL(t *testing.T) {
	cases := []struct{ name, config string }{
		{"the resource's organization", fmt.Sprintf(`
resource %q "test" {
  id              = "acc-x"
  organization_id = ".."
}
`, projectType)},
		{"the resource's id", fmt.Sprintf(`
resource %q "test" {
  id              = ".."
  organization_id = %q
}
`, projectType, accOrg)},
		{"the data source's organization", fmt.Sprintf(`
data %q "read" {
  id              = "demo"
  organization_id = ".."
}
`, projectType)},
		{"the data source's id", fmt.Sprintf(`
data %q "read" {
  id              = ".."
  organization_id = %q
}
`, projectType, accOrg)},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				Steps: []resource.TestStep{{
					Config:      c.config,
					PlanOnly:    true,
					ExpectError: regexp.MustCompile(`Not a valid identifier`),
				}},
			})
		})
	}
}

// callFake sends one request to the fake with a valid token.
func callFake(method, path string) (int, []byte, error) {
	token, err := mintFakeToken()
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequest(method, endpoint+path, nil)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := harnessHTTP.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}

// A project is created in a region that hosts projects. One that
// doesn't is refused at plan, naming the ones that do, rather than at apply
// with the platform's 422.
func TestAccProjectRegionThatHostsNoProjectsIsRefusedAtPlan(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
resource %q "test" {
  id              = "acc-project-global"
  organization_id = %q
  region          = "global"
}
`, projectType, accOrg),
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`(?s)can't hold a project.*us-texas-1`),
		}},
	})
}

// A project made before regions were required reads back with no region. It
// imports and plans empty: the provider's region isn't planned onto it, which
// would be a change no apply can make.
func TestAccProjectWithNoRegionPlansEmpty(t *testing.T) {
	config := fmt.Sprintf(`
resource %q "test" {
  id              = "before-regions"
  organization_id = %q
  display_name    = "Before regions"
}
`, projectType, accOrg)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{
				Config:             config,
				ResourceName:       projectAddr,
				ImportState:        true,
				ImportStateId:      accOrg + "/before-regions",
				ImportStatePersist: true,
			},
			{
				Config:   config,
				PlanOnly: true,
				Check:    resource.TestCheckNoResourceAttr(projectAddr, "region"),
			},
			{
				// Its region can't be set afterwards, and an update that
				// tried would fail on every apply, so the plan refuses it.
				Config: strings.Replace(config, `display_name    = "Before regions"`,
					`display_name    = "Before regions"`+"\n  region          = \"us-texas-1\"", 1),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile("(?s)has no region to set.*remove\\s+`region`"),
			},
			{
				// Destroying the seeded project would take it from every
				// later test, so it leaves state without being deleted.
				Config: fmt.Sprintf(`
removed {
  from = %s
  lifecycle {
    destroy = false
  }
}
`, projectAddr),
			},
		},
	})
}

// Every region says whether it hosts projects, and project_region_ids is the
// ones that do: the values a project's region takes.
func TestAccRegionsSayWhichHostProjects(t *testing.T) {
	regions := provider.ResourceName("regions")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`data %q "all" {}`, regions),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data."+regions+".all", "ids.#", "1"),
					resource.TestCheckResourceAttr("data."+regions+".all", "regions.0.id", "us-texas-1"),
					resource.TestCheckResourceAttr("data."+regions+".all", "regions.0.hosts_projects", "true"),
					resource.TestCheckResourceAttr("data."+regions+".all", "project_region_ids.#", "1"),
					resource.TestCheckResourceAttr("data."+regions+".all", "project_region_ids.0", "us-texas-1"),
				),
			},
			{
				Config: fmt.Sprintf(`data %q "all" { include_global = true }`, regions),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data."+regions+".all", "regions.0.id", "global"),
					resource.TestCheckResourceAttr("data."+regions+".all", "regions.0.hosts_projects", "false"),
					resource.TestCheckResourceAttr("data."+regions+".all", "project_region_ids.#", "1"),
				),
			},
		},
	})
}
