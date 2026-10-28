package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// The list reduces a row to identity when the credential's roles there don't
// read the organization's settings. That row's description is "", whatever
// the organization holds, so it reads as null rather than as a description
// that's empty. The row the same organization's admin would get, as the
// platform sent it on v1.3.0-rc.4, keeps its text.
func TestAReducedOrganizationRowReadsNoDescription(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/regions/global/organizations" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		writeTestJSON(t, w, http.StatusOK, map[string]any{"totalCount": 2, "organizations": []any{
			map[string]any{"id": "reduced", "displayName": "Kind Org", "status": "active",
				"selfLink": "/v1/regions/global/organizations/reduced", "description": "", "labels": map[string]any{}},
			map[string]any{"id": "full", "displayName": "Kind Org", "status": "active",
				"selfLink": "/v1/regions/global/organizations/full", "memberCount": 1, "labels": map[string]any{},
				"description": "Operator org for the kind real-auth (mock IdP) path"},
		}})
	}))
	t.Cleanup(server.Close)

	resp := readTestDataSource(t, &organizationsDataSource{meta: testClusterMeta(t, server.URL, time.Minute)}, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	var got organizationsModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	if resp.Diagnostics.HasError() || len(got.Organizations) != 2 {
		t.Fatalf("state: %v, %d rows", resp.Diagnostics.Errors(), len(got.Organizations))
	}
	full, reduced := got.Organizations[0], got.Organizations[1]
	if !reduced.Description.IsNull() {
		t.Errorf("the reduced row's description reads %s, want null", reduced.Description)
	}
	if reduced.DisplayName.ValueString() != "Kind Org" || reduced.Status.ValueString() != "active" {
		t.Errorf("the reduced row's identity reads as %+v", reduced)
	}
	if full.Description.ValueString() != "Operator org for the kind real-auth (mock IdP) path" {
		t.Errorf("the full row's description reads %s", full.Description)
	}
}

// An organization read by id with no description reads null, as the same
// organization does in the list.
func TestAnOrganizationWithNoDescriptionReadsNull(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, w, http.StatusOK, map[string]any{
			"id": "o", "displayName": "O", "status": "active", "description": "", "labels": map[string]any{},
		})
	}))
	t.Cleanup(server.Close)

	resp := readTestDataSource(t, &organizationDataSource{meta: testClusterMeta(t, server.URL, time.Minute)},
		map[string]tftypes.Value{"id": str("o")})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	var got organizationDataSourceModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	if !got.Description.IsNull() {
		t.Errorf("the description reads %s, want null", got.Description)
	}
}
