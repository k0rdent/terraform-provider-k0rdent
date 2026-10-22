package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func readAccounts(t *testing.T, routes map[string]http.HandlerFunc, set map[string]tftypes.Value) ([]serviceAccountView, datasource.ReadResponse) {
	t.Helper()
	api := listAPI(t, routes)
	resp := readTestDataSource(t, &serviceAccountsDataSource{meta: testClusterMeta(t, api.URL, time.Minute)}, set)
	var got serviceAccountsModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	var views []serviceAccountView
	if !got.ServiceAccounts.IsNull() {
		resp.Diagnostics.Append(got.ServiceAccounts.ElementsAs(context.Background(), &views, false)...)
	}
	return views, resp
}

func readCredentials(t *testing.T, routes map[string]http.HandlerFunc, set map[string]tftypes.Value) ([]serviceAccountCredentialView, datasource.ReadResponse) {
	t.Helper()
	api := listAPI(t, routes)
	resp := readTestDataSource(t, &serviceAccountCredentialsDataSource{meta: testClusterMeta(t, api.URL, time.Minute)}, set)
	var got serviceAccountCredentialsModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	var views []serviceAccountCredentialView
	if !got.Issued.IsNull() {
		resp.Diagnostics.Append(got.Issued.ElementsAs(context.Background(), &views, false)...)
	}
	return views, resp
}

func ids[T any](rows []T, id func(T) string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = id(r)
	}
	return out
}

// With no scope, the search is the organization's own accounts and every
// project's, in id order, each found once.
func TestServiceAccountsSearchTheOrganizationAndEveryProject(t *testing.T) {
	t.Parallel()
	views, resp := readAccounts(t, map[string]http.HandlerFunc{
		"GET /v1/regions/global/projects": func(w http.ResponseWriter, r *http.Request) {
			if got := r.URL.Query().Get("organizationId"); got != "o1" {
				t.Errorf("projects listed for organization %q, want o1", got)
			}
			paged(t, "projects", true, map[string]any{"id": "demo", "organizationId": "o1"})(w, r)
		},
		"GET " + orgPath + "/iam/service-accounts": func(w http.ResponseWriter, r *http.Request) {
			q := r.URL.Query()
			switch q.Get("scope") + "/" + q.Get("scopeId") {
			case "organization/o1":
				paged(t, "serviceAccounts", true,
					map[string]any{"id": "sa-org", "displayName": "Org", "roles": []string{"iam-admin"}, "status": "active"})(w, r)
			case "project/demo":
				paged(t, "serviceAccounts", true, map[string]any{"id": "sa-demo", "displayName": "Demo"},
					map[string]any{"id": "sa-org", "displayName": "Org"})(w, r)
			default:
				t.Errorf("service accounts listed at %s", r.URL.RawQuery)
				paged(t, "serviceAccounts", true)(w, r)
			}
		},
	}, map[string]tftypes.Value{"organization_id": str("o1")})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	if got := strings.Join(ids(views, func(v serviceAccountView) string { return v.ID.ValueString() }), ","); got != "sa-demo,sa-org" {
		t.Errorf("found %s, want sa-demo,sa-org", got)
	}
	if views[1].Status.ValueString() != "active" || len(views[1].Roles.Elements()) != 1 {
		t.Errorf("sa-org read as %+v", views[1])
	}
}

// A named scope is searched alone.
func TestServiceAccountsAtOneScope(t *testing.T) {
	t.Parallel()
	views, resp := readAccounts(t, map[string]http.HandlerFunc{
		"GET " + orgPath + "/iam/service-accounts": func(w http.ResponseWriter, r *http.Request) {
			if q := r.URL.Query(); q.Get("scope") != "project" || q.Get("scopeId") != "demo" {
				t.Errorf("service accounts listed at %s", r.URL.RawQuery)
			}
			paged(t, "serviceAccounts", true, map[string]any{"id": "sa-demo", "displayName": "x"})(w, r)
		},
	}, map[string]tftypes.Value{"organization_id": str("o1"), "scope": str("project"), "scope_id": str("demo")})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	if len(views) != 1 || views[0].ID.ValueString() != "sa-demo" {
		t.Errorf("found %v, want sa-demo alone", views)
	}
}

// A project the project list found and that's gone by the time its accounts
// are listed is skipped and named in a warning, not a failure.
func TestServiceAccountsSkipAProjectThatWentAway(t *testing.T) {
	t.Parallel()
	views, resp := readAccounts(t, map[string]http.HandlerFunc{
		"GET /v1/regions/global/projects": paged(t, "projects", true,
			map[string]any{"id": "demo", "organizationId": "o1"},
			map[string]any{"id": "old", "organizationId": "o1", "status": "deleting"},
			map[string]any{"id": "vanished", "organizationId": "o1"}),
		"GET " + orgPath + "/iam/service-accounts": func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("scopeId") {
			case "old":
				t.Error("a project being deleted was searched")
				paged(t, "serviceAccounts", true)(w, r)
			case "vanished":
				writeTestJSON(t, w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404, "message": "not found"}})
			case "demo":
				paged(t, "serviceAccounts", true, map[string]any{"id": "sa-demo", "displayName": "x"})(w, r)
			default:
				paged(t, "serviceAccounts", true)(w, r)
			}
		},
	}, map[string]tftypes.Value{"organization_id": str("o1")})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	if len(views) != 1 || views[0].ID.ValueString() != "sa-demo" {
		t.Errorf("found %v, want sa-demo alone", views)
	}
	named := false
	for _, w := range resp.Diagnostics.Warnings() {
		named = named || strings.Contains(w.Summary(), "vanished")
	}
	if !named {
		t.Errorf("nothing named the project that went away: %v", resp.Diagnostics)
	}
}

