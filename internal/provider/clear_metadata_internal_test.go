package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// testValue builds a resource value from r's schema, null except for set.
func testValue(t *testing.T, r resource.Resource, set map[string]tftypes.Value) (tftypes.Value, resource.SchemaResponse) {
	t.Helper()
	var s resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &s)
	object, ok := s.Schema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema is not an object")
	}
	values := make(map[string]tftypes.Value, len(object.AttributeTypes))
	for name, attrType := range object.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	for name, v := range set {
		values[name] = v
	}
	return tftypes.NewValue(object, values), s
}

// patchRecorder answers every PATCH and GET with body, and keeps the last
// PATCH it was sent.
type patchRecorder struct {
	mu   sync.Mutex
	sent map[string]any
}

func (p *patchRecorder) server(t *testing.T, body map[string]any) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read PATCH: %v", err)
			}
			p.mu.Lock()
			p.sent = map[string]any{}
			if err := json.Unmarshal(raw, &p.sent); err != nil {
				t.Errorf("PATCH body: %v", err)
			}
			p.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
}

// Removing an attribute from configuration has to clear it upstream. Sending
// nothing leaves the old value, the read returns it, and every apply fails.
func TestClusterUpdateClearsRemovedMetadata(t *testing.T) {
	var rec patchRecorder
	server := rec.server(t, map[string]any{
		"id": "c1", "clusterType": "/v1/regions/test/compute/cluster-types/test",
		"nodePools": []any{}, "nodeCount": 0, "state": "active", "securityGroups": []any{},
		"selfLink":  "/v1/regions/test/projects/test/compute/clusters/c1",
		"uid":       "00000000-0000-4000-8000-000000000009",
		"createdAt": "2026-09-22T00:00:00Z", "updatedAt": "2026-09-22T00:00:00Z",
		"health": map[string]string{"state": "unknown"},
	})
	defer server.Close()

	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	base := map[string]tftypes.Value{
		"id": str("c1"), "cluster_type": str("/v1/regions/test/compute/cluster-types/test"),
		"region": str("test"), "project": str("test"),
	}
	withMetadata := map[string]tftypes.Value{
		"display_name": str("old"), "description": str("old"),
		"tags": tftypes.NewValue(tftypes.Map{ElementType: tftypes.String},
			map[string]tftypes.Value{"team": str("ml")}),
	}
	for k, v := range base {
		withMetadata[k] = v
	}
	stateRaw, s := testValue(t, r, withMetadata)
	planRaw, _ := testValue(t, r, base)

	resp := resource.UpdateResponse{State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}
	r.Update(context.Background(), resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics.Errors())
	}
	for _, field := range []string{"displayName", "description"} {
		if got, ok := rec.sent[field]; !ok || got != "" {
			t.Errorf("%s = %#v in the PATCH, want an empty string", field, got)
		}
	}
	if tags, ok := rec.sent["tags"].(map[string]any); !ok || len(tags) != 0 {
		t.Errorf("tags = %#v in the PATCH, want an empty object", rec.sent["tags"])
	}
}

func TestInstanceGroupUpdateClearsRemovedMetadata(t *testing.T) {
	var rec patchRecorder
	server := rec.server(t, map[string]any{
		"id": "g1", "clusterType": "/v1/regions/test/compute/cluster-types/test",
		"nodePools": []any{}, "state": "active",
		"selfLink": "/v1/regions/test/projects/test/compute/instance-groups/g1",
	})
	defer server.Close()

	r := &instanceGroupResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	base := map[string]tftypes.Value{
		"id": str("g1"), "cluster_type": str("/v1/regions/test/compute/cluster-types/test"),
		"region": str("test"), "project": str("test"),
	}
	withMetadata := map[string]tftypes.Value{"display_name": str("old"), "description": str("old")}
	for k, v := range base {
		withMetadata[k] = v
	}
	stateRaw, s := testValue(t, r, withMetadata)
	planRaw, _ := testValue(t, r, base)

	resp := resource.UpdateResponse{State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}
	r.Update(context.Background(), resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics.Errors())
	}
	for _, field := range []string{"displayName", "description"} {
		if got, ok := rec.sent[field]; !ok || got != "" {
			t.Errorf("%s = %#v in the PATCH, want an empty string", field, got)
		}
	}
}
