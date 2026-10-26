package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// The gateway admits the renamed-scope marker's organization on the two quota
// reads and on nothing else, as it would any organization that doesn't exist.
func TestOnlyTheQuotaReadsAnswerForTheRenamedScopeMarker(t *testing.T) {
	s := newState()
	secrets, err := newRunSecrets()
	if err != nil {
		t.Fatalf("newRunSecrets: %v", err)
	}
	mux := http.NewServeMux()
	s.routes(mux, secrets.auth)
	send := func(method, path string) (int, string) {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+secrets.AccessToken)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	org := "/v1/regions/global/organizations/org-" + quotaRenamedScope
	for _, path := range []string{org + "/quota", org + "/projects/team-a/quota"} {
		if code, body := send(http.MethodGet, path); code != http.StatusOK {
			t.Errorf("GET %s: %d %s, want it served", path, code, body)
		}
	}
	for _, path := range []string{org, org + "/projects", org + "/projects/team-a", org + "/iam/roles"} {
		if code, body := send(http.MethodGet, path); code != http.StatusForbidden || body != live403 {
			t.Errorf("GET %s: %d %s, want the gateway's 403", path, code, body)
		}
	}
}
