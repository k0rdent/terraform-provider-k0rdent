package acceptance

import (
	"fmt"
	"net/http"
	"regexp"
	"runtime"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// The display name carries a marker that tells the fake which scale refusal to
// answer with. They are listed in internal/tools/fakeapi.
func markedInstanceGroupConfig(name, marker string, size int) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = "acceptance %s"
  node_pool = {
    "gpu-workers" = { size = %d }
  }
}
`, instanceGroupType, name, clusterTypeRef, marker, size)
}

// The platform answers a scale it will admit shortly with a 409 or a 503. The
// apply waits those out instead of failing.
func TestAccInstanceGroupScaleWaitsOutTransientRefusals(t *testing.T) {
	t.Parallel()
	for name, marker := range map[string]string{
		"acc-settling":     "fake-scale-settling",
		"acc-settling-v14": "fake-scale-settling fake-api-v1.4",
		"acc-unconfirmed":  "fake-scale-unconfirmed",
	} {
		t.Run(marker, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
				Steps: []resource.TestStep{
					{Config: markedInstanceGroupConfig(name, marker, 1)},
					{
						Config: markedInstanceGroupConfig(name, marker, 2),
						Check: resource.TestCheckResourceAttr(
							instanceGroupAddr, "node_pool.gpu-workers.size", "2"),
					},
				},
			})
		})
	}
}

// A refusal that retrying cannot clear fails the apply at once, and the
// diagnostic says what to change.
func TestAccInstanceGroupScaleRefusalsSayWhatToDo(t *testing.T) {
	t.Parallel()
	for marker, expect := range map[string]*regexp.Regexp{
		"fake-scale-over-quota": regexp.MustCompile(
			`(?s)requested 2, admissible 1, binding scope project.*at most 1 host`),
		"fake-pool-short":                        regexp.MustCompile(`(?s)POOL_COUNT_MISMATCH.*-refresh-only`),
		"fake-scale-refuses-RESERVATION_DRIFTED": regexp.MustCompile(`(?s)RESERVATION_DRIFTED.*administrator`),
	} {
		t.Run(marker, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
				Steps: []resource.TestStep{
					{Config: markedInstanceGroupConfig("acc-refused", marker, 2)},
					{Config: markedInstanceGroupConfig("acc-refused", marker, 4), ExpectError: expect},
				},
			})
		})
	}
}

// A group deleted while it is still creating reads creating, then failed, then
// is gone. Destroying one whose create timed out has to wait through that.
func TestAccInstanceGroupDestroyAfterACreateTimeout(t *testing.T) {
	t.Parallel()
	config := fmt.Sprintf(`
resource %q "test" {
  id           = "acc-stuck"
  cluster_type = %q
  display_name = "acceptance fake-creates-forever"
  node_pool = {
    "gpu-workers" = { size = 1 }
  }
  timeouts {
    create = "1s"
  }
}
`, instanceGroupType, clusterTypeRef)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{
			{Config: config, ExpectError: regexp.MustCompile(`is still being created`)},
		},
	})
}

// The platform refuses to delete an instance group while it's `updating`,
// with a CONFLICT_IN_USE 409, and says to retry once the group is `active` or
// `failed`. A destroy that finds a scale running waits for it to finish and
// then deletes the group. The scale is sent outside Terraform, as another run
// would send it, so each destroy skips its refresh, as the plan's refusal of
// the changed `node_pool` says to. That also leaves the read before the DELETE
// the first of the two the fake takes to finish a scale.
func TestAccInstanceGroupDestroyWaitsOutAScale(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		skipTest(t, "the CLI sequence runs on unix")
	}
	const name = "acc-destroy-mid-scale"
	path := "/v1/regions/us-texas-1/projects/demo/compute/instance-groups/" + name
	c := newCLIRun(t)
	c.write(instanceGroupConfig(name, 1))
	if out, ok := c.run("apply", "-auto-approve"); !ok {
		t.Fatalf("apply failed:\n%s", out)
	}
	destroyed := false
	t.Cleanup(func() {
		if destroyed {
			return
		}
		if out, ok := c.run("destroy", "-auto-approve", "-refresh=false"); !ok {
			t.Errorf("cleanup destroy failed:\n%s", out)
		}
	})

	if err := postFake(path+"/scale", `{"nodePools":[{"nodePool":"gpu-workers","size":2}]}`); err != nil {
		t.Fatalf("scale: %v", err)
	}
	out, ok := c.run("destroy", "-auto-approve", "-refresh=false")
	if !ok {
		t.Fatalf("the destroy of a group mid-scale failed:\n%s", out)
	}
	destroyed = true
	if status, _, err := readFakeStore(path); err != nil || status != http.StatusNotFound {
		t.Errorf("the group is still on the platform after the destroy: %d %v", status, err)
	}
}

// pooledInstanceGroupConfig is a group naming the given pools, read back
// through the data source so a check sees every pool the platform holds.
func pooledInstanceGroupConfig(name, pools string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  node_pool    = { %s }
}

data %q "read" {
  id         = %q
  depends_on = [%s.test]
}
`, instanceGroupType, name, clusterTypeRef, pools, instanceGroupType, name, instanceGroupType)
}