// One account's credentials, without the expired ones unless asked, and
// never anything secret.
func TestServiceAccountCredentialsOfOneAccount(t *testing.T) {
	t.Parallel()
	routes := map[string]http.HandlerFunc{
		"GET " + orgPath + "/iam/service-accounts/sa-ci/credentials": paged(t, "credentials", true,
			map[string]any{"id": "cred-2", "status": "active", "expiresAt": "2027-01-31T00:00:00Z"},
			map[string]any{"id": "cred-1", "status": "active", "lastUsedIp": "192.0.2.7"},
			map[string]any{"id": "cred-old", "status": "expired"}),
	}
	set := map[string]tftypes.Value{"organization_id": str("o1"), "service_account_id": str("sa-ci")}
	views, resp := readCredentials(t, routes, set)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	if got := strings.Join(ids(views, func(v serviceAccountCredentialView) string { return v.ID.ValueString() }), ","); got != "cred-1,cred-2" {
		t.Errorf("found %s, want cred-1,cred-2", got)
	}
	if views[1].ExpiresAt.ValueString() != "2027-01-31T00:00:00Z" || views[0].ServiceAccountID.ValueString() != "sa-ci" ||
		views[0].LastUsedIP.ValueString() != "192.0.2.7" {
		t.Errorf("read as %+v", views)
	}

	set["status"] = str("expired")
	views, resp = readCredentials(t, routes, set)
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	if len(views) != 1 || views[0].ID.ValueString() != "cred-old" {
		t.Errorf(`status = "expired" found %v, want cred-old alone`, views)
	}
	for name := range serviceAccountCredentialViewAttributes() {
		if strings.Contains(name, "secret") {
			t.Errorf("a credential reports %s", name)
		}
	}
}

// Without an account, the credentials of every account the scope finds.
func TestServiceAccountCredentialsOfEveryAccountAtAScope(t *testing.T) {
	t.Parallel()
	views, resp := readCredentials(t, map[string]http.HandlerFunc{
		"GET " + orgPath + "/iam/service-accounts": paged(t, "serviceAccounts", true,
			map[string]any{"id": "sa-a", "displayName": "a"}, map[string]any{"id": "sa-b", "displayName": "b"}),
		"GET " + orgPath + "/iam/service-accounts/sa-a/credentials": paged(t, "credentials", true, map[string]any{"id": "cred-a"}),
		"GET " + orgPath + "/iam/service-accounts/sa-b/credentials": paged(t, "credentials", true, map[string]any{"id": "cred-b"}),
	}, map[string]tftypes.Value{"organization_id": str("o1"), "scope": str("organization")})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	got := ids(views, func(v serviceAccountCredentialView) string {
		return v.ServiceAccountID.ValueString() + "/" + v.ID.ValueString()
	})
	if strings.Join(got, ",") != "sa-a/cred-a,sa-b/cred-b" {
		t.Errorf("found %v", got)
	}
}

func validate(t *testing.T, ds datasource.DataSourceWithValidateConfig, set map[string]tftypes.Value) []string {
	t.Helper()
	ctx := context.Background()
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
	for name, v := range set {
		vals[name] = v
	}
	var resp datasource.ValidateConfigResponse
	ds.ValidateConfig(ctx, datasource.ValidateConfigRequest{Config: tfsdk.Config{Raw: tftypes.NewValue(typ, vals), Schema: s.Schema}}, &resp)
	var out []string
	for _, d := range resp.Diagnostics.Errors() {
		out = append(out, d.Summary())
	}
	return out
}

// A search whose scope can't be searched as written is refused before
// anything is sent, and one from a variable, unknown at validate and maybe
// null later, is let through.
func TestAServiceAccountScopeMustBeSearchable(t *testing.T) {
	t.Parallel()
	unknown := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	accounts, credentials := &serviceAccountsDataSource{}, &serviceAccountCredentialsDataSource{}
	for _, tc := range []struct {
		name    string
		ds      datasource.DataSourceWithValidateConfig
		set     map[string]tftypes.Value
		refused bool
	}{
		{"a project scope with no id", accounts, map[string]tftypes.Value{"scope": str("project")}, true},
		{"a scope id with no scope", accounts, map[string]tftypes.Value{"scope_id": str("demo")}, true},
		{"an account and a scope", credentials, map[string]tftypes.Value{"service_account_id": str("sa-ci"), "scope": str("organization")}, true},
		{"the organization scope alone", accounts, map[string]tftypes.Value{"scope": str("organization")}, false},
		{"an unknown scope beside a scope id", accounts, map[string]tftypes.Value{"scope": unknown, "scope_id": str("demo")}, false},
		{"an unknown scope beside an account", credentials, map[string]tftypes.Value{"service_account_id": str("sa-ci"), "scope": unknown}, false},
		{"an unknown account beside a scope", credentials, map[string]tftypes.Value{"service_account_id": unknown, "scope": str("organization")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.set["organization_id"] = str("o1")
			if errs := validate(t, tc.ds, tc.set); (len(errs) > 0) != tc.refused {
				t.Errorf("refused = %v (%v), want %v", len(errs) > 0, errs, tc.refused)
			}
		})
	}
}
