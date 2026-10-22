package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// projectAPI answers the project operations from functions, and records what
// each request carried.
type projectAPI struct {
	mu      sync.Mutex
	calls   []string
	patched map[string]any
	reads   int
	get     func(reads int) (int, map[string]any)
	remove  func() (int, map[string]any)
	list    func(r *http.Request) map[string]any
	listed  int // rows the list has answered since the last offset 0
}

func (a *projectAPI) serve(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.calls = append(a.calls, r.Method+" "+r.URL.Path)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		reply := func(status int, body map[string]any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if body != nil {
				if err := json.NewEncoder(w).Encode(body); err != nil {
					t.Errorf("write: %v", err)
				}
			}
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/regions/global/projects":
			body := a.list(r)
			a.checkPage(t, r, body)
			reply(http.StatusOK, body)
		case r.Method == http.MethodGet:
			a.reads++
			reply(a.get(a.reads))
		case r.Method == http.MethodPatch:
			a.patched = map[string]any{}
			if err := json.Unmarshal(raw, &a.patched); err != nil {
				t.Errorf("PATCH body: %v", err)
			}
			reply(http.StatusOK, projectBody("active", "", ""))
		case r.Method == http.MethodDelete:
			reply(a.remove())
		}
	}))
}

// checkPage holds the project list to offset paging on the wire. The two
// styles are mutually exclusive, so a cursor parameter is a 422 upstream, and
// an offset that doesn't advance by the rows already read re-reads or skips a
// page.
func (a *projectAPI) checkPage(t *testing.T, r *http.Request, body map[string]any) {
	t.Helper()
	q := r.URL.Query()
	if q.Has("pageToken") || q.Has("pageSize") {
		t.Errorf("sent cursor paging on the project list: %s", r.URL.RawQuery)
	}
	if got := q.Get("limit"); got != strconv.Itoa(OffsetPageLimit) {
		t.Errorf("limit=%q, want %d", got, OffsetPageLimit)
	}
	if q.Get("offset") == "0" {
		a.listed = 0
	}
	if got := q.Get("offset"); got != strconv.Itoa(a.listed) {
		t.Errorf("offset=%q, want %d: the rows read so far", got, a.listed)
	}
	if rows, ok := body["projects"].([]any); ok {
		a.listed += len(rows)
	}
}

// projectBody is a Project as the platform writes it: displayName and
// description are always present, as "" when unset.
func projectBody(status, displayName, description string) map[string]any {
	return map[string]any{
		"id": "cp-production", "organizationId": "org-ml", "status": status,
		"displayName": displayName, "description": description,
		"selfLink": "/v1/regions/global/organizations/org-ml/projects/cp-production",
	}
}

func projectError(status int, reason, message string) map[string]any {
	return map[string]any{"error": map[string]any{
		"code": status, "reason": reason, "message": message, "requestId": "req-1",
	}}
}

func projectState(t *testing.T, set map[string]tftypes.Value) tfsdk.State {
	t.Helper()
	v := map[string]tftypes.Value{"id": str("cp-production"), "organization_id": str("org-ml")}
	for k, x := range set {
		v[k] = x
	}
	raw, s := testValue(t, &projectResource{}, v)
	return tfsdk.State{Raw: raw, Schema: s.Schema}
}

func deleteProject(t *testing.T, api *projectAPI) resource.DeleteResponse {
	t.Helper()
	server := api.serve(t)
	t.Cleanup(server.Close)
	state := projectState(t, nil)
	resp := resource.DeleteResponse{State: state}
	r := &projectResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	r.Delete(context.Background(), resource.DeleteRequest{State: state}, &resp)
	return resp
}

// The platform returns both fields as "" when they were never set. A
// configuration that leaves them out must read that back as null, or the
// first apply fails as an inconsistent result.
func TestProjectStateReadsAnUnsetFieldAsNull(t *testing.T) {
	empty := ""
	got := projectStateFrom(k0rdent.Project{Id: ptr("p"), DisplayName: &empty, Description: &empty}, projectModel{
		DisplayName: types.StringNull(), Description: types.StringValue(""),
	})
	if !got.DisplayName.IsNull() {
		t.Errorf("display_name %v, want null for a field the configuration left out", got.DisplayName)
	}
	if got.Description.IsNull() || got.Description.ValueString() != "" {
		t.Errorf("description %v, want the empty string the configuration wrote", got.Description)
	}
}

