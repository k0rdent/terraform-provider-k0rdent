package acceptance

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

func ceilingConfig(name string, gib int) string {
	return fmt.Sprintf(`
resource %q "test" {
  id                  = %q
  cluster_type        = %q
  display_name        = "acceptance"
  storage_ceiling_gib = %d
  node_pool = {
    "gpu-workers" = { node_count = 1 }
  }
}
`, clusterType, name, clusterTypeRef, gib)
}

// v1.3.0-dev.4's storage ceiling: sent on the create, raised in place, kept
// when it's left out, and refused at plan when it would go down.
func TestAccClusterStorageCeiling(t *testing.T) {
	const name = "acc-storage"
	addr := clusterType + ".test"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: ceilingConfig(name, 100),
				Check:  resource.TestCheckResourceAttr(addr, "storage_ceiling_gib", "100"),
			},
			{Config: ceilingConfig(name, 100), PlanOnly: true},
			{
				Config: ceilingConfig(name, 250),
				Check:  resource.TestCheckResourceAttr(addr, "storage_ceiling_gib", "250"),
			},
			{Config: ceilingConfig(name, 250), PlanOnly: true},
			{
				Config:      ceilingConfig(name, 50),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)storage ceiling can't be lowered.*250.*50`),
			},
			{
				// Left out, the ceiling the cluster has stays.
				Config:   clusterConfig(name, 1),
				PlanOnly: true,
			},
		},
	})
}
