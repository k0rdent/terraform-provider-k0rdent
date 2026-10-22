package acceptance

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

var (
	versionsType = provider.ResourceName("cluster_type_versions")
	versionType  = provider.ResourceName("cluster_type_version")
	versionsAddr = "data." + versionsType + ".history"
	versionAddr  = "data." + versionType + ".pinned"
)

// A cluster type's version history is what the API can answer. The two things
// worth asserting are that it comes back oldest first and that each entry carries the
// Kubernetes version the list record does not.
func TestAccClusterTypeVersions(t *testing.T) {
	config := fmt.Sprintf(`
data %q "history" {
  cluster_type = "gpu-training"
}
`, versionsType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config: config,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(versionsAddr, "versions.#", "4"),

				// Oldest first. Index 0 being version 1 is the assertion; a
				// provider that sorted or reversed would fail here.
				resource.TestCheckResourceAttr(versionsAddr, "versions.0.version", "1"),
				resource.TestCheckResourceAttr(versionsAddr, "versions.0.status", "deprecated"),
				resource.TestCheckResourceAttr(versionsAddr, "versions.3.version", "4"),
				resource.TestCheckResourceAttr(versionsAddr, "versions.3.status", "draft"),

				// The fan-out. `listClusterTypeVersions` carries no k0s version,
				// so a value here proves the per-entry read happened.
				resource.TestCheckResourceAttr(versionsAddr, "versions.0.kubernetes_version", "v1.34.0+k0s.0"),
				resource.TestCheckResourceAttr(versionsAddr, "versions.1.kubernetes_version", "v1.35.1+k0s.0"),

				// Two versions, one Kubernetes version. A module that treated
				// this list as a list of distinct Kubernetes versions would be
				// reading it wrong, and the fixture says so.
				resource.TestCheckResourceAttr(versionsAddr, "versions.2.kubernetes_version", "v1.35.1+k0s.0"),

				// Version-scoped, which is the whole reason to read this.
				resource.TestCheckResourceAttr(versionsAddr, "versions.1.self_link",
					"/v1/regions/us-texas-1/compute/cluster-types/gpu-training/versions/22222222-2222-2222-2222-222222222222"),
				resource.TestCheckResourceAttr(versionsAddr, "versions.0.created_at", "2026-01-04T09:00:00Z"),
			),
		}},
	})
}

func TestAccClusterTypeVersionsFilterByStatus(t *testing.T) {
	config := fmt.Sprintf(`
data %q "history" {
  cluster_type = "gpu-training"
  status       = "available"
}
`, versionsType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config: config,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(versionsAddr, "versions.#", "2"),
				resource.TestCheckResourceAttr(versionsAddr, "versions.0.version", "2"),
				resource.TestCheckResourceAttr(versionsAddr, "versions.1.version", "3"),
			),
		}},
	})
}

// The platform answers 422 rather than an empty list, so the plan is where this
// belongs. Without the validator it is a failed apply.
func TestAccClusterTypeVersionsRejectsAnUnknownStatus(t *testing.T) {
	config := fmt.Sprintf(`
data %q "history" {
  cluster_type = "gpu-training"
  status       = "retired"
}
`, versionsType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config:      config,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`(?s)status.*value must be one of`),
		}},
	})
}

// A pinned read is the version-scoped `self_link`, which is what stops a new
// ClusterType version moving a cluster underneath its configuration.
func TestAccClusterTypeVersionPinned(t *testing.T) {
	config := fmt.Sprintf(`
data %q "pinned" {
  cluster_type = "gpu-training"
  version_id   = "22222222-2222-2222-2222-222222222222"
}
`, versionType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config: config,
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(versionAddr, "version", "2"),
				resource.TestCheckResourceAttr(versionAddr, "status", "available"),
				resource.TestCheckResourceAttr(versionAddr, "kubernetes_version", "v1.35.1+k0s.0"),
				resource.TestCheckResourceAttr(versionAddr, "self_link",
					"/v1/regions/us-texas-1/compute/cluster-types/gpu-training/versions/22222222-2222-2222-2222-222222222222"),
				// The snapshot carries the pools, which is what makes it usable
				// for anything beyond pinning.
				resource.TestCheckResourceAttr(versionAddr, "node_pools.#", "2"),
			),
		}},
	})
}

// A malformed id is a 400 whose message names the path parameter. Answering it
// here names the attribute instead, and costs no round trip.
func TestAccClusterTypeVersionRejectsAMalformedID(t *testing.T) {
	config := fmt.Sprintf(`
data %q "pinned" {
  cluster_type = "gpu-training"
  version_id   = "version-two"
}
`, versionType)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config:      config,
			ExpectError: regexp.MustCompile(`Not a version id`),
		}},
	})
}
