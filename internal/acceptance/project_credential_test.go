package acceptance

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

// A credential bound to one project, as the platform gives a tenant who has
// no role on the organization. The platform checks every read under
// /organizations/{orgId} against the organization, where a role on a project
// doesn't count, so it refuses such a credential its own project's read by id
// with a bare 403. The fake's project credential is refused the same way.
//
// t.Setenv keeps these tests off t.Parallel, so nothing else runs while the
// provider signs in as the project credential.

// useProjectCredential signs the provider in as the fake's project credential
// for the rest of the test.
func useProjectCredential(t *testing.T) {
	t.Helper()
	if fakeProjectCredential.clientID == "" {
		t.Skip("needs the fake, under TF_ACC")
	}
	t.Setenv(provider.EnvVar("CLIENT_ID"), fakeProjectCredential.clientID)
	t.Setenv(provider.EnvVar("CLIENT_SECRET"), fakeProjectCredential.clientSecret)
}

// saying matches each phrase in order, whatever Terraform wraps them at, after
// the platform's own 403.
func saying(phrases ...string) *regexp.Regexp {
	parts := []string{`403 Forbidden`, `forbidden`}
	for _, p := range phrases {
		parts = append(parts, strings.Join(strings.Fields(regexp.QuoteMeta(p)), `\s+`))
	}
	return regexp.MustCompile(`(?s)` + strings.Join(parts, `.*`))
}

// demoInOrg is the read by id the project credential is refused.
const demoInOrg = "/v1/regions/global/organizations/" + accOrg + "/projects/demo"

// checkProjectReadByID fails unless the provider sent want reads by id of
// `demo` since before was taken.
func checkProjectReadByID(before *int, want int) resource.TestCheckFunc {
	return func(*terraform.State) error {
		n, err := fakeServed(http.MethodGet, demoInOrg)
		if err != nil {
			return err
		}
		if got := n - *before; got != want {
			return fmt.Errorf("the provider sent %d reads of %s, want %d", got, demoInOrg, want)
		}
		return nil
	}
}

// Without `organization_id` the project is the row the project list has for
// it, so the read by id the platform would refuse is never sent, and every
// attribute is still set.
func TestAccProjectDataSourceWithAProjectCredential(t *testing.T) {
	useProjectCredential(t)
	before, err := fakeServed(http.MethodGet, demoInOrg)
	if err != nil {
		t.Fatal(err)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf("data %q \"read\" {\n  id = \"demo\"\n}\n", projectType),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectDataAddr, "id", "demo"),
					resource.TestCheckResourceAttr(projectDataAddr, "organization_id", accOrg),
					resource.TestCheckResourceAttr(projectDataAddr, "display_name", "Demo"),
					resource.TestCheckNoResourceAttr(projectDataAddr, "description"),
					resource.TestCheckResourceAttr(projectDataAddr, "status", "active"),
					resource.TestCheckResourceAttr(projectDataAddr, "region", "us-texas-1"),
					resource.TestCheckResourceAttr(projectDataAddr, "created_at", "2026-01-01T00:00:00Z"),
					resource.TestCheckResourceAttr(projectDataAddr, "self_link",
						"/v1/regions/global/organizations/"+accOrg+"/projects/demo"),
					checkProjectReadByID(&before, 0),
				),
			},
		},
	})
}

// With `organization_id` the read by id is tried first, and its 403 falls back
// to the project list filtered to that organization and id.
func TestAccProjectDataSourceWithAProjectCredentialAndItsOrganization(t *testing.T) {
	useProjectCredential(t)
	before, err := fakeServed(http.MethodGet, demoInOrg)
	if err != nil {
		t.Fatal(err)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf("data %q \"read\" {\n  id              = \"demo\"\n  organization_id = %q\n}\n",
					projectType, accOrg),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(projectDataAddr, "organization_id", accOrg),
					resource.TestCheckResourceAttr(projectDataAddr, "display_name", "Demo"),
					resource.TestCheckResourceAttr(projectDataAddr, "status", "active"),
					resource.TestCheckResourceAttr(projectDataAddr, "region", "us-texas-1"),
					resource.TestCheckResourceAttr(projectDataAddr, "created_at", "2026-01-01T00:00:00Z"),
					resource.TestCheckResourceAttrSet(projectDataAddr, "self_link"),
					// The read by id was sent, and refused, before the list.
					func(*terraform.State) error {
						n, err := fakeServed(http.MethodGet, demoInOrg)
						if err != nil {
							return err
						}
						if n == before {
							return fmt.Errorf("the provider never sent the read by id")
						}
						return nil
					},
				),
			},
		},
	})
}

