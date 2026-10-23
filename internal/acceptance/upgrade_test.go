package acceptance

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// The fake's gpu-inference history: v1 and v2 available, v2 allowing at most
// four workers, v3 a draft, v4 available, and v5 available but failing to
// reconcile when upgraded into.
const inferenceRef = "/v1/regions/us-texas-1/compute/cluster-types/gpu-inference"

func inferenceVersion(n int) string {
	return fmt.Sprintf("%s/versions/aaaaaaaa-0000-4000-8000-%012d", inferenceRef, n)
}

func inferenceConfig(name string, version, workers int) string {
	return poolConfig(name, inferenceVersion(version), workers)
}

func poolConfig(name, ref string, workers int) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  node_pool = {
    "workers" = { node_count = %d }
  }
}
`, clusterType, name, ref, workers)
}

// boundsVersion spells version n of one of the fake's bounds types.
func boundsVersion(typeID, prefix string, n int) string {
	return fmt.Sprintf("/v1/regions/us-texas-1/compute/cluster-types/%s/versions/%s-0000-4000-8000-%012d",
		typeID, prefix, n)
}

func inPlace() resource.ConfigPlanChecks {
	return resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
		plancheck.ExpectResourceAction(clusterAddr, plancheck.ResourceActionUpdate),
	}}
}

// v1 to v4 is two upgrades, v2 then v4, skipping the draft, in one apply. Going
// back afterwards is refused at plan time rather than replacing the cluster.
func TestAccClusterUpgradesAcrossTwoVersions(t *testing.T) {
	const name = "acc-upgrade"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: inferenceConfig(name, 1, 2),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterAddr, "cluster_type_resolved", inferenceVersion(1)),
					resource.TestCheckResourceAttr(clusterAddr, "kubernetes_version", "v1.34.0+k0s.0"),
				),
			},
			{
				Config: inferenceConfig(name, 4, 2),
				// The plan shows the Kubernetes version the upgrade reaches,
				// v4's, rather than "known after apply". The node total is
				// left to the apply, since a new version can bring or drop
				// pools.
				ConfigPlanChecks: resource.ConfigPlanChecks{PreApply: []plancheck.PlanCheck{
					plancheck.ExpectResourceAction(clusterAddr, plancheck.ResourceActionUpdate),
					plancheck.ExpectKnownValue(clusterAddr, tfjsonpath.New("kubernetes_version"),
						knownvalue.StringExact("v1.35.1+k0s.0")),
					unknownAfterApply(clusterAddr, "node_count"),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterAddr, "kubernetes_version", "v1.35.1+k0s.0"),
					resource.TestCheckResourceAttr(clusterAddr, "cluster_type_resolved", inferenceVersion(4)),
					resource.TestCheckResourceAttr(clusterAddr, "cluster_type", inferenceVersion(4)),
					resource.TestCheckResourceAttr(clusterAddr, "state", "active"),
					resource.TestCheckResourceAttr(clusterAddr, "node_pool.workers.node_count", "2"),
				),
			},
			{
				Config:   inferenceConfig(name, 4, 2),
				PlanOnly: true,
			},
			{
				Config:      inferenceConfig(name, 2, 2),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)different\s+version\s+of\s+its\s+ClusterType.*forward-only`),
			},
		},
	})
}

// v2 to v5 passes v4 and then fails. State records v4, the version the cluster
// reached, so a configuration pinned to v4 plans clean, and the cluster is
// still there to destroy.
func TestAccClusterUpgradeStopsAtAFailedStep(t *testing.T) {
	const name = "acc-upgrade-fails"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{Config: inferenceConfig(name, 2, 2)},
			{
				Config: inferenceConfig(name, 5, 2),
				// The runtime event log's warning and error come before the
				// advice, through the fake's listClusterRuntimeEvents.
				// Terraform wraps the detail, so any space may be a newline.
				ExpectError: regexp.MustCompile(`(?s)` + strings.ReplaceAll(strings.Join([]string{
					`failed at version 4 of its ClusterType`,
					`the last 2 of 2:`,
					`warn control-plane, updating-cluster: control-plane replicas did not report ready`,
					`error cluster, updating-cluster: the upgrade to clusterType version 5 did not reconcile`,
					`does not retry`, `Nothing was deleted`,
				}, ".*"), " ", `\s+`)),
			},
			{
				Config:   inferenceConfig(name, 4, 2),
				PlanOnly: true,
			},
			{
				// The cluster data source reads the same failure afterwards.
				Config: inferenceConfig(name, 4, 2) + fmt.Sprintf(`
data %q "read" {
  id = %q
}
`, clusterType, name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterDataAddr, "state", "failed"),
					resource.TestCheckResourceAttr(clusterDataAddr, "operation.status", "failed"),
					resource.TestCheckNoResourceAttr(clusterDataAddr, "upgrade_target_version_id"),
					resource.TestCheckResourceAttr(clusterDataAddr, "cluster_type", inferenceVersion(4)),
					resource.TestCheckResourceAttr(clusterDataAddr, "kubernetes_version", "v1.35.1+k0s.0"),
					resource.TestCheckResourceAttr(clusterAddr, "kubernetes_version", "v1.35.1+k0s.0"),
				),
			},
			{
				Config:      inferenceConfig(name, 5, 2),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`does\s+not\s+retry\s+an\s+upgrade\s+from\s+` + "`failed`"),
			},
		},
	})
}

