package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The project credential mints its own token, and that token is refused what
// the k0rdent API v1.3.0-rc.3 refused a credential bound to one project, with
// the same bare 403. It still reads its own project's routes and the flat
// lists, which show it only its own.
func TestAProjectCredentialReachesOnlyItsProject(t *testing.T) {
	h, secrets := guardedFake(t)
	host := "127.0.0.1:" + testPort

	grant := grantJSON(map[string]string{
		"grant_type": "client_credentials", "client_id": projectClientID, "client_secret": secrets.ProjectClientSecret,
	})
	rec := serve(h, mintRequest(host, grant))
	var minted struct {
		AccessToken string `json:"access_token"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &minted) != nil ||
		minted.AccessToken != secrets.ProjectAccessToken {
		t.Fatalf("the project credential minted %d %q, want its own token", rec.Code, minted.AccessToken)
	}
	wrong := grantJSON(map[string]string{
		"grant_type": "client_credentials", "client_id": projectClientID, "client_secret": secrets.ClientSecret,
	})
	if rec := serve(h, mintRequest(host, wrong)); rec.Code != http.StatusUnauthorized {
		t.Errorf("the project client id minted with the run's secret: %d", rec.Code)
	}

	get := func(bearer, path string) (int, string) {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = host
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := serve(h, req)
		return rec.Code, strings.TrimSpace(rec.Body.String())
	}
	org := "/v1/regions/global/organizations/" + projectPrincipal.org
	for _, path := range []string{
		org,
		org + "/projects",
		org + "/projects/" + projectPrincipal.project,
		org + "/projects/" + projectPrincipal.project + "/quota",
		org + "/quota",
		org + "/iam/roles",
		org + "/iam/service-accounts",
		"/v1/regions/" + projectRegion + "/projects/example/networking/networks",
	} {
		if code, body := get(secrets.ProjectAccessToken, path); code != http.StatusForbidden || body != live403 {
			t.Errorf("GET %s answered %d %s, want %s", path, code, body, live403)
		}
		// The run's own credential is refused none of it.
		if code, _ := get(secrets.AccessToken, path); code == http.StatusForbidden {
			t.Errorf("GET %s refused the run's own credential", path)
		}
	}

	for _, path := range []string{
		"/v1/regions",
		"/v1/regions/" + projectRegion + "/projects/" + projectPrincipal.project + "/networking/networks",
		"/v1/regions/" + projectRegion + "/compute/cluster-types",
		"/v1/regions/global/notifications/webhooks",
	} {
		if code, body := get(secrets.ProjectAccessToken, path); code != http.StatusOK {
			t.Errorf("GET %s answered %d %s, want 200", path, code, body)
		}
	}

	// Both lists page one row at a time, so totalCount is what each shows.
	listed := func(bearer, path, key string) (int, []string) {
		code, body := get(bearer, path)
		if code != http.StatusOK {
			t.Fatalf("GET %s answered %d %s", path, code, body)
		}
		var page struct {
			TotalCount int `json:"totalCount"`
		}
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(body), &raw); err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		var items []struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(body), &page) != nil || json.Unmarshal(raw[key], &items) != nil {
			t.Fatalf("GET %s: %s", path, body)
		}
		var ids []string
		for _, it := range items {
			ids = append(ids, it.ID)
		}
		return page.TotalCount, ids
	}
	if n, ids := listed(secrets.ProjectAccessToken, "/v1/regions/global/projects", "projects"); n != 1 ||
		len(ids) != 1 || ids[0] != projectPrincipal.project {
		t.Errorf("the project list showed the project credential %d: %v, want only %s", n, ids, projectPrincipal.project)
	}
	if n, _ := listed(secrets.AccessToken, "/v1/regions/global/projects", "projects"); n < 2 {
		t.Errorf("the project list showed the run's credential %d projects", n)
	}
	if n, ids := listed(secrets.ProjectAccessToken, "/v1/regions/global/organizations", "organizations"); n != 1 ||
		len(ids) != 1 || ids[0] != projectPrincipal.org {
		t.Errorf("the organization list showed the project credential %d: %v, want only %s", n, ids, projectPrincipal.org)
	}
}