// Removing a field from configuration has to clear it upstream. A PATCH that
// leaves the key out keeps the old value, and removing both would send `{}`,
// which the contract refuses.
func TestProjectUpdateClearsWhatConfigurationRemoved(t *testing.T) {
	api := &projectAPI{}
	server := api.serve(t)
	defer server.Close()

	state := projectState(t, map[string]tftypes.Value{
		"display_name": str("Production"), "description": str("the old one"), "status": str("active"),
	})
	plan := projectState(t, map[string]tftypes.Value{"status": str("active")})
	resp := resource.UpdateResponse{State: state}
	r := &projectResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	r.Update(context.Background(), resource.UpdateRequest{
		State: state, Plan: tfsdk.Plan(plan),
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics.Errors())
	}
	if api.patched["displayName"] != "" || api.patched["description"] != "" {
		t.Errorf("PATCH sent %v, want both fields cleared with an empty string", api.patched)
	}
	var out projectModel
	resp.State.Get(context.Background(), &out)
	if !out.DisplayName.IsNull() || !out.Description.IsNull() {
		t.Errorf("state %v %v, want both null as planned", out.DisplayName, out.Description)
	}
}

// A destroy that finds the project already being deleted, by an earlier
// destroy that timed out or by somebody else, has nothing to start. It waits
// out the teardown that is already running.
func TestProjectDeleteResumesATeardownAlreadyRunning(t *testing.T) {
	api := &projectAPI{
		remove: func() (int, map[string]any) {
			return http.StatusConflict, projectError(http.StatusConflict, "CONFLICT",
				`conflict: project "cp-production" is already being deleted`)
		},
		get: func(reads int) (int, map[string]any) {
			if reads > 2 {
				return http.StatusNotFound, projectError(http.StatusNotFound, "NOT_FOUND", "not found")
			}
			return http.StatusOK, projectBody("deleting", "", "")
		},
	}
	if resp := deleteProject(t, api); resp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", resp.Diagnostics.Errors())
	}
	if api.reads < 3 {
		t.Errorf("%d reads, want the wait to run to the 404", api.reads)
	}
}

// The other 409 is a project that still holds clusters. That one is not
// waited on, and the platform's reason is what the operator needs to read.
func TestProjectDeleteReportsWhyItWasRefused(t *testing.T) {
	api := &projectAPI{
		remove: func() (int, map[string]any) {
			return http.StatusConflict, projectError(http.StatusConflict, "CONFLICT",
				`conflict: project "cp-production" still has 2 cluster(s); delete them before deleting the project`)
		},
		get: func(int) (int, map[string]any) { return http.StatusOK, projectBody("active", "", "") },
	}
	resp := deleteProject(t, api)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a refused delete reported success")
	}
	detail := resp.Diagnostics.Errors()[0].Detail()
	for _, want := range []string{"409", "still has 2 cluster(s)", "req-1"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail does not carry %q: %q", want, detail)
		}
	}
	// One before the DELETE, looking for a delete already running, and one
	// to tell the two 409s apart.
	if api.reads != 2 {
		t.Errorf("%d reads, want one before the delete, one to tell the two 409s apart and no wait", api.reads)
	}
}

// An empty status is not progress. Treating it as `deleting` would spend the
// whole timeout on a response the provider cannot read.
func TestProjectDeleteFailsOnAnEmptyStatus(t *testing.T) {
	api := &projectAPI{
		remove: func() (int, map[string]any) { return http.StatusNoContent, nil },
		get:    func(int) (int, map[string]any) { return http.StatusOK, projectBody("", "", "") },
	}
	resp := deleteProject(t, api)
	if !resp.Diagnostics.HasError() {
		t.Fatal("an empty status was waited on")
	}
	if d := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(d, `""`) {
		t.Errorf("the error does not name the empty status: %q", d)
	}
	// The read before the DELETE takes no view of the status. The wait's
	// first read is where it stops.
	if api.reads != 2 {
		t.Errorf("%d reads, want it to stop on the wait's first", api.reads)
	}
}

