package acceptance

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// The fake keeps a cluster with this in its display name creating until it is
// cancelled.
const neverSettles = "fake-never-settles"

func stuckClusterConfig(name string) string {
	return fmt.Sprintf(`
resource %q "test" {
  id           = %q
  cluster_type = %q
  display_name = %q
  timeouts {
    create = "1s"
  }
}
`, clusterType, name, clusterTypeRef, neverSettles)
}

// checkFakeStateNow is checkFakeState for a step's PreConfig.
func checkFakeStateNow(t *testing.T, name, want string) {
	t.Helper()
	if err := checkFakeState(name, want)(nil); err != nil {
		t.Error(err)
	}
}

// checkFakeState reads the cluster from the fake, bypassing the provider.
func checkFakeState(name, want string) resource.TestCheckFunc {
	return func(*terraform.State) error {
		status, body, err := readFake("/v1/regions/us-texas-1/projects/demo/compute/clusters/" + name)
		if err != nil {
			return err
		}
		if status != http.StatusOK {
			return fmt.Errorf("reading %s answered %d", name, status)
		}
		var got struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			return err
		}
		if got.State != want {
			return fmt.Errorf("the platform reports %s as %q, want %q", name, got.State, want)
		}
		return nil
	}
}

// A create that never settles times out, fails, and leaves the cluster in
// state, tainted and still creating. The plan that would replace it is
// refused. The destroy at the end has to cancel the create, because the
// platform refuses to delete a cluster mid-operation, and CheckDestroy proves
// the cluster is gone.
func TestAccDestroyCancelsACreateThatNeverSettled(t *testing.T) {
	const name = "acc-cancel"
	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: protoV6,
		CheckDestroy:             checkGone(clusterType, clusterPath),
		Steps: []resource.TestStep{
			{
				Config:      stuckClusterConfig(name),
				ExpectError: regexp.MustCompile(`(?s)is still being created.*terraform untaint <address>`),
			},
			{
				// The tainted cluster's replacement is refused at plan, so
				// nothing is destroyed until the test's own destroy.
				PreConfig:   func() { checkFakeStateNow(t, name, "creating") },
				Config:      stuckClusterConfig(name),
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)is still being created.*terraform untaint <address>.*allow_replace = true`),
			},
		},
	})
}
