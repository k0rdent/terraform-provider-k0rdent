package acceptance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
)

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