// A project the list doesn't show the credential in that organization keeps
// the platform's 403, with what the read by id needs.
func TestAccProjectDataSourceWithAProjectCredentialRefusesAnotherProject(t *testing.T) {
	useProjectCredential(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf("data %q \"read\" {\n  id              = \"example\"\n  organization_id = %q\n}\n",
					projectType, accOrg),
				ExpectError: saying(`The project list doesn't show "example" in organization "org-demo" either`,
					"organizations-viewer or compute-viewer on the organization"),
			},
			{
				// Without the organization, the list is all there is.
				Config:      fmt.Sprintf("data %q \"read\" {\n  id = \"example\"\n}\n", projectType),
				ExpectError: regexp.MustCompile(`No such project`),
			},
		},
	})
}

// The organization's read is refused with the role it needs, and the
// organizations list it points to answers in its place.
func TestAccOrganizationWithAProjectCredentialSaysWhatItNeeds(t *testing.T) {
	useProjectCredential(t)
	organization := provider.ResourceName("organization")
	organizations := provider.ResourceName("organizations")
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf("data %q \"this\" {\n  id = %q\n}\n", organization, accOrg),
				ExpectError: saying("Reading an organization needs organizations-viewer",
					"A credential scoped to a project can't have that role, but `"+organizations+
						"` lists the organizations it can see, with their status."),
			},
			{
				Config: fmt.Sprintf("data %q \"all\" {}\n", organizations),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("data."+organizations+".all", "ids.#", "1"),
					resource.TestCheckResourceAttr("data."+organizations+".all", "ids.0", accOrg),
					resource.TestCheckResourceAttr("data."+organizations+".all", "organizations.0.status", "active"),
				),
			},
		},
	})
}

// The gateway answers an organization that doesn't exist with the same bare
// 403, so a credential scoped to the organization that mistypes its id is
// told that may be why, not only that a role is missing.
func TestAccOrganizationThatDoesNotExistSaysTheIDMayBeWrong(t *testing.T) {
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf("data %q \"this\" {\n  id = \"org-not-ours\"\n}\n", provider.ResourceName("organization")),
				ExpectError: saying("Reading an organization needs organizations-viewer",
					"the platform gives this same 403 for an organization id that doesn't exist"),
			},
		},
	})
}

// Every other organization-level read a project credential can't make says
// which role it needs.
func TestAccOrganizationReadsWithAProjectCredentialSayWhatTheyNeed(t *testing.T) {
	useProjectCredential(t)
	for _, c := range []struct {
		suffix, body, want string
	}{
		{"project_quota", fmt.Sprintf("organization_id = %q\n  project_id = \"demo\"", accOrg),
			"Reading quota needs organizations-viewer"},
		{"tenant_quota", fmt.Sprintf("organization_id = %q", accOrg), "Reading quota needs organizations-viewer"},
		{"roles", fmt.Sprintf("organization_id = %q", accOrg), "Listing roles needs iam-viewer or iam-admin"},
		{"service_accounts", fmt.Sprintf("organization_id = %q", accOrg), "Reading service accounts needs iam-admin"},
	} {
		t.Run(c.suffix, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				Steps: []resource.TestStep{
					{
						Config:      fmt.Sprintf("data %q \"this\" {\n  %s\n}\n", provider.ResourceName(c.suffix), c.body),
						ExpectError: saying(c.want, "A credential scoped to a project can't have"),
					},
				},
			})
		})
	}
}

// A project credential's 403 creating a project names the role the create
// needs and the one a destroy needs.
func TestAccProjectCreateWithAProjectCredentialSaysWhatItNeeds(t *testing.T) {
	useProjectCredential(t)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf("resource %q \"test\" {\n  id              = \"acc-project-credential\"\n"+
					"  organization_id = %q\n}\n", projectType, accOrg),
				ExpectError: saying("Creating a project needs organizations-member or organizations-admin",
					"Ask for organizations-admin"),
			},
		},
	})
}
