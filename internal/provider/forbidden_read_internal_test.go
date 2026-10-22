package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// A refresh answered with 403 must fail, and must leave the resource in state.
// Dropping it would read an authorization failure as "deleted outside
// Terraform", and the next plan would propose creating it again, which is the
// destructive way to confuse the two. The 404 half shows each Read reaches the
// API at all, so the 403 half can't pass because a Read stopped before asking.
func TestAReadRefusedWith403KeepsTheResource(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if err := json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": status, "reason": http.StatusText(status), "message": "refused", "requestId": "req-1",
			}}); err != nil {
				t.Errorf("write: %v", err)
			}
		}))
		meta := testClusterMeta(t, server.URL, time.Minute)

		for _, newResource := range (&aiProvider{}).Resources(context.Background()) {
			r := newResource()
			var md resource.MetadataResponse
			r.Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)

			configurable, ok := r.(resource.ResourceWithConfigure)
			if !ok {
				t.Fatalf("%s takes no provider data, so it has no client to read with", md.TypeName)
			}
			configurable.Configure(context.Background(), resource.ConfigureRequest{ProviderData: meta}, &resource.ConfigureResponse{})

			raw, s := testValue(t, r, identityOf(t, r))
			st := tfsdk.State{Raw: raw, Schema: s.Schema}
			resp := resource.ReadResponse{State: st}
			r.Read(context.Background(), resource.ReadRequest{State: st}, &resp)

			switch status {
			case http.StatusForbidden:
				if !resp.Diagnostics.HasError() {
					t.Errorf("%s: a 403 on refresh reported no error", md.TypeName)
				}
				if resp.State.Raw.IsNull() {
					t.Errorf("%s: a 403 on refresh dropped the resource from state", md.TypeName)
				}
			case http.StatusNotFound:
				if readsAList[md.TypeName] {
					// A 404 on the list says the list is missing, not the
					// subscription, so it's an error that keeps the resource.
					// That error is the proof the Read asked.
					if !resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
						t.Errorf("%s: a 404 on the list read as the resource being gone: %v",
							md.TypeName, resp.Diagnostics)
					}
					continue
				}
				if !resp.State.Raw.IsNull() {
					t.Errorf("%s: a 404 on refresh kept the resource, so this test can't tell a 403 apart: %v",
						md.TypeName, resp.Diagnostics)
				}
			}
		}
		server.Close()
	}
}

// readsAList names the resources whose read is a list filtered by id, because
// the contract has no read of one. Only the item's absence from a list that
// answered means it's gone.
var readsAList = map[string]bool{ResourceName(webhookSubscriptionResourceSuffix): true}

// identityOf fills whatever of id, region, project and the parent references a
// resource declares, so its Read has something to ask the API for. The values
// don't matter: the server refuses every request.
func identityOf(t *testing.T, r resource.Resource) map[string]tftypes.Value {
	t.Helper()
	var s resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &s)
	out := map[string]tftypes.Value{}
	for _, name := range []string{"id", "region", "project", "organization_id", "network", "vpc", "vpc_id", "network_id", "service_account_id"} {
		a, ok := s.Schema.Attributes[name]
		if !ok {
			continue
		}
		if a.GetType().TerraformType(context.Background()).Is(tftypes.String) {
			out[name] = str("x")
		}
	}
	return out
}
