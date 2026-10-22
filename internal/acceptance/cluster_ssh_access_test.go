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

// clusterSSHConfig is a cluster with the SSH access block given, written
// inside `ssh_access = [ ... ]`.
func clusterSSHConfig(name, users string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  ssh_access = [%s
  ]
}
`, clusterType, name, clusterTypeRef, users)
}

// The users go on the create, and the refresh after it reads them back from
// the platform, which answers adam first, without a change. Immutable, a
// change to them is a replacement the guard refuses until the configuration
// permits it, and the replacement carries the new users.
func TestAccClusterSSHAccess(t *testing.T) {
	const name = "acc-cluster-ssh"
	both := clusterSSHConfig(name, sshZoe+sshAdam)
	zoeOnly := clusterSSHConfig(name, sshZoe)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: both,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterAddr, "ssh_access.#", "2"),
					resource.TestCheckTypeSetElemNestedAttrs(clusterAddr, "ssh_access.*", map[string]string{
						"username":          "zoe",
						"ssh_public_keys.#": "2",
						"ssh_public_keys.0": "ssh-ed25519 zed",
						"ssh_public_keys.1": "ssh-ed25519 abe",
						"sudo":              "ALL=(ALL) NOPASSWD:ALL",
					}),
					resource.TestCheckTypeSetElemNestedAttrs(clusterAddr, "ssh_access.*", map[string]string{
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
				ExpectError: refusal("ssh_access", "cluster"),
			},
			{
				Config:           permitReplace(zoeOnly, clusterType),
				ConfigPlanChecks: expect(clusterAddr, plancheck.ResourceActionReplace),
				Check:            resource.TestCheckResourceAttr(clusterAddr, "ssh_access.#", "1"),
			},
			{
				Config:           permitReplace(zoeOnly, clusterType),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}

// A teardown that fails leaves the cluster `failed` with its node pools and
// SSH users released, which is what a landscape on dev.4 answered. The
// configuration that made it still plans clean, so the refresh that
// `terraform destroy` plans first gets past the replacement guard, and the
// destroy that ends the test succeeds. A configuration that drops
// `ssh_access` agrees with the platform and records that in place.
func TestAccClusterWhoseTeardownFailedStillDestroys(t *testing.T) {
	const name = "acc-cluster-teardown-failed"
	cluster := func(access string) string {
		return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = "acceptance fake-teardown-fails"%s
}
`, clusterType, name, clusterTypeRef, access)
	}
	withAccess := cluster(`
  ssh_access = [` + sshAdam + `
  ]`)
	path := clusterPath(map[string]string{"region": "us-texas-1", "project": "demo", "id": name})
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
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
							t.Fatalf("reading the cluster: %d %v", status, err)
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
				Config:           cluster(""),
				ConfigPlanChecks: expect(clusterAddr, plancheck.ResourceActionUpdate),
				Check:            resource.TestCheckNoResourceAttr(clusterAddr, "ssh_access.#"),
			},
		},
	})
}

// A destroy the platform ends without deleting the cluster says what to do in
// order: destroy again, then stop managing it with a removed block. The second
// destroy, which the test ends with, succeeds.
func TestAccClusterDestroyThePlatformEndedSaysWhatNext(t *testing.T) {
	const name = "acc-cluster-destroy-refused"
	config := fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = "acceptance fake-teardown-fails"
}
`, clusterType, name, clusterTypeRef)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{Config: config},
			{
				Config:      config,
				Destroy:     true,
				ExpectError: regexp.MustCompile(`(?s)run .terraform destroy. again.*removed \{.*destroy = false`),
			},
		},
	})
}