// v2 allows at most four workers. With six, the upgrade is refused and says
// which pool and why. Shrinking to three in the same apply works, because the
// decrease goes first.
func TestAccClusterUpgradeAndScaleDownInOneApply(t *testing.T) {
	const name = "acc-upgrade-shrink"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{Config: inferenceConfig(name, 1, 6)},
			{
				Config:      inferenceConfig(name, 2, 6),
				ExpectError: regexp.MustCompile(`(?s)"workers"\s+has\s+6\s+nodes.*version\s+2\s+allows\s+1\s+to\s+4`),
			},
			{
				Config:           inferenceConfig(name, 2, 3),
				ConfigPlanChecks: inPlace(),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterAddr, "cluster_type_resolved", inferenceVersion(2)),
					resource.TestCheckResourceAttr(clusterAddr, "node_pool.workers.node_count", "3"),
				),
			},
			{
				Config:   inferenceConfig(name, 2, 3),
				PlanOnly: true,
			},
		},
	})
}

// A cluster_type that is only known at apply, here from another resource, can
// still move the cluster to another version.
func TestAccClusterUpgradesToAVersionKnownOnlyAtApply(t *testing.T) {
	const name = "acc-upgrade-unknown"
	config := func(version int) string {
		return fmt.Sprintf(`
resource "terraform_data" "v" {
  input = %q
}
resource %q "test" {
  id           = %q
  cluster_type = terraform_data.v.output
}
`, inferenceVersion(version), clusterType, name)
	}
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{Config: config(1)},
			{
				Config: config(2),
				Check:  resource.TestCheckResourceAttr(clusterAddr, "cluster_type_resolved", inferenceVersion(2)),
			},
		},
	})
}

// Each version can move a pool's minimum or maximum. A scale is checked
// against the version the cluster is on and an upgrade against the one it
// moves to, so each count change and the upgrade have to go in the order the
// bounds admit. All four directions, each in one apply.
func TestAccClusterUpgradeOrdersScalesByBounds(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		have, want     int
	}{
		{"max lowered", inferenceVersion(1), inferenceVersion(2), 6, 3},
		{"max raised", inferenceVersion(2), inferenceVersion(4), 4, 8},
		{"min raised", boundsVersion("gpu-minraise", "bbbbbbbb", 1), boundsVersion("gpu-minraise", "bbbbbbbb", 2), 2, 3},
		{"min lowered", boundsVersion("gpu-minlower", "cccccccc", 1), boundsVersion("gpu-minlower", "cccccccc", 2), 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name := "acc-order-" + regexp.MustCompile(" ").ReplaceAllString(tc.name, "-")
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: protoV6,
				CheckDestroy:             checkGone(clusterType, clusterPath),
				Steps: []resource.TestStep{
					{Config: poolConfig(name, tc.from, tc.have)},
					{
						Config:           poolConfig(name, tc.to, tc.want),
						ConfigPlanChecks: inPlace(),
						Check: resource.ComposeAggregateTestCheckFunc(
							resource.TestCheckResourceAttr(clusterAddr, "cluster_type_resolved", tc.to),
							resource.TestCheckResourceAttr(clusterAddr, "node_pool.workers.node_count", fmt.Sprint(tc.want)),
						),
					},
					{Config: poolConfig(name, tc.to, tc.want), PlanOnly: true},
				},
			})
		})
	}
}

// A count read from another resource is unknown at plan, so the route is
// checked at apply. Here the count is what makes the raised minimum fit.
func TestAccClusterUpgradeWithACountKnownOnlyAtApply(t *testing.T) {
	const name = "acc-order-unknown-count"
	cfg := func(ref string, workers int) string {
		return fmt.Sprintf(`
resource "terraform_data" "workers" {
  input = %d
}

resource %q "test" {
  id           = %q
  cluster_type = %q
  node_pool = {
    "workers" = { node_count = terraform_data.workers.output }
  }
}
`, workers, clusterType, name, ref)
	}
	from, to := boundsVersion("gpu-minraise", "bbbbbbbb", 1), boundsVersion("gpu-minraise", "bbbbbbbb", 2)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{Config: cfg(from, 2)},
			{
				Config:           cfg(to, 3),
				ConfigPlanChecks: inPlace(),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterAddr, "cluster_type_resolved", to),
					resource.TestCheckResourceAttr(clusterAddr, "node_pool.workers.node_count", "3"),
				),
			},
		},
	})
}

// A route some step cannot take is refused at plan, before any of it runs:
// gpu-chain's version 3 allows one worker, and its version 4 is BMaaS-only.
func TestAccClusterUpgradeRefusesARouteAtPlan(t *testing.T) {
	const name = "acc-upgrade-route"
	chain := func(n int) string { return boundsVersion("gpu-chain", "dddddddd", n) }
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{Config: poolConfig(name, chain(1), 2)},
			{
				Config:      poolConfig(name, chain(3), 2),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)"workers".*version\s+3\s+allows\s+1\s+to\s+1`),
			},
			{
				Config:      poolConfig(name, chain(5), 1),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)Version\s+4\s+is\s+the\s+next\s+available.*no\s+Kubernetes\s+configuration`),
			},
			{
				Config:           poolConfig(name, chain(3), 1),
				ConfigPlanChecks: inPlace(),
				Check:            resource.TestCheckResourceAttr(clusterAddr, "cluster_type_resolved", chain(3)),
			},
		},
	})
}
