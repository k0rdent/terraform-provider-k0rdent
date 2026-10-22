package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func peeringBody(state string) map[string]any {
	return map[string]any{
		"id": "p1", "remoteVpc": "/v1/regions/test/projects/other/networking/vpcs/far",
		"state": state, "selfLink": "/v1/regions/test/projects/test/networking/vpcs/near/peerings/p1",
	}
}

func peeringPlan(t *testing.T, r resource.Resource, id tftypes.Value) (tfsdk.Plan, resource.SchemaResponse) {
	raw, s := testValue(t, r, map[string]tftypes.Value{
		"id": id, "vpc": str("near"), "region": str("test"), "project": str("elsewhere"),
		"remote_vpc": str("/v1/regions/test/projects/other/networking/vpcs/far"),
	})
	return tfsdk.Plan{Raw: raw, Schema: s.Schema}, s
}

// create504 answers the create with a 504 and every read with an active
// peering. echo says whether the peering read back carries the tags the
// create sent, as one this create made would, or none, as one made by
// someone else under the same id would.
func create504(t *testing.T, id tftypes.Value, echo bool) resource.CreateResponse {
	var mu sync.Mutex
	var sent map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodPost {
			var body struct {
				Tags map[string]string `json:"tags"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("read create: %v", err)
			}
			sent = body.Tags
			w.WriteHeader(http.StatusGatewayTimeout)
			if err := json.NewEncoder(w).Encode(map[string]any{"code": 504, "message": "workflow not dispatched"}); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		got := peeringBody("active")
		got["id"] = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if echo {
			got["tags"] = sent
		}
		if err := json.NewEncoder(w).Encode(got); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	r := &vpcPeeringResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	plan, s := peeringPlan(t, r, id)
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(plan.Raw.Type(), nil), Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: plan}, &resp)
	return resp
}

// A 504 on create means the peering was committed and settles on its own. It
// is read back, and the create token it carries shows it is this one.
func TestPeeringCreate504WithAnIdIsWaitedOut(t *testing.T) {
	resp := create504(t, str("p1"), true)
	if resp.Diagnostics.HasError() {
		t.Fatalf("a committed peering was reported as failed: %v", resp.Diagnostics.Errors())
	}
}

// With no id configured, the create sends a generated one, so the 504 no
// longer leaves a peering nothing can name.
func TestPeeringCreate504WithoutAnIdIsReadBackUnderTheGeneratedID(t *testing.T) {
	resp := create504(t, tftypes.NewValue(tftypes.String, nil), true)
	if resp.Diagnostics.HasError() {
		t.Fatalf("a committed peering was reported as failed: %v", resp.Diagnostics.Errors())
	}
	var id types.String
	resp.State.GetAttribute(context.Background(), path.Root("id"), &id)
	if !strings.HasPrefix(id.ValueString(), generatedIDPrefix) {
		t.Errorf("id in state = %s, want the generated one", id)
	}
}

// A peering found under the configured id without this create's token may
// be someone else's, so a 504 does not hand it over.
func TestPeeringCreate504DoesNotTakeAPeeringWithoutTheToken(t *testing.T) {
	resp := create504(t, str("p1"), false)
	assertRefusedWithImport(t, resp.Diagnostics, "terraform import <address> test/elsewhere/near/p1")
	if !resp.State.Raw.IsNull() {
		t.Errorf("state was written for a peering this create cannot show it made: %v", resp.State.Raw)
	}
}

func TestPeeringCreateKeepsScopeWhenTheWaitFails(t *testing.T) {
	server := failingCreate(t, peeringBody("failed"))
	defer server.Close()
	r := &vpcPeeringResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	assertCreateKeepsScope(t, r, map[string]tftypes.Value{
		"id": str("p1"), "vpc": str("near"), "region": str("test"), "project": str("elsewhere"),
		"remote_vpc": str("/v1/regions/test/projects/other/networking/vpcs/far"),
	}, func(req resource.CreateRequest, resp *resource.CreateResponse) {
		r.Create(context.Background(), req, resp)
	})
}
