package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// The fake holds an object whose display name carries this mid-operation
// until the platform has refused a write against it because it's busy, and
// then lets it settle, as another run's operation would finish.
const busyUntilRefused = "fake-busy-until-refused"

// The fake starts another run's operation on a cluster whose display name
// carries this the moment its first upgrade arrives.
const busyAtFirstUpgrade = "fake-busy-at-first-upgrade"

// fakeServed is how many requests with this method and path the fake has
// served, from its own count. The count is taken before a request is served,
// so it holds everything an apply sent by the time the apply returns, which
// the request log, drained through a pipe, doesn't promise.
func fakeServed(method, path string) (int, error) {
	status, body, err := getFake("/_harness/requests?method="+url.QueryEscape(method)+
		"&path="+url.QueryEscape(path), fakeOwnerToken)
	if err != nil {
		return 0, err
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("the fake's request count answered %d: %s", status, body)
	}
	var got struct {
		Count int `json:"count"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		return 0, fmt.Errorf("the fake's request count: %w", err)
	}
	return got.Count, nil
}

// A cluster still creating when the destroy comes, here because its create
// wait timed out, is destroyed after its create finishes when the
// configuration says `on_destroy_busy = "wait"`. CheckDestroy shows the
// cluster is gone and that no cancel was sent for it.
func TestAccDestroyWaitsForABusyClusterWhenAsked(t *testing.T) {
	const name = "acc-destroy-waits"
	config := fmt.Sprintf(`
resource %q "test" {
  id              = %q
  cluster_type    = %q
  display_name    = %q
  on_destroy_busy = "wait"
  timeouts {
    create = "1s"
  }
}
`, clusterType, name, clusterTypeRef, busyUntilRefused)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy: resource.ComposeAggregateTestCheckFunc(
			checkGone(clusterType, clusterPath),
			func(*terraform.State) error {
				cancels, err := fakeServed("POST", "/v1/regions/us-texas-1/projects/demo/compute/clusters/"+name+"/cancel")
				if err != nil {
					return err
				}
				if cancels != 0 {
					return fmt.Errorf("the destroy sent %d cancels, want none: on_destroy_busy is wait", cancels)
				}
				return nil
			},
		),
		Steps: []resource.TestStep{
			{
				// The create recorded on_destroy_busy with the tainted
				// cluster, so the destroy that follows reads it.
				Config:      config,
				ExpectError: regexp.MustCompile(`(?s)is still being created.*terraform untaint <address>`),
			},
			{
				// Its replacement only plans, so the cluster is still the one
				// the test's destroy meets mid-create.
				PreConfig:          func() { checkFakeStateNow(t, name, "creating") },
				Config:             config,
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
		},
	})
}

// `on_destroy_busy` is the provider's alone, so changing it on a settled
// cluster is an update that sends the platform nothing, and the plan after it
// is empty.
func TestAccOnDestroyBusyChangesInPlace(t *testing.T) {
	const name = "acc-on-destroy-busy"
	config := func(onBusy string) string {
		return strings.Replace(clusterConfig(name, 2), `display_name = "acceptance"`,
			`display_name = "acceptance"`+"\n  on_destroy_busy = "+onBusy, 1)
	}
	patches := func() (int, error) {
		return fakeServed("PATCH", "/v1/regions/us-texas-1/projects/demo/compute/clusters/"+name)
	}
	var before int
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: clusterConfig(name, 2),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckNoResourceAttr(clusterAddr, "on_destroy_busy"),
					func(*terraform.State) error {
						var err error
						before, err = patches()
						return err
					},
				),
			},
			{
				Config: config(`"wait"`),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterAddr, "on_destroy_busy", "wait"),
					resource.TestCheckResourceAttr(clusterAddr, "state", "active"),
					func(*terraform.State) error {
						after, err := patches()
						if err != nil {
							return err
						}
						if after != before {
							return fmt.Errorf("changing on_destroy_busy sent %d PATCHes, want none", after-before)
						}
						return nil
					},
				),
			},
		},
	})
}

// A value outside the two is refused at plan, before anything is made. A test
// of its own, so the destroy that ends every test has a configuration that
// plans.
func TestAccOnDestroyBusyRefusesAnotherValue(t *testing.T) {
	config := strings.Replace(clusterConfig("acc-on-destroy-busy-bad", 2), `display_name = "acceptance"`,
		`display_name = "acceptance"`+"\n  on_destroy_busy = \"sometimes\"", 1)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config:      config,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`(?s)on_destroy_busy.*cancel.*wait`),
		}},
	})
}

// A step to an upgrade is refused because another run started an operation
// on the cluster in the moment after this one settled it. The upgrade waits
// for that operation and takes the step: two upgrade requests, the refused
// one and the one taken, and the cluster lands on the version asked for.
func TestAccUpgradeWaitsOutAnotherRunsOperation(t *testing.T) {
	const name = "acc-upgrade-busy"
	config := func(version int) string {
		return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = %q
  node_pool = {
    "workers" = { node_count = 2 }
  }
}
`, clusterType, name, inferenceVersion(version), busyAtFirstUpgrade)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: config(1),
				Check:  resource.TestCheckResourceAttr(clusterAddr, "cluster_type_resolved", inferenceVersion(1)),
			},
			{
				Config:           config(2),
				ConfigPlanChecks: inPlace(),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterAddr, "cluster_type_resolved", inferenceVersion(2)),
					resource.TestCheckResourceAttr(clusterAddr, "state", "active"),
					func(*terraform.State) error {
						sent, err := fakeServed("POST", "/v1/regions/us-texas-1/projects/demo/compute/clusters/"+name+"/upgrade")
						if err != nil {
							return err
						}
						if sent != 2 {
							return fmt.Errorf("the apply sent %d upgrades, want the one refused and the one taken", sent)
						}
						return nil
					},
				),
			},
		},
	})
}

// A store another run is still creating is imported, and the same apply
// changes it. The platform refuses the update with CONFLICT_IN_USE while the
// store is `creating`. The update waits for it to settle and sends again, with
// the etag the settled store reads: the fake moves the etag on the settle, so
// one resent with the etag read while it was `creating` is refused as a
// concurrent edit.
func TestAccFileStoreUpdateWaitsForACreatingStore(t *testing.T) {
	const name = "acc-busy-store"
	config := fmt.Sprintf(`
import {
  to = %[1]s.test
  id = %[2]q
}

resource %[1]q "test" {
  id           = %[2]q
  display_name = %[3]q
  description  = "changed while it was creating"
  share_type   = "general"
  capacity_gib = 10
}
`, fileStoreType, name, busyUntilRefused)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(fileStoreType, fileStorePath),
		Steps: []resource.TestStep{{
			PreConfig: func() {
				if err := postFake("/v1/regions/us-texas-1/projects/demo/storage/file-stores", fmt.Sprintf(
					`{"id":%q,"shareType":"general","capacityGiB":10,"displayName":%q}`, name, busyUntilRefused)); err != nil {
					t.Fatal(err)
				}
			},
			Config: config,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(fileStoreAddr, "description", "changed while it was creating"),
				resource.TestCheckResourceAttr(fileStoreAddr, "state", "active"),
				func(*terraform.State) error {
					sent, err := fakeServed("PATCH", "/v1/regions/us-texas-1/projects/demo/storage/file-stores/"+name)
					if err != nil {
						return err
					}
					if sent != 2 {
						return fmt.Errorf("the update sent %d PATCHes, want the one refused while creating and the one sent after", sent)
					}
					return nil
				},
			),
		}},
	})
}
