package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// resentDeleteAPI is one resource whose first DELETE is acted on but answered
// by the gateway with a 504, the way a backend that took too long to answer
// leaves it. The retry layer sends the DELETE again, and the resource, already
// `deleting`, refuses it with the 409 its contract documents. Reads say
// `deleting` once, then 404.
type resentDeleteAPI struct {
	// body is the resource as a read returns it in the given state.
	body func(state string) map[string]any
	// refusal is how the resent DELETE is refused: a 409, or for a kind
	// whose contract answers a repeated delete with 204, that.
	refusal int

	mu      sync.Mutex
	state   string
	deletes int
	reads   int
}

func (a *resentDeleteAPI) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch r.Method {
		case http.MethodDelete:
			a.deletes++
			switch {
			case a.state == "":
				writeTestError(t, w, http.StatusNotFound, "not found")
			case a.deletes == 1:
				a.state = "deleting"
				w.WriteHeader(http.StatusGatewayTimeout)
			case a.refusal == http.StatusNoContent:
				w.WriteHeader(http.StatusNoContent)
			default:
				writeTestJSON(t, w, http.StatusConflict, map[string]any{"error": map[string]any{
					"code": http.StatusConflict, "requestId": "req-409",
					"message": "conflict: it is deleting and cannot be deleted yet",
					"details": []map[string]any{{"reason": "CONFLICT_IN_USE"}},
				}})
			}
		case http.MethodGet:
			a.reads++
			if a.state == "" {
				writeTestError(t, w, http.StatusNotFound, "not found")
				return
			}
			writeTestJSON(t, w, http.StatusOK, a.body(a.state))
			if a.state == "deleting" {
				a.state = "" // gone on the next read
			}
		default:
			writeTestError(t, w, http.StatusMethodNotAllowed, r.Method+" "+r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// resendingMeta is a client whose transport is the provider's own retry layer,
// so the 504 is resent the way it is in a real apply, without the sleeps.
func resendingMeta(t *testing.T, endpoint string) *Meta {
	t.Helper()
	rt, _ := fastTransport(t)
	client, err := k0rdent.New(endpoint, k0rdent.WithTransport(rt))
	if err != nil {
		t.Fatalf("new test client: %v", err)
	}
	return &Meta{
		Client: client, Region: "test", Project: "test",
		Wait: waitConfig{Timeout: time.Minute, Interval: time.Microsecond, ProvisionalFailureWindow: time.Minute},
	}
}

func stateBody(field string) func(string) map[string]any {
	return func(state string) map[string]any { return map[string]any{"id": "x1", field: state} }
}

// The provider's own DELETE, resent after a 504, must not come back as a
// failed destroy. Every kind whose delete the platform can refuse while it's
// `deleting` reads the resource on a 409 and waits the delete out. The
// security group and the file store answer a repeated DELETE with 204 by
// their contract, and are held to both answers.
func TestADeleteResentAfterA504IsWaitedOut(t *testing.T) {
	type kind struct {
		name  string
		body  func(string) map[string]any
		set   map[string]tftypes.Value
		build func(*Meta) resource.Resource
	}
	kinds := []kind{
		{"instance group", func(s string) map[string]any {
			return map[string]any{"id": "x1", "clusterType": "/v1/regions/test/compute/cluster-types/t", "state": s}
		}, nil, func(m *Meta) resource.Resource { return &instanceGroupResource{meta: m} }},
		{"VPC peering", stateBody("state"), map[string]tftypes.Value{"vpc": str("v1")},
			func(m *Meta) resource.Resource { return &vpcPeeringResource{meta: m} }},
		{"subnet", stateBody("state"), map[string]tftypes.Value{"network": str("n1")},
			func(m *Meta) resource.Resource { return &subnetResource{meta: m} }},
		{"network", func(s string) map[string]any {
			return map[string]any{"id": "x1", "type": "evpn", "state": s, "evpn": map[string]any{"type": "l3vpn"}}
		}, nil, func(m *Meta) resource.Resource { return &networkResource{meta: m} }},
		{"security group", stateBody("state"), nil,
			func(m *Meta) resource.Resource { return &securityGroupResource{meta: m} }},
		{"file store", stateBody("state"), nil,
			func(m *Meta) resource.Resource { return &fileStoreResource{meta: m} }},
		{"project", stateBody("status"), map[string]tftypes.Value{"organization_id": str("org-1")},
			func(m *Meta) resource.Resource { return &projectResource{meta: m} }},
	}
	for _, k := range kinds {
		for _, refusal := range []int{http.StatusConflict, http.StatusNoContent} {
			t.Run(fmt.Sprintf("%s answers %d", k.name, refusal), func(t *testing.T) {
				api := &resentDeleteAPI{body: k.body, refusal: refusal, state: "active"}
				r := k.build(resendingMeta(t, api.serve(t).URL))
				set := map[string]tftypes.Value{"id": str("x1")}
				for name, v := range k.set {
					set[name] = v
				}
				raw, s := testValue(t, r, set)
				st := tfsdk.State{Raw: raw, Schema: s.Schema}
				resp := resource.DeleteResponse{State: st}
				r.Delete(context.Background(), resource.DeleteRequest{State: st}, &resp)

				if resp.Diagnostics.HasError() {
					t.Fatalf("destroy failed: %s", diagText(resp.Diagnostics))
				}
				if api.deletes != 2 {
					t.Errorf("%d DELETEs, want the first and the resend", api.deletes)
				}
				if api.state != "" {
					t.Errorf("the destroy returned while the resource was still %q", api.state)
				}
			})
		}
	}
}

// A 409 on a resource that isn't `deleting` is a real refusal, and is
// reported with the platform's own reason rather than waited on.
func TestADeleteRefusedForAnotherReasonIsStillReported(t *testing.T) {
	api := &resentDeleteAPI{body: func(s string) map[string]any {
		return map[string]any{"id": "x1", "clusterType": "/v1/regions/test/compute/cluster-types/t", "state": s}
	}, refusal: http.StatusConflict, state: "updating"}
	api.deletes = 1 // the first DELETE is the refused one
	r := &instanceGroupResource{meta: resendingMeta(t, api.serve(t).URL)}
	raw, s := testValue(t, r, map[string]tftypes.Value{"id": str("x1")})
	st := tfsdk.State{Raw: raw, Schema: s.Schema}
	resp := resource.DeleteResponse{State: st}
	r.Delete(context.Background(), resource.DeleteRequest{State: st}, &resp)

	if !resp.Diagnostics.HasError() {
		t.Fatal("a refused delete reported success")
	}
	detail := diagText(resp.Diagnostics)
	for _, want := range []string{"409", "CONFLICT_IN_USE", "req-409"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the diagnostic doesn't carry %q: %s", want, detail)
		}
	}
	// One before the DELETE, which finds no delete running, and the one that
	// told the refusal apart.
	if api.reads != 2 {
		t.Errorf("%d reads, want the one before the delete and the one that told the refusal apart", api.reads)
	}
}
