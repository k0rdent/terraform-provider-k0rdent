package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// clusterUnderID answers a GET of cluster c1 as the cluster with uid.
func clusterUnderID(t *testing.T, uid string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/compute/clusters/c1"):
			writeJSON(w, map[string]any{
				"id": "c1", "clusterType": "/v1/regions/test/compute/cluster-types/test",
				"nodePools": []any{map[string]any{"id": "workers", "nodeCount": 1}},
				"nodeCount": 1, "state": "active", "securityGroups": []any{},
				"displayName": "someone else's",
				"selfLink":    "/v1/regions/test/projects/test/compute/clusters/c1",
				"uid":         uid, "createdAt": "2026-10-23T00:00:00Z", "updatedAt": "2026-10-23T00:00:00Z",
				"health": map[string]string{"state": "healthy"},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/networking/vpcs"):
			writeJSON(w, map[string]any{"vpcs": []any{}})
		default:
			writeTestError(t, w, http.StatusNotFound, "not found")
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// A deleted cluster releases its id, so another apply can make a new one
// under it. A refresh drops the row rather than adopting that one, so a
// destroy can't delete it.
func TestARefreshDropsAClusterRecreatedUnderItsID(t *testing.T) {
	const ours, theirs = "00000000-0000-4000-8000-0000000000aa", "00000000-0000-4000-8000-0000000000bb"
	for _, tc := range []struct {
		name, held, answered string
		kept                 bool
	}{
		{"same cluster", ours, ours, true},
		{"another cluster under the id", ours, theirs, false},
		{"a row with none recorded takes the first", "", theirs, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &clusterResource{meta: testClusterMeta(t, clusterUnderID(t, tc.answered).URL, time.Minute)}
			raw, s := testValue(t, r, clusterValues("/v1/regions/test/compute/cluster-types/test", "", 1, "active"))
			st := tfsdk.State{Raw: raw, Schema: s.Schema}
			req := resource.ReadRequest{State: st}
			freshPrivate(t, &req.Private, nil)
			if d := recordUID(context.Background(), req.Private, tc.held); d.HasError() {
				t.Fatal(d)
			}
			resp := resource.ReadResponse{State: st}
			freshPrivate(t, &resp.Private, nil)
			r.Read(context.Background(), req, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("Read: %v", resp.Diagnostics.Errors())
			}
			if kept := !resp.State.Raw.IsNull(); kept != tc.kept {
				t.Fatalf("row kept = %v, want %v", kept, tc.kept)
			}
			if !tc.kept {
				if len(resp.Diagnostics.Warnings()) == 0 || !strings.Contains(resp.Diagnostics.Warnings()[0].Detail(), theirs) {
					t.Errorf("no warning naming the other cluster: %v", resp.Diagnostics)
				}
				return
			}
			if got, same := heldObject(context.Background(), resp.Private, tc.answered); !same || got != tc.answered {
				t.Errorf("recorded uid = %q, want %q", got, tc.answered)
			}
		})
	}
}

// Every kind with a configured id makes the same check, so each is driven
// through its own Read: the same object keeps the row, another drops it.
func TestEveryKindDropsAnObjectRecreatedUnderItsID(t *testing.T) {
	const ours, theirs = "00000000-0000-4000-8000-0000000000aa", "00000000-0000-4000-8000-0000000000bb"
	scoped := map[string]tftypes.Value{"id": str("x1"), "region": str("test"), "project": str("test")}
	with := func(extra map[string]tftypes.Value) map[string]tftypes.Value {
		out := map[string]tftypes.Value{}
		for k, v := range scoped {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for _, kind := range []struct {
		name  string
		build func(meta *Meta) resource.Resource
		state map[string]tftypes.Value
		body  map[string]any
	}{
		{"instance group", func(m *Meta) resource.Resource { return &instanceGroupResource{meta: m} }, scoped,
			map[string]any{"id": "x1", "state": "active", "nodePools": []any{}}},
		{"file store", func(m *Meta) resource.Resource { return &fileStoreResource{meta: m} }, scoped,
			map[string]any{"id": "x1", "state": "available"}},
		{"security group", func(m *Meta) resource.Resource { return &securityGroupResource{meta: m} }, scoped,
			map[string]any{"id": "x1", "state": "active"}},
		{"network", func(m *Meta) resource.Resource { return &networkResource{meta: m} }, scoped,
			map[string]any{"id": "x1", "type": "evpn", "state": "active"}},
		{"subnet", func(m *Meta) resource.Resource { return &subnetResource{meta: m} }, with(map[string]tftypes.Value{"network": str("n1")}),
			map[string]any{"id": "x1", "state": "active", "cidr": "10.0.0.0/24"}},
		{"VPC peering", func(m *Meta) resource.Resource { return &vpcPeeringResource{meta: m} }, with(map[string]tftypes.Value{"vpc": str("v1")}),
			map[string]any{"id": "x1", "state": "active"}},
		{"project", func(m *Meta) resource.Resource { return &projectResource{meta: m} },
			map[string]tftypes.Value{"id": str("x1"), "organization_id": str("org-1")},
			map[string]any{"id": "x1", "organizationId": "org-1", "region": "test"}},
	} {
		for _, tc := range []struct {
			name     string
			answered string
			kept     bool
		}{{"same object", ours, true}, {"another object", theirs, false}} {
			t.Run(kind.name+"/"+tc.name, func(t *testing.T) {
				body := map[string]any{"uid": tc.answered}
				for k, v := range kind.body {
					body[k] = v
				}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/x1") {
						writeJSON(w, body)
						return
					}
					writeJSON(w, map[string]any{})
				}))
				t.Cleanup(server.Close)
				r := kind.build(testClusterMeta(t, server.URL, time.Minute))
				raw, s := testValue(t, r, kind.state)
				req := resource.ReadRequest{State: tfsdk.State{Raw: raw, Schema: s.Schema}}
				freshPrivate(t, &req.Private, nil)
				if d := recordUID(context.Background(), req.Private, ours); d.HasError() {
					t.Fatal(d)
				}
				resp := resource.ReadResponse{State: req.State}
				freshPrivate(t, &resp.Private, nil)
				r.Read(context.Background(), req, &resp)
				if resp.Diagnostics.HasError() {
					t.Fatalf("Read: %v", resp.Diagnostics.Errors())
				}
				if kept := !resp.State.Raw.IsNull(); kept != tc.kept {
					t.Errorf("row kept = %v, want %v (%v)", kept, tc.kept, resp.Diagnostics)
				}
			})
		}
	}
}
