package acceptance

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// A pool size read from another resource is unknown when the plan is made.
func unknownSizeConfig(name, pool string, size int) string {
	return fmt.Sprintf(`
resource "terraform_data" "size" {
  input = %d
}

resource %q "test" {
  id           = %q
  cluster_type = %q
  node_pool = {
    "gpu-workers" = %s
  }
}
`, size, instanceGroupType, name, clusterTypeRef, pool)
}

func TestAccInstanceGroupGrowsToASizeKnownOnlyAtApply(t *testing.T) {
	const name = "acc-ig-unknown-size"
	pool := "{ size = terraform_data.size.output }"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{
			{Config: unknownSizeConfig(name, pool, 1)},
			{
				Config: unknownSizeConfig(name, pool, 3),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(instanceGroupAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.TestCheckResourceAttr(instanceGroupAddr, "node_pool.gpu-workers.size", "3"),
			},
			{
				Config:           unknownSizeConfig(name, pool, 3),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{plancheck.ExpectEmptyPlan()}},
			},
		},
	})
}

// The shrink is only visible once the size is known. The plan made at apply
// time is then a replacement the configuration does not permit, so the apply
// fails there rather than replacing a group the plan said it would update.
func TestAccInstanceGroupShrinkKnownOnlyAtApplyDeletesNothing(t *testing.T) {
	const name = "acc-ig-unknown-shrink"
	pool := "{ size = terraform_data.size.output }"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{
			{Config: unknownSizeConfig(name, pool, 2)},
			{
				Config:      unknownSizeConfig(name, pool, 1),
				ExpectError: refusal("node_pool", "instance group"),
			},
			{
				// Still there to update, not to create.
				Config: unknownSizeConfig(name, pool, 2),
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(instanceGroupAddr, plancheck.ResourceActionUpdate),
				}},
				Check: resource.TestCheckResourceAttr(instanceGroupAddr, "node_pool.gpu-workers.size", "2"),
			},
		},
	})
}
