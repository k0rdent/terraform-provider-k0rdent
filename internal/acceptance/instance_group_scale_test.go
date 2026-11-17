package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
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

// sendFake sends a request the provider doesn't, and returns the fake's answer.
func sendFake(t *testing.T, method, path, body string) (int, []byte) {
	t.Helper()
	token, err := mintFakeToken()
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(method, endpoint+path, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := harnessHTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read: %v", method, path, err)
	}
	return resp.StatusCode, out
}

// fake-api-v1.4 makes the fake refuse a group's scale as API v1.4 does, with
// the reason at the top level as well as on details[]. Without it the reason
// is on details[] only, as in v1.3. TestAccInstanceGroupScaleWaitsOutTransientRefusals
// relies on that difference.
func TestAccTheFakesV14MarkerChangesTheRefusalsShape(t *testing.T) {
	if os.Getenv(resource.EnvTfAcc) == "" {
		skipTest(t, resource.EnvTfAcc+" isn't set, so there's no fake to ask")
	}
	t.Parallel()
	for name, tc := range map[string]struct{ marker, topLevel string }{
		"acc-shape-v13": {"", ""},
		"acc-shape-v14": {"fake-api-v1.4", "CONFLICT_IN_USE"},
	} {
		t.Run(name, func(t *testing.T) {
			path := instanceGroupPath(map[string]string{"region": "us-texas-1", "project": "demo", "id": name})
			create := fmt.Sprintf(`{"id":%q,"displayName":"acceptance %s","clusterType":%q,`+
				`"nodePools":[{"nodePool":"gpu-workers","size":1}]}`, name, tc.marker, clusterTypeRef)
			if code, out := sendFake(t, http.MethodPost, path[:strings.LastIndex(path, "/")], create); code != http.StatusCreated {
				t.Fatalf("create: %d %s", code, out)
			}
			t.Cleanup(func() { sendFake(t, http.MethodDelete, path, "") })
			// Still creating, so the scale is refused for the operation in flight.
			code, out := sendFake(t, http.MethodPost, path+"/scale", `{"nodePools":[{"nodePool":"gpu-workers","size":2}]}`)
			var refusal struct {
				Error struct {
					Reason  string `json:"reason"`
					Details []struct {
						Reason string `json:"reason"`
					} `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(out, &refusal); err != nil {
				t.Fatalf("scale answered %d %s: %v", code, out, err)
			}
			e := refusal.Error
			if code != http.StatusConflict || e.Reason != tc.topLevel || len(e.Details) != 1 || e.Details[0].Reason != "CONFLICT_IN_USE" {
				t.Errorf("scale answered %d with reason %q, want 409, %q and details[] CONFLICT_IN_USE: %s",
					code, e.Reason, tc.topLevel, out)
			}
		})
	}
}
