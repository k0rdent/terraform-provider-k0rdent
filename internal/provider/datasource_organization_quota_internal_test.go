package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// quotaViewSpelled is a project's quota view as a platform sends it, naming
// the organization's scope sent.
func quotaViewSpelled(t *testing.T, sent string) k0rdent.TenantQuotaView {
	t.Helper()
	var view k0rdent.TenantQuotaView
	body := `{"dimensions":[{"dimension":"node_count","unit":"count","enforced":true,"overLimit":false,
		"headroom":2,"bindingScope":"` + sent + `","scopes":[
		{"scope":"` + sent + `","targetId":"org-demo","limit":32,"used":6,"headroom":26,"overLimit":false},
		{"scope":"project","targetId":"team-a","limit":8,"used":6,"headroom":2,"overLimit":false}]}]}`
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatal(err)
	}
	return view
}

// A landscape runs a platform from before the quota API renamed the
// organization's scope or after it. Each data source reports the one name it
// documents whichever arrives, and leaves `project` alone.
func TestQuotaViewNamesTheOrganizationScopeOneWay(t *testing.T) {
	for _, reported := range []string{organizationScope, tenantScope} {
		for _, sent := range []string{tenantScope, organizationScope} {
			t.Run(reported+" from "+sent, func(t *testing.T) {
				var diags diag.Diagnostics
				_, dims := quotaViewFrom(context.Background(), quotaViewSpelled(t, sent), reported, &diags)
				if diags.HasError() {
					t.Fatalf("diags: %v", diags)
				}
				rows := map[string]quotaDimensionModel{}
				diags.Append(dims.ElementsAs(context.Background(), &rows, false)...)
				var scopes []quotaScopeModel
				diags.Append(rows["node_count"].Scopes.ElementsAs(context.Background(), &scopes, false)...)
				if diags.HasError() || len(scopes) != 2 {
					t.Fatalf("scopes=%v diags=%v", scopes, diags)
				}
				if got := rows["node_count"].BindingScope.ValueString(); got != reported {
					t.Errorf("binding_scope = %q, want %q", got, reported)
				}
				if got := scopes[0].Scope.ValueString(); got != reported {
					t.Errorf("scopes[0].scope = %q, want %q", got, reported)
				}
				if got := scopes[1].Scope.ValueString(); got != "project" {
					t.Errorf("scopes[1].scope = %q, want project", got)
				}
			})
		}
	}
}

// A scope neither name covers is the platform's to define, so it reads as
// sent rather than being taken for the organization.
func TestQuotaScopeAsLeavesOtherScopes(t *testing.T) {
	for _, s := range []string{"project", "region", ""} {
		if got := quotaScopeAs(s, organizationScope); got != s {
			t.Errorf("quotaScopeAs(%q) = %q", s, got)
		}
	}
}
