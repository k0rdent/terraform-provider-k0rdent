package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/provider"
)

const (
	// Pinned one version behind the head, so there is an upgrade to make.
	clusterTypeV2 = clusterTypeRef + "/versions/22222222-2222-2222-2222-222222222222"
	clusterTypeV3 = "33333333-3333-3333-3333-333333333333"
)

var (
	clusterDataAddr       = "data." + clusterType + ".read"
	instanceGroupDataAddr = "data." + instanceGroupType + ".read"
	fileStoreDataAddr     = "data." + fileStoreType + ".read"
)

// postFake sends what the provider never does, such as an upgrade, so a data
// source has something to observe.
func postFake(path, body string) error {
	token, err := mintFakeToken()
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, endpoint+path, bytes.NewBufferString(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := harnessHTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		out, rerr := io.ReadAll(resp.Body)
		if rerr != nil {
			return fmt.Errorf("POST %s answered %d: %w", path, resp.StatusCode, rerr)
		}
		return fmt.Errorf("POST %s answered %d: %s", path, resp.StatusCode, out)
	}
	return nil
}

func clusterReadConfig(name string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = "acceptance fake-holds-upgrade"
  node_pool = {
    "gpu-workers" = { node_count = 1 }
  }
}

data %q "read" {
  id         = %q
  depends_on = [%s.test]
}
`, clusterType, name, clusterTypeV2, clusterType, name, clusterType)
}

// The second step is the point: an upgrade started outside Terraform has to
// show its target, its phase and when it began.
func TestAccClusterDataSourceSeesAnUpgradeInFlight(t *testing.T) {
	const name = "acc-cluster-read"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config: clusterReadConfig(name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterDataAddr, "state", "active"),
					resource.TestCheckResourceAttr(clusterDataAddr, "cluster_type", clusterTypeV2),
					resource.TestCheckResourceAttr(clusterDataAddr, "control_plane_state", "ready"),
					resource.TestCheckResourceAttr(clusterDataAddr, "control_plane_desired_replicas", "3"),
					resource.TestCheckResourceAttr(clusterDataAddr, "control_plane_ready_replicas", "3"),
					resource.TestCheckResourceAttr(clusterDataAddr, "control_plane_version", "v1.35.1+k0s.0"),
					resource.TestCheckResourceAttr(clusterDataAddr, "operation.status", "succeeded"),
					resource.TestCheckResourceAttr(clusterDataAddr, "operation.percent_complete", "100"),
					resource.TestCheckResourceAttr(clusterDataAddr, "operation.completed_at", "2026-09-21T00:20:00Z"),
					resource.TestCheckNoResourceAttr(clusterDataAddr, "operation.current_stage"),
					resource.TestCheckNoResourceAttr(clusterDataAddr, "upgrade_target_version_id"),
					resource.TestCheckNoResourceAttr(clusterDataAddr, "state_reason"),
					resource.TestCheckResourceAttr(clusterDataAddr, "security_groups.#", "0"),
				),
			},
			{
				PreConfig: func() {
					if err := postFake("/v1/regions/us-texas-1/projects/demo/compute/clusters/"+name+"/upgrade", `{}`); err != nil {
						t.Fatal(err)
					}
				},
				Config: clusterReadConfig(name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterDataAddr, "state", "updating"),
					resource.TestCheckResourceAttr(clusterDataAddr, "upgrade_target_version_id", clusterTypeV3),
					// The bound version moves only when the upgrade succeeds.
					resource.TestCheckResourceAttr(clusterDataAddr, "cluster_type", clusterTypeV2),
					resource.TestCheckResourceAttr(clusterDataAddr, "control_plane_state", "progressing"),
					resource.TestCheckResourceAttr(clusterDataAddr, "operation.status", "running"),
					resource.TestCheckResourceAttr(clusterDataAddr, "operation.started_at", "2026-09-21T00:00:00Z"),
					resource.TestCheckResourceAttr(clusterDataAddr, "operation.current_stage", "applying-config"),
					resource.TestCheckResourceAttr(clusterDataAddr, "operation.percent_complete", "25"),
					resource.TestCheckNoResourceAttr(clusterDataAddr, "operation.completed_at"),
				),
			},
			{
				// The platform refuses to delete a cluster mid-upgrade, so the
				// held upgrade is cancelled before the destroy, by the
				// execution id the cluster reports, as v1.3.0-dev.4 requires.
				PreConfig: func() {
					clusterPath := "/v1/regions/us-texas-1/projects/demo/compute/clusters/" + name
					status, body, err := readFake(clusterPath)
					if err != nil || status != http.StatusOK {
						t.Fatalf("read the cluster to cancel: %d %v", status, err)
					}
					var c struct {
						Operation struct {
							ExecutionID string `json:"executionId"`
						} `json:"operation"`
					}
					if err := json.Unmarshal(body, &c); err != nil {
						t.Fatal(err)
					}
					cancel := fmt.Sprintf(`{"executionId":%q}`, c.Operation.ExecutionID)
					if err := postFake(clusterPath+"/cancel", cancel); err != nil {
						t.Fatal(err)
					}
				},
				Config: clusterReadConfig(name),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr(clusterDataAddr, "state", "failed"),
					resource.TestCheckResourceAttr(clusterDataAddr, "operation.status", "cancelled"),
				),
			},
		},
	})
}

func TestAccInstanceGroupDataSource(t *testing.T) {
	const name = "acc-hosts-read"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(instanceGroupType, instanceGroupPath),
		Steps: []resource.TestStep{{
			Config: instanceGroupConfig(name, 2) + fmt.Sprintf(`
