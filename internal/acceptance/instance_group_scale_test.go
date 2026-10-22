package acceptance

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
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
	for name, marker := range map[string]string{
		"acc-settling":    "fake-scale-settling",
		"acc-unconfirmed": "fake-scale-unconfirmed",
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
