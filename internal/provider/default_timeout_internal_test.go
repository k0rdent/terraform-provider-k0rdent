package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	k0rdent "github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// With no `timeouts` block, a create, its resume and a delete all run under
// the long default when the kind builds or frees hardware, and under
// operation_timeout otherwise.
func TestEachOperationRunsUnderItsKindsDefaultTimeout(t *testing.T) {
	const short, long = time.Hour, 4 * time.Hour
	all := []string{"create", "resume", "delete"}
	kinds := pendingKinds()
	kinds["project"] = pendingKind{
		build: func(m *Meta) resource.Resource { return &projectResource{meta: m} },
		plan: func(*testing.T, resource.Resource) map[string]tftypes.Value {
			return map[string]tftypes.Value{"id": str("p1"), "organization_id": str("o1")}
		},
	}
	for kind, c := range map[string]struct {
		want time.Duration
		ops  []string
	}{
		"cluster": {long, all}, "instance group": {long, all},
		"file store": {short, all}, "network": {short, all}, "security group": {short, all},
		"subnet": {short, all}, "VPC peering": {short, all},
		// A project is only waited on while it is deleted.
		"project": {short, []string{"delete"}},
	} {
		want := c.want
		for _, op := range c.ops {
			t.Run(kind+" "+op, func(t *testing.T) {
				k := kinds[kind]
				active := map[string]any{}
				for name, v := range k.body {
					active[name] = v
				}
				active["state"] = "active"
				var deleted atomic.Bool
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.Method == http.MethodDelete:
						deleted.Store(true)
						w.WriteHeader(http.StatusAccepted)
					case deleted.Load():
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusNotFound)
						writeJSON(w, map[string]any{"error": map[string]any{"code": 404, "message": "not found"}})
					case r.Method == http.MethodPost:
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusCreated)
						writeJSON(w, active)
					default:
						writeJSON(w, active)
					}
				}))
				t.Cleanup(server.Close)
				seen := &deadlineRecorder{base: testTransport(t)}
				client, err := k0rdent.New(server.URL, k0rdent.WithTransport(seen))
				if err != nil {
					t.Fatalf("new test client: %v", err)
				}
				r := k.build(&Meta{Client: client, Region: "test", Project: "test",
					Wait: waitConfig{Timeout: short, Long: long, Interval: time.Microsecond}})

				start := time.Now()
				diags := runOperation(t, r, k.plan(t, r), op)
				end := time.Now()
				if diags.HasError() {
					t.Fatalf("%s: %v", op, diags.Errors())
				}
				seen.mu.Lock()
				defer seen.mu.Unlock()
				if len(seen.sent) == 0 {
					t.Fatalf("the %s sent nothing", op)
				}
				for _, s := range seen.sent {
					if !s.bounded || s.at.Before(start.Add(want)) || s.at.After(end.Add(want)) {
						t.Errorf("%s was sent with a deadline %s out, want %s",
							s.request, s.at.Sub(start).Round(time.Minute), want)
					}
				}
			})
		}
	}
}

// runOperation runs op on r: a create from values, the resume of a create
// state last read as `creating`, or a delete of values.
func runOperation(t *testing.T, r resource.Resource, values map[string]tftypes.Value, op string) diag.Diagnostics {
	t.Helper()
	ctx := context.Background()
	raw, s := testValue(t, r, values)
	switch op {
	case "create":
		resp := resource.CreateResponse{State: tfsdk.State{Raw: raw, Schema: s.Schema}}
		r.Create(ctx, resource.CreateRequest{Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}}, &resp)
		return resp.Diagnostics
	case "resume":
		values["state"] = str("creating")
		raw, _ = testValue(t, r, values)
		resp := resource.UpdateResponse{State: tfsdk.State{Raw: raw, Schema: s.Schema}}
		r.Update(ctx, resource.UpdateRequest{State: tfsdk.State{Raw: raw, Schema: s.Schema},
			Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}}, &resp)
		return resp.Diagnostics
	default:
		resp := resource.DeleteResponse{State: tfsdk.State{Raw: raw, Schema: s.Schema}}
		r.Delete(ctx, resource.DeleteRequest{State: tfsdk.State{Raw: raw, Schema: s.Schema}}, &resp)
		return resp.Diagnostics
	}
}