data %q "read" {
  id         = %q
  depends_on = [%s.test]
}
`, instanceGroupType, name, instanceGroupType),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(instanceGroupDataAddr, "state", "active"),
				resource.TestCheckResourceAttr(instanceGroupDataAddr, "node_pools.gpu-workers.desired_count", "2"),
				resource.TestCheckResourceAttr(instanceGroupDataAddr, "node_pools.gpu-workers.ready_count", "2"),
				resource.TestCheckResourceAttr(instanceGroupDataAddr, "progress_updated_at", "2026-09-21T00:00:00Z"),
				resource.TestCheckNoResourceAttr(instanceGroupDataAddr, "state_reason"),
				resource.TestCheckResourceAttr(instanceGroupDataAddr, "security_groups.#", "0"),
			),
		}},
	})
}

func TestAccFileStoreDataSource(t *testing.T) {
	const name = "acc-datasets-read"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(fileStoreType, fileStorePath),
		Steps: []resource.TestStep{{
			Config: fileStoreConfig(name, "acceptance", 1024, oneConsumer) + fmt.Sprintf(`
data %q "read" {
  id         = %q
  depends_on = [%s.test]
}
`, fileStoreType, name, fileStoreType),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(fileStoreDataAddr, "state", "active"),
				resource.TestCheckResourceAttr(fileStoreDataAddr, "capacity_gib", "1024"),
				resource.TestCheckResourceAttr(fileStoreDataAddr, "consumer_status.%", "1"),
				resource.TestCheckResourceAttr(fileStoreDataAddr, "consumer_status.10.0.0.31.state", "active"),
				resource.TestCheckResourceAttrSet(fileStoreDataAddr, "preferred_export_location.server"),
				resource.TestCheckNoResourceAttr(fileStoreDataAddr, "state_reason"),
			),
		}},
	})
}

// A data source read deferred to apply still knows its scope at plan, so a
// resource that takes the region from it doesn't see null become a value.
func TestAccClusterDataSourceScopeIsKnownWhenTheReadIsDeferred(t *testing.T) {
	const name = "acc-ds-deferred-scope"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
}

data %q "read" {
  id = %s.test.id
}

resource "terraform_data" "scope" {
  input = data.%s.read.region
}
`, clusterType, name, clusterTypeRef, clusterType, clusterType, clusterType),
			Check: resource.TestCheckResourceAttr("terraform_data.scope", "output", "us-texas-1"),
		}},
	})
}

// The suite asks for two rows a page, so the fake's seven cluster types are
// four pages. The fake answers a request without `offset` with its first two
// and no token, as the real list answers with its first page, so a provider
// that read that page would report two.
func TestAccClusterTypesDataSourceReadsEveryPage(t *testing.T) {
	clusterTypesType := provider.ResourceName("cluster_types")
	clusterTypesAddr := "data." + clusterTypesType + ".all"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		Steps: []resource.TestStep{{
			Config: fmt.Sprintf("data %q \"all\" {}\n", clusterTypesType),
			Check: resource.ComposeAggregateTestCheckFunc(
				resource.TestCheckResourceAttr(clusterTypesAddr, "cluster_types.#", "7"),
				resource.TestCheckResourceAttr(clusterTypesAddr, "cluster_types.0.id", "cpu-general"),
				resource.TestCheckResourceAttr(clusterTypesAddr, "cluster_types.6.id", "gpu-training"),
			),
		}},
	})
}
