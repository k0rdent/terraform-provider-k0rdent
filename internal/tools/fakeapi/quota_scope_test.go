package main

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The quota reads name the organization's scope `tenant`, as the pinned
// contract does, and `organization` for an organization id carrying
// quotaRenamedScope. The provider's acceptance test reads both through this,
// so it proves nothing unless the two differ here.
func TestQuotaReadsNameTheOrganizationScopeEitherWay(t *testing.T) {
	s := newState()
	for org, want := range map[string]string{
		"org-demo":                 "tenant",
		"org-" + quotaRenamedScope: "organization",
	} {
		for name, h := range map[string]http.HandlerFunc{"organization": s.getTenantQuota, "project": s.getProjectQuota} {
			t.Run(org+" "+name, func(t *testing.T) {
				_, out := in(t, h, http.MethodGet, "global", "", map[string]string{"orgId": org, "id": "team-a"}, "")
				raw, err := json.Marshal(out)
				if err != nil {
					t.Fatal(err)
				}
				var view struct {
					Dimensions []struct {
						Dimension    string
						BindingScope string
						Scopes       []struct{ Scope string }
					}
				}
				if err := json.Unmarshal(raw, &view); err != nil || len(view.Dimensions) == 0 {
					t.Fatalf("view %s: %v", raw, err)
				}
				for _, row := range view.Dimensions {
					if got := row.Scopes[0].Scope; got != want {
						t.Errorf("%s scopes[0].scope = %s, want %s", row.Dimension, got, want)
					}
					if name == "organization" && row.BindingScope != "" && row.BindingScope != want {
						t.Errorf("%s bindingScope = %s, want %s", row.Dimension, row.BindingScope, want)
					}
				}
			})
		}
	}
}
