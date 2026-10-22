package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// failingCreate answers the create with body and every read with a failed state.
func failingCreate(t *testing.T, body map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
}

// A create whose wait fails leaves a tainted row. Its scope has to be in that
// row, or the next refresh reads the provider's project, gets a 404, and drops
// a resource that is still running.
func assertCreateKeepsScope(t *testing.T, r resource.Resource, set map[string]tftypes.Value, create func(resource.CreateRequest, *resource.CreateResponse)) {
	t.Helper()
	planRaw, s := testValue(t, r, set)
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(planRaw.Type(), nil), Schema: s.Schema}}
	create(resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("the wait was meant to fail")
	}
	if got := resp.Diagnostics.Errors()[0].Summary(); !strings.HasSuffix(got, "did not become active") {
		t.Fatalf("the create failed before its wait: %s", got)
	}
	for attr, want := range map[string]string{"region": "test", "project": "elsewhere"} {
		var got types.String
		resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root(attr), &got)...)
		if got.ValueString() != want {
			t.Errorf("%s in the tainted row = %s, want %q", attr, got, want)
		}
	}
}

func TestClusterCreateKeepsScopeWhenTheWaitFails(t *testing.T) {
	server := failingCreate(t, map[string]any{
		"id": "c1", "clusterType": "/v1/regions/test/compute/cluster-types/test",
		"nodePools": []any{}, "nodeCount": 0, "state": "failed", "securityGroups": []any{},
		"selfLink":  "/v1/regions/test/projects/elsewhere/compute/clusters/c1",
		"uid":       "00000000-0000-4000-8000-00000000000a",
		"createdAt": "2026-09-22T00:00:00Z", "updatedAt": "2026-09-22T00:00:00Z",
		"health": map[string]string{"state": "unknown"},
	})
	defer server.Close()
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	assertCreateKeepsScope(t, r, map[string]tftypes.Value{
		"id": str("c1"), "cluster_type": str("/v1/regions/test/compute/cluster-types/test"),
		"region": str("test"), "project": str("elsewhere"),
	}, func(req resource.CreateRequest, resp *resource.CreateResponse) {
		r.Create(context.Background(), req, resp)
	})
}

func TestInstanceGroupCreateKeepsScopeWhenTheWaitFails(t *testing.T) {
	server := failingCreate(t, map[string]any{
		"id": "g1", "clusterType": "/v1/regions/test/compute/cluster-types/test",
		"nodePools": []any{}, "state": "failed",
		"selfLink": "/v1/regions/test/projects/elsewhere/compute/instance-groups/g1",
	})
	defer server.Close()
	r := &instanceGroupResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	pool := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"size": tftypes.Number}}
	assertCreateKeepsScope(t, r, map[string]tftypes.Value{
		"id": str("g1"), "cluster_type": str("/v1/regions/test/compute/cluster-types/test"),
		"region": str("test"), "project": str("elsewhere"),
		"node_pool": tftypes.NewValue(tftypes.Map{ElementType: pool}, map[string]tftypes.Value{
			"workers": tftypes.NewValue(pool, map[string]tftypes.Value{"size": tftypes.NewValue(tftypes.Number, 1)}),
		}),
	}, func(req resource.CreateRequest, resp *resource.CreateResponse) {
		r.Create(context.Background(), req, resp)
	})
}

// A create whose wait fails keeps the plan in its tainted row, not just the id
// and scope. With node_pool null there, the refresh after `terraform untaint`
// adopted the control plane's pool too, and step 7 of the walkthrough
// planned dropping it: exit 2 on a cluster the provider had just made.
func TestATimedOutClusterCreateRefreshesToWhatWasConfigured(t *testing.T) {
	server := failingCreate(t, map[string]any{
		"id": "c1", "clusterType": "/v1/regions/test/compute/cluster-types/test",
		"nodePools": []any{
			map[string]any{"id": "control-plane", "nodeCount": 1},
			map[string]any{"id": "workers", "nodeCount": 1},
		},
		"nodeCount": 2, "state": "failed", "securityGroups": []any{},
		"selfLink":  "/v1/regions/test/projects/test/compute/clusters/c1",
		"uid":       "00000000-0000-4000-8000-00000000000a",
		"createdAt": "2026-09-22T00:00:00Z", "updatedAt": "2026-09-22T00:00:00Z",
		"health": map[string]string{"state": "unknown"},
	})
	defer server.Close()
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	values := clusterValues("/v1/regions/test/compute/cluster-types/test", "", 1, "")
	values["display_name"] = str("Terraform walkthrough")
	planRaw, s := testValue(t, r, values)
	created := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(planRaw.Type(), nil), Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}, &created)
	if !created.Diagnostics.HasError() {
		t.Fatal("the wait was meant to fail")
	}
	if got := stateAttr(t, created.State, "display_name"); got != "Terraform walkthrough" {
		t.Errorf("display_name in the tainted row = %q, want the plan's", got)
	}

	read := resource.ReadResponse{State: created.State}
	r.Read(context.Background(), resource.ReadRequest{State: created.State}, &read)
	if read.Diagnostics.HasError() {
		t.Fatalf("Read: %v", read.Diagnostics.Errors())
	}
	var pools map[string]types.Object
	read.Diagnostics.Append(read.State.GetAttribute(context.Background(), path.Root("node_pool"), &pools)...)
	if _, adopted := pools["control-plane"]; adopted || len(pools) != 1 {
		t.Errorf("node_pool after the refresh = %v, want only the configured workers", pools)
	}
}