// A bare id is enough while one organization holds it. The list is searched
// rather than walked, and only an exact match counts: the search is a
// substring match.
func TestProjectOrganizationIsFoundFromABareID(t *testing.T) {
	api := &projectAPI{list: func(r *http.Request) map[string]any {
		if got := r.URL.Query().Get("search"); got != "cp-production" {
			t.Errorf("search=%q, want the id", got)
		}
		return map[string]any{"totalCount": 2, "projects": []any{
			map[string]any{"id": "cp-production-old", "organizationId": "org-other"},
			map[string]any{"id": "cp-production", "organizationId": "org-ml"},
		}}
	}}
	server := api.serve(t)
	defer server.Close()

	meta := testClusterMeta(t, server.URL, time.Minute)
	var diags = resource.ImportStateResponse{}.Diagnostics
	org, ok := findProjectOrganization(context.Background(), meta, "cp-production", &diags)
	if !ok || org != "org-ml" {
		t.Fatalf("got %q %v (%v), want org-ml", org, ok, diags)
	}
}

// The contract promises an id is unique inside an organization, not across
// them. If two organizations hold the same id, taking the first would adopt
// the wrong one and let a later destroy delete it.
func TestProjectOrganizationLookupRefusesAnAmbiguousID(t *testing.T) {
	api := &projectAPI{list: func(*http.Request) map[string]any {
		return map[string]any{"totalCount": 2, "projects": []any{
			map[string]any{"id": "cp-production", "organizationId": "org-ml"},
			map[string]any{"id": "cp-production", "organizationId": "org-other"},
		}}
	}}
	server := api.serve(t)
	defer server.Close()

	var diags = resource.ImportStateResponse{}.Diagnostics
	org, ok := findProjectOrganization(context.Background(), testClusterMeta(t, server.URL, time.Minute),
		"cp-production", &diags)
	if ok || !diags.HasError() {
		t.Fatalf("an ambiguous id resolved to %q", org)
	}
	if d := diags.Errors()[0].Detail(); !strings.Contains(d, "org-ml") || !strings.Contains(d, "org-other") {
		t.Errorf("the error does not name both organizations: %q", d)
	}
}

// A list that leaves out `totalCount` still has to be read to the end: the
// second organization holding the id may be on the next page.
func TestProjectOrganizationLookupReadsPastAFullPage(t *testing.T) {
	api := &projectAPI{list: func(r *http.Request) map[string]any {
		if r.URL.Query().Get("offset") != "0" {
			return map[string]any{"projects": []any{
				map[string]any{"id": "cp-production", "organizationId": "org-other"},
			}}
		}
		rows := []any{map[string]any{"id": "cp-production", "organizationId": "org-ml"}}
		for i := 1; i < catalogPageSize; i++ {
			rows = append(rows, map[string]any{"id": fmt.Sprintf("cp-production-%d", i), "organizationId": "org-ml"})
		}
		return map[string]any{"projects": rows}
	}}
	server := api.serve(t)
	defer server.Close()

	var diags = resource.ImportStateResponse{}.Diagnostics
	org, ok := findProjectOrganization(context.Background(), testClusterMeta(t, server.URL, time.Minute),
		"cp-production", &diags)
	if ok {
		t.Fatalf("stopped after one page and resolved to %q", org)
	}
	if d := diags.Errors()[0].Detail(); !strings.Contains(d, "org-ml, org-other") {
		t.Errorf("the error does not name both organizations in order: %q", d)
	}
}