// The platform creates every pool on the ClusterType, so a pool `node_pool`
// starts naming is already on the group. At the platform's size it is adopted
// with nothing sent, and larger it is scaled, in place either way. Smaller
// would need a shrink, which is a replacement, and an id the group doesn't
// have is refused at plan.
func TestAccInstanceGroupNamesAPoolItAlreadyHas(t *testing.T) {
	t.Parallel()
	update := resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
		plancheck.ExpectResourceAction(instanceGroupAddr, plancheck.ResourceActionUpdate),
	}}
	for name, tc := range map[string]struct {
		added string
		want  string
	}{
		// gpu-training's control-plane is 1 and gpu-workers 2 by default.
		"acc-adopt-same":  {`"gpu-workers" = { size = 2 }`, "2"},
		"acc-adopt-grown": {`"gpu-workers" = { size = 3 }`, "3"},
	} {
		t.Run(name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
				Steps: []resource.TestStep{
					{Config: pooledInstanceGroupConfig(name, `"control-plane" = { size = 1 }`)},
					{
						Config:           pooledInstanceGroupConfig(name, `"control-plane" = { size = 1 }, `+tc.added),
						ConfigPlanChecks: update,
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(instanceGroupAddr, "node_pool.gpu-workers.size", tc.want),
							resource.TestCheckResourceAttr(instanceGroupDataAddr, "node_pools.gpu-workers.desired_count", tc.want),
							resource.TestCheckResourceAttr(instanceGroupDataAddr, "node_pools.control-plane.desired_count", "1"),
						),
					},
				},
			})
		})
	}
}

func TestAccInstanceGroupNamesAPoolItCantAdopt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		added string
		want  *regexp.Regexp
	}{
		"acc-adopt-smaller": {`"gpu-workers" = { size = 1 }`, regexp.MustCompile("Changing `node_pool` would replace")},
		"acc-adopt-unknown": {`"cpu-workers" = { size = 1 }`, regexp.MustCompile(`has no pool "cpu-workers"`)},
	} {
		t.Run(name, func(t *testing.T) {
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
				Steps: []resource.TestStep{
					{Config: pooledInstanceGroupConfig(name, `"control-plane" = { size = 1 }`)},
					{
						Config:      pooledInstanceGroupConfig(name, `"control-plane" = { size = 1 }, `+tc.added),
						PlanOnly:    true,
						ExpectError: tc.want,
					},
				},
			})
		})
	}
}

// A pool a later ClusterType version brings isn't created on a group bound to
// an earlier one. gpu-poolgrow's version 2 adds a `system` pool.
func TestAccInstanceGroupGetsOnlyItsVersionsPools(t *testing.T) {
	t.Parallel()
	const name = "acc-version-pools"
	config := fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = "/v1/regions/us-texas-1/compute/cluster-types/gpu-poolgrow/versions/eeeeeeee-0000-4000-8000-000000000001"
  node_pool    = { "workers" = { size = 2 } }
}

data %q "read" {
  id         = %q
  depends_on = [%s.test]
}
`, instanceGroupType, name, instanceGroupType, name, instanceGroupType)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{{
			Config: config,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(instanceGroupDataAddr, "node_pools.%", "2"),
				resource.TestCheckResourceAttr(instanceGroupDataAddr, "node_pools.control-plane.desired_count", "1"),
				resource.TestCheckNoResourceAttr(instanceGroupDataAddr, "node_pools.system.desired_count"),
			),
		}},
	})
}
