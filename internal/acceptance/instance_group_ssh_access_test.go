package acceptance

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// sshAccessConfig is an instance group with the SSH access block given,
// written inside `ssh_access = [ ... ]`.
func sshAccessConfig(name, users string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  node_pool = {
    "gpu-workers" = { size = 1 }
  }
  ssh_access = [%s
  ]
}
`, instanceGroupType, name, clusterTypeRef, users)
}

// Written zoe first. The platform answers adam first, as a k0rdent API build
// from main was seen to, so a plan that compared the users in order would drift.
const (
	sshZoe = `
    {
      username        = "zoe"
      ssh_public_keys = ["ssh-ed25519 zed", "ssh-ed25519 abe"]
      sudo            = "ALL=(ALL) NOPASSWD:ALL"
    },`
	sshAdam = `
    {
      username        = "adam"
      ssh_public_keys = ["ssh-ed25519 abe"]
      display_name    = "Adam"
    },`
)

// The access is sent on create, read back without drift although the platform
// reorders it, and, being immutable, a change to it is a replacement the guard
// refuses until the configuration permits it.
func TestAccInstanceGroupSSHAccess(t *testing.T) {
	const name = "acc-ig-ssh"
	both := sshAccessConfig(name, sshZoe+sshAdam)
	zoeOnly := sshAccessConfig(name, sshZoe)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{
			{
				// file() of a .pub file ends in a newline, and the platform
				// trims it, so the key would read back different.
				Config: sshAccessConfig(name, `
    {
      username        = "adam"
      ssh_public_keys = ["ssh-ed25519 abe\n"]
    },`),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`no whitespace at either end`),
			},
			{
				Config: both,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(instanceGroupAddr, "state", "active"),
					resource.TestCheckResourceAttr(instanceGroupAddr, "ssh_access.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs(instanceGroupAddr, "ssh_access.*", map[string]string{
						"username":          "zoe",
						"ssh_public_keys.#": "2",
						"ssh_public_keys.0": "ssh-ed25519 zed",
						"ssh_public_keys.1": "ssh-ed25519 abe",
						"sudo":              "ALL=(ALL) NOPASSWD:ALL",
					}),
					resource.TestCheckTypeSetElemNestedAttrs(instanceGroupAddr, "ssh_access.*", map[string]string{
						"username":     "adam",
						"display_name": "Adam",
					}),
				),
			},
			{
				Config:           both,
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
			{
				Config:      zoeOnly,
				PlanOnly:    true,
				ExpectError: refusal("ssh_access", "instance group"),
			},
			{
				Config:           permitReplace(zoeOnly, instanceGroupType),
				ConfigPlanChecks: expect(instanceGroupAddr, plancheck.ResourceActionReplace),
				Check:            resource.TestCheckResourceAttr(instanceGroupAddr, "ssh_access.#", "1"),
			},
		},
	})
}

// A group being deleted reads back with no SSH users, because the platform
// releases them as the delete starts (live-shapes-dev4.txt, section E). Live
// on dev.4 that stopped both a plan and a destroy with "Changing `ssh_access`
// would replace this instance group". Here the delete is started out of band,
// the way an interrupted destroy leaves it, then the configuration plans clean
// while the delete runs, and the test's own destroy, which refreshes first,
// waits it out.
func TestAccInstanceGroupBeingDeletedPlansAndDestroysClean(t *testing.T) {
	const name = "acc-ig-ssh-deleting"
	config := fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = "acceptance fake-deletes-slowly"
  node_pool = {
    "gpu-workers" = { size = 1 }
  }
  ssh_access = [%s
  ]
}
`, instanceGroupType, name, clusterTypeRef, sshAdam)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{
			{Config: config},
			{
				PreConfig: func() {
					status, _, err := callFake(http.MethodDelete, instanceGroupPath(map[string]string{
						"region": "us-texas-1", "project": "demo", "id": name,
					}))
					if err != nil || status != http.StatusNoContent {
						t.Fatalf("starting the delete out of band: %d %v", status, err)
					}
				},
				Config:   config,
				PlanOnly: true,
			},
		},
	})
}

// A teardown that fails leaves the group `failed` with its users released.
// The configuration that made it still plans clean, so a second destroy gets
// past its refresh, and a configuration that drops `ssh_access` agrees with
// the platform and records that in place rather than replacing the group.
func TestAccInstanceGroupWhoseTeardownFailedPlansWithoutReplacing(t *testing.T) {
	const name = "acc-ig-ssh-teardown-failed"
	group := func(access string) string {
		return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = "acceptance fake-teardown-fails"
  node_pool = {
    "gpu-workers" = { size = 1 }
  }%s
}
`, instanceGroupType, name, clusterTypeRef, access)
	}
	withAccess := group(`
  ssh_access = [` + sshAdam + `
  ]`)
	path := instanceGroupPath(map[string]string{"region": "us-texas-1", "project": "demo", "id": name})
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{
			{Config: withAccess},
			{
				PreConfig: func() {
					status, _, err := callFake(http.MethodDelete, path)
					if err != nil || status != http.StatusNoContent {
						t.Fatalf("starting the delete out of band: %d %v", status, err)
					}
					for range 10 {
						status, body, err := callFake(http.MethodGet, path)
						if err != nil || status != http.StatusOK {
							t.Fatalf("reading the group: %d %v", status, err)
						}
						if strings.Contains(string(body), `"state":"failed"`) {
							return
						}
					}
					t.Fatal("the teardown never failed")
				},
				Config:   withAccess,
				PlanOnly: true,
			},
			{
				Config:           group(""),
				ConfigPlanChecks: expect(instanceGroupAddr, plancheck.ResourceActionUpdate),
				Check:            resource.TestCheckNoResourceAttr(instanceGroupAddr, "ssh_access.#"),
			},
		},
	})
}
