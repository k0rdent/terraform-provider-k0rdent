package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// Leaving `node_pool` out reads every materialized pool into state, and the
// next plan has to accept that rather than propose an update.
func TestAccClusterWithoutNodePoolPlansEmpty(t *testing.T) {
	const name = "acc-no-pool"
	config := fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
}
`, clusterType, name, clusterTypeRef)
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{PostApplyPostRefresh: []plancheck.PlanCheck{
					plancheck.ExpectEmptyPlan(),
				}},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterAddr, "node_pool.gpu-workers.node_count", "2"),
					resource.TestCheckResourceAttr(clusterAddr, "node_pool.control-plane.node_count", "1"),
					resource.TestCheckResourceAttr(clusterAddr, "node_count", "3"),
				),
			},
			{Config: config, PlanOnly: true},
			{RefreshState: true},
			{Config: config, PlanOnly: true},
		},
	})
}

// A configured pool scaled behind Terraform's back is drift, and the plan has
// to say so.
func TestAccClusterConfiguredPoolDriftShows(t *testing.T) {
	const name = "acc-pool-drift"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{Config: clusterConfig(name, 2)},
			{
				PreConfig: func() {
					if err := scaleClusterOutOfBand(name, "gpu-workers", 5); err != nil {
						t.Fatal(err)
					}
				},
				Config:             clusterConfig(name, 2),
				PlanOnly:           true,
				ExpectNonEmptyPlan: true,
			},
			{
				Config: clusterConfig(name, 2),
				Check:  resource.TestCheckResourceAttr(clusterAddr, "node_pool.gpu-workers.node_count", "2"),
			},
		},
	})
}

// scaleClusterOutOfBand scales through the API and waits for it to settle, so
// the next refresh reads the new count rather than one still in flight.
func scaleClusterOutOfBand(id, pool string, count int) error {
	token, err := mintFakeToken()
	if err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"nodePools": []map[string]any{{"id": pool, "nodeCount": count}},
	})
	if err != nil {
		return err
	}
	path := "/v1/regions/us-texas-1/projects/demo/compute/clusters/" + id
	req, err := http.NewRequest(http.MethodPost, endpoint+path+"/scale", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := harnessHTTP.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the fake refused the scale: %s", resp.Status)
	}
	for range 10 {
		_, body, err := readFake(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), `"state":"active"`) {
			return nil
		}
	}
	return fmt.Errorf("%s did not settle after the scale", id)
}
