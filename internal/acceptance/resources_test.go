package acceptance

import (
	"fmt"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

const clusterTypeRef = "/v1/regions/us-texas-1/compute/cluster-types/gpu-training"

// Derived, never written out: `make name-audit` is what keeps the provider
// name in one constant, and a test config is Go source like any other.
var (
	clusterType       = provider.ResourceName("kubernetes_cluster")
	instanceGroupType = provider.ResourceName("instance_group")
	clusterAddr       = clusterType + ".test"
	instanceGroupAddr = instanceGroupType + ".test"
)

func clusterConfig(name string, count int) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = "acceptance"
  node_pool = {
    "gpu-workers" = { node_count = %d }
  }
}
`, clusterType, name, clusterTypeRef, count)
}

func instanceGroupConfig(name string, size int) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = "acceptance"
  node_pool = {
    "gpu-workers" = { size = %d }
  }
}
`, instanceGroupType, name, clusterTypeRef, size)
}

func TestAccCluster(t *testing.T) {
	const name = "acc-cluster"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: clusterConfig(name, 2),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterAddr, "id", name),
					resource.TestCheckResourceAttr(clusterAddr, "state", "active"),
					// The scope has to reach state, or a later provider-block
					// change retargets this resource instead of replacing it.
					resource.TestCheckResourceAttr(clusterAddr, "region", "us-texas-1"),
					resource.TestCheckResourceAttr(clusterAddr, "project", "demo"),
				),
			},
			{
				Config:   clusterConfig(name, 2),
				PlanOnly: true, // the second plan is empty
			},
			{
				Config: clusterConfig(name, 3), // scale in place
				Check: resource.TestCheckResourceAttr(
					clusterAddr, "node_pool.gpu-workers.node_count", "3"),
			},
			{
				Config:            clusterConfig(name, 3),
				ResourceName:      clusterAddr,
				ImportState:       true,
				ImportStateId:     name,
				ImportStateVerify: true,
				// The same as the scoped import below, for the same reasons.
				// The bare id has to land in the same scope, so region and
				// project are compared.
				ImportStateVerifyIgnore: []string{"node_pool.%", "node_pool.control-plane"},
			},
			{
				Config:            clusterConfig(name, 3),
				ResourceName:      clusterAddr,
				ImportState:       true,
				ImportStateId:     "us-texas-1/demo/" + name,
				ImportStateVerify: true,
				// An import adopts every pool, so the pool count and the
				// control-plane pool the configuration doesn't manage are
				// ignored. The gpu-workers count is still compared, and so is
				// the cluster type: bound at its current version, the import
				// records the unversioned spelling the configuration used.
				ImportStateVerifyIgnore: []string{"node_pool.%", "node_pool.control-plane"},
			},
		},
	})
}

func TestAccInstanceGroup(t *testing.T) {
	const name = "acc-hosts"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{
			{
				Config: instanceGroupConfig(name, 1),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(instanceGroupAddr, "id", name),
					resource.TestCheckResourceAttr(instanceGroupAddr, "region", "us-texas-1"),
					resource.TestCheckResourceAttr(instanceGroupAddr, "project", "demo"),
				),
			},
			{
				Config:   instanceGroupConfig(name, 1),
				PlanOnly: true,
			},
			{
				Config: instanceGroupConfig(name, 2),
				Check: resource.TestCheckResourceAttr(
					instanceGroupAddr, "node_pool.gpu-workers.size", "2"),
			},
			{
				Config:                  instanceGroupConfig(name, 2),
				ResourceName:            instanceGroupAddr,
				ImportState:             true,
				ImportStateId:           "us-texas-1/demo/" + name,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"node_pool"},
			},
		},
	})
}
