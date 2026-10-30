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
// that's empty. An organization created before a name and a description were
// required holds neither, and the platform sends "" for both. The row the
// organization's admin would get, as the platform sent it on v1.3.0-rc.4,
// keeps its text.
func TestAReducedOrganizationRowReadsNoDescription(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/regions/global/organizations" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		writeTestJSON(t, w, http.StatusOK, map[string]any{"totalCount": 3, "organizations": []any{
			map[string]any{"id": "reduced", "displayName": "Kind Org", "status": "active",
				"selfLink": "/v1/regions/global/organizations/reduced", "description": "", "labels": map[string]any{}},
			map[string]any{"id": "full", "displayName": "Kind Org", "status": "active",
				"selfLink": "/v1/regions/global/organizations/full", "memberCount": 1, "labels": map[string]any{},
				"description": "Operator org for the kind real-auth (mock IdP) path"},
			map[string]any{"id": "legacy", "displayName": "", "status": "active",
				"selfLink": "/v1/regions/global/organizations/legacy", "memberCount": 1, "labels": map[string]any{},
				"description": ""},
		}})
	}))
	t.Cleanup(server.Close)

	resp := readTestDataSource(t, &organizationsDataSource{meta: testClusterMeta(t, server.URL, time.Minute)}, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	var got organizationsModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	if resp.Diagnostics.HasError() || len(got.Organizations) != 3 {
		t.Fatalf("state: %v, %d rows", resp.Diagnostics.Errors(), len(got.Organizations))
	}
	full, legacy, reduced := got.Organizations[0], got.Organizations[1], got.Organizations[2]
	if !reduced.Description.IsNull() {
		t.Errorf("the reduced row's description reads %s, want null", reduced.Description)
	}
	if reduced.DisplayName.ValueString() != "Kind Org" || reduced.Status.ValueString() != "active" {
		t.Errorf("the reduced row's identity reads as %+v", reduced)
	}
	if full.Description.ValueString() != "Operator org for the kind real-auth (mock IdP) path" {
		t.Errorf("the full row's description reads %s", full.Description)
	}
	if !legacy.DisplayName.IsNull() || !legacy.Description.IsNull() {
		t.Errorf("an organization with no name or description reads %s and %s, want null for both",
			legacy.DisplayName, legacy.Description)
	}
}

// An organization read by id with no name or description reads null for
// both, as the same organization does in the list.
func TestAnOrganizationWithNoDescriptionReadsNull(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, w, http.StatusOK, map[string]any{
			"id": "o", "displayName": "", "status": "active", "description": "", "labels": map[string]any{},
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
	if !got.DisplayName.IsNull() {
		t.Errorf("the display name reads %s, want null", got.DisplayName)
	}
}

// projectRows answers the project list with one project that has a name and a
// description and one that has neither, which the platform sends as "".
func projectRows(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/regions/global/projects" {
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		writeTestJSON(t, w, http.StatusOK, map[string]any{"totalCount": 2, "projects": []any{
			map[string]any{"id": "bare", "organizationId": "o", "region": "r", "status": "active",
				"displayName": "", "description": ""},
			map[string]any{"id": "named", "organizationId": "o", "region": "r", "status": "active",
				"displayName": "Named", "description": "Default project for the kind real-auth e2e path"},
		}})
	}))
	t.Cleanup(server.Close)
	return server
}

// A project with no name or description reads null for both in the list.
func TestAProjectWithNoNameOrDescriptionReadsNullInTheList(t *testing.T) {
	resp := readTestDataSource(t, &projectsDataSource{meta: testClusterMeta(t, projectRows(t).URL, time.Minute)}, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	var got projectsModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	if resp.Diagnostics.HasError() || len(got.Projects) != 2 {
		t.Fatalf("state: %v, %d rows", resp.Diagnostics.Errors(), len(got.Projects))
	}
	bare, named := got.Projects[0], got.Projects[1]
	if !bare.DisplayName.IsNull() || !bare.Description.IsNull() {
		t.Errorf("a project with no name or description reads %s and %s, want null for both",
			bare.DisplayName, bare.Description)
	}
	if named.DisplayName.ValueString() != "Named" ||
		named.Description.ValueString() != "Default project for the kind real-auth e2e path" {
		t.Errorf("a named project reads %s and %s", named.DisplayName, named.Description)
	}
}

// The same project read on its own reads null for both too.
func TestAProjectWithNoNameOrDescriptionReadsNull(t *testing.T) {
	resp := readTestDataSource(t, &projectDataSource{meta: testClusterMeta(t, projectRows(t).URL, time.Minute)},
		map[string]tftypes.Value{"id": str("bare")})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	var got projectDataSourceModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	if !got.DisplayName.IsNull() || !got.Description.IsNull() {
		t.Errorf("the project reads %s and %s, want null for both", got.DisplayName, got.Description)
	}
}