// The list data source had the same one-page stop as the lookup: without
// `totalCount` it returned the first 500 projects as if they were all of them.
func TestProjectsListReadsPastAFullPageWithoutATotal(t *testing.T) {
	api := &projectAPI{list: func(r *http.Request) map[string]any {
		if r.URL.Query().Get("offset") != "0" {
			return map[string]any{"projects": []any{map[string]any{"id": "p-last", "organizationId": "org-ml"}}}
		}
		var rows []any
		for i := 0; i < catalogPageSize; i++ {
			rows = append(rows, map[string]any{"id": fmt.Sprintf("p-%04d", i), "organizationId": "org-ml"})
		}
		return map[string]any{"projects": rows}
	}}
	server := api.serve(t)
	defer server.Close()

	ctx := context.Background()
	ds := &projectsDataSource{meta: testClusterMeta(t, server.URL, time.Minute)}
	var s datasource.SchemaResponse
	ds.Schema(ctx, datasource.SchemaRequest{}, &s)
	typ, ok := s.Schema.Type().TerraformType(ctx).(tftypes.Object)
	if !ok {
		t.Fatal("the schema is not an object")
	}
	vals := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		vals[name] = tftypes.NewValue(at, nil)
	}
	raw := tftypes.NewValue(typ, vals)
	resp := datasource.ReadResponse{State: tfsdk.State{Raw: raw, Schema: s.Schema}}
	ds.Read(ctx, datasource.ReadRequest{Config: tfsdk.Config{Raw: raw, Schema: s.Schema}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	var ids types.List
	resp.State.GetAttribute(ctx, path.Root("ids"), &ids)
	if got := len(ids.Elements()); got != catalogPageSize+1 {
		t.Errorf("%d projects, want %d", got, catalogPageSize+1)
	}
}

// The import lookup and the region check read the project list through the
// same offset pager as every other list, so the acceptance suite's smaller
// page reaches them too, and a project past the first page is still found.
func TestProjectLookupsPageWithTheLoweredLimit(t *testing.T) {
	was := OffsetPageLimit
	OffsetPageLimit = 2
	t.Cleanup(func() { OffsetPageLimit = was })
	rows := []any{
		map[string]any{"id": "cp-production-0", "organizationId": "org-ml", "region": "us-oregon-1"},
		map[string]any{"id": "cp-production-1", "organizationId": "org-ml", "region": "us-oregon-1"},
		map[string]any{"id": "cp-production-2", "organizationId": "org-ml", "region": "us-oregon-1"},
		map[string]any{"id": "cp-production-3", "organizationId": "org-ml", "region": "us-oregon-1"},
		map[string]any{"id": "cp-production", "organizationId": "org-ml", "region": "us-texas-1"},
	}
	api := &projectAPI{list: func(r *http.Request) map[string]any {
		offset, err := strconv.Atoi(r.URL.Query().Get("offset"))
		if err != nil || offset > len(rows) {
			t.Errorf("offset=%q", r.URL.Query().Get("offset"))
			return map[string]any{"totalCount": len(rows), "projects": []any{}}
		}
		end := min(offset+OffsetPageLimit, len(rows))
		return map[string]any{"totalCount": len(rows), "projects": rows[offset:end]}
	}}
	server := api.serve(t)
	defer server.Close()
	meta := testClusterMeta(t, server.URL, time.Minute)

	var diags = resource.ImportStateResponse{}.Diagnostics
	if org, ok := findProjectOrganization(context.Background(), meta, "cp-production", &diags); !ok || org != "org-ml" {
		t.Errorf("lookup got %q %v (%v), want org-ml", org, ok, diags)
	}
	if region, ok := meta.projectRegion(context.Background(), "cp-production"); !ok || region != "us-texas-1" {
		t.Errorf("region check got %q %v, want us-texas-1", region, ok)
	}
	if len(api.calls) != 6 {
		t.Errorf("%d list calls, want three pages of two for each: %v", len(api.calls), api.calls)
	}
}

// At the bound the lookup can't tell whether another organization holds the
// id, and naming the organization is the way round that.
func TestProjectOrganizationLookupSaysHowToGetPastTheBound(t *testing.T) {
	rows := make([]any, catalogPageSize)
	for i := range rows {
		rows[i] = map[string]any{"id": fmt.Sprintf("cp-production-%d", i), "organizationId": "org-ml"}
	}
	api := &projectAPI{list: func(*http.Request) map[string]any { return map[string]any{"projects": rows} }}
	server := api.serve(t)
	defer server.Close()

	var diags = resource.ImportStateResponse{}.Diagnostics
	_, ok := findProjectOrganization(context.Background(), testClusterMeta(t, server.URL, time.Minute),
		"cp-production", &diags)
	if ok || !diags.HasError() {
		t.Fatal("a list that never ended resolved")
	}
	e := diags.Errors()[0]
	if e.Summary() != "The list of projects did not end" || !strings.Contains(e.Detail(), "Name the organization") {
		t.Errorf("got %q: %q, want the bound and how to get past it", e.Summary(), e.Detail())
	}
}

func TestProjectOrganizationLookupSaysWhenNothingMatches(t *testing.T) {
	api := &projectAPI{list: func(*http.Request) map[string]any {
		return map[string]any{"totalCount": 1, "projects": []any{
			map[string]any{"id": "cp-production-old", "organizationId": "org-other"},
		}}
	}}
	server := api.serve(t)
	defer server.Close()

	var diags = resource.ImportStateResponse{}.Diagnostics
	_, ok := findProjectOrganization(context.Background(), testClusterMeta(t, server.URL, time.Minute),
		"cp-production", &diags)
	if ok || !diags.HasError() {
		t.Fatalf("a missing project was found: %v", diags)
	}
	if d := diags.Errors()[0].Detail(); !strings.Contains(d, "cp-production") {
		t.Errorf("the error does not name the id: %q", d)
	}
}
