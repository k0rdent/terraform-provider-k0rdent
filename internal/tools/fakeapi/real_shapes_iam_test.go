package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Shapes from the run-3 drift triage on the global tier: organizations, their
// projects and roles, each answered as API v1.3.0-rc.5 does.

// getAt calls a handler on a URL with a query, with the path values its route
// would set.
func getAt(t *testing.T, h http.HandlerFunc, target string, path map[string]string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range path {
		req.SetPathValue(k, v)
	}
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", target, rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return out
}

// An organization answers its description, version, update time and both
// counts on the list and the get, and the get adds the project summary.
func TestAnOrganizationAnswersWhatTheRealOneDoes(t *testing.T) {
	s := newState()
	list := getAt(t, s.listOrganizations, "/?offset=0&limit=500", nil)
	for _, row := range listOf(t, list["organizations"]) {
		org := object(t, row)
		for _, key := range []string{"description", "version", "updatedAt", "memberCount", "pendingInviteCount"} {
			if _, ok := org[key]; !ok {
				t.Errorf("listed organization %v has no %s", org["id"], key)
			}
		}
	}

	got := getAt(t, s.getOrganization, "/", map[string]string{"orgId": "org-demo"})
	if got["description"] == "" || got["version"] == nil || got["updatedAt"] == nil {
		t.Errorf("org-demo: description %v, version %v, updatedAt %v", got["description"], got["version"], got["updatedAt"])
	}
	summary := object(t, got["projectSummary"])
	projects := listOf(t, summary["projects"])
	// The seeds and the test projects are all active and all org-demo's.
	total := float64(len(projectSeeds))
	if summary["totalCount"] != total || summary["activeCount"] != total || len(projects) != min(len(projectSeeds), projectSummaryPreview) {
		t.Errorf("projectSummary = %v, want %v projects, all active, and a preview of %d", summary, total, projectSummaryPreview)
	}
	for _, p := range projects {
		row := object(t, p)
		if row["id"] == nil || row["displayName"] == nil || row["status"] == nil {
			t.Errorf("preview row %v lacks id, displayName or status", row)
		}
	}

	empty := getAt(t, s.getOrganization, "/", map[string]string{"orgId": "org-empty"})
	if empty["description"] != "" {
		t.Errorf("org-empty description = %v, want \"\"", empty["description"])
	}
	if summary := object(t, empty["projectSummary"]); summary["totalCount"] != 0.0 || len(listOf(t, summary["projects"])) != 0 {
		t.Errorf("org-empty projectSummary = %v, want none", summary)
	}
}

// The organizations and projects lists page one row at a time, and echo the
// offset and limit asked for, or the default limit.
func TestAOneRowPageEchoesOffsetAndLimit(t *testing.T) {
	s := newState()
	for _, list := range []struct {
		name string
		h    http.HandlerFunc
	}{{"organizations", s.listOrganizations}, {"projects", s.listProjects}} {
		page := getAt(t, list.h, "/?offset=1&limit=500", nil)
		if page["offset"] != 1.0 || page["limit"] != 500.0 || len(listOf(t, page[list.name])) != 1 {
			t.Errorf("%s at offset 1: %v", list.name, page)
		}
		bare := getAt(t, list.h, "/", nil)
		if bare["offset"] != 0.0 || bare["limit"] != float64(offsetDefaultLimit) {
			t.Errorf("%s with no paging: offset %v limit %v, want 0 and %d", list.name, bare["offset"], bare["limit"], offsetDefaultLimit)
		}
	}
}

// Every role answers a description, its update time and the self-service
// permissions every built-in role grants, never an empty list.
func TestARoleAnswersItsDescriptionAndPermissions(t *testing.T) {
	s := newState()
	page := getAt(t, s.listRoles, "/?offset=0&limit=500", map[string]string{"orgId": "org-demo"})
	roles := listOf(t, page["roles"])
	if len(roles) != len(fakeRoles) {
		t.Fatalf("roles = %d, want %d", len(roles), len(fakeRoles))
	}
	for _, row := range roles {
		role := object(t, row)
		if role["description"] == nil || role["description"] == "" || role["updatedAt"] == nil {
			t.Errorf("role %v: description %v, updatedAt %v", role["id"], role["description"], role["updatedAt"])
		}
		if perms := listOf(t, role["permissions"]); len(perms) != len(selfServicePermissions) {
			t.Errorf("role %v permissions = %v, want the self-service set", role["id"], perms)
		}
	}
}

// A project answers its update time: its create's until a PATCH moves it.
func TestAProjectAnswersWhenItLastChanged(t *testing.T) {
	s := newState()
	org := map[string]string{"orgId": "org-demo", "project": "fresh"}
	_, created := in(t, s.createProjectInOrg, http.MethodPost, "", "", org, `{"id":"fresh","region":"us-texas-1"}`)
	if created["updatedAt"] == nil || created["updatedAt"] != created["createdAt"] {
		t.Errorf("create: createdAt %v, updatedAt %v, want the same", created["createdAt"], created["updatedAt"])
	}
	// Made a day earlier, so a PATCH within the same second still moves it.
	day := 24 * time.Hour
	projectKind.rows(s.global())[at{id: "fresh"}].createdAt = time.Now().UTC().Add(-day).Truncate(time.Second)
	_, before := in(t, s.getProjectInOrg, http.MethodGet, "", "", org, "")
	_, patched := in(t, s.updateProjectInOrg, http.MethodPatch, "", "", org, `{"displayName":"Fresh"}`)
	if before["updatedAt"] != before["createdAt"] {
		t.Errorf("before the patch: createdAt %v, updatedAt %v, want the same", before["createdAt"], before["updatedAt"])
	}
	if patched["updatedAt"] == before["updatedAt"] || patched["createdAt"] != before["createdAt"] {
		t.Errorf("patch: createdAt %v, updatedAt %v, want only updatedAt moved", patched["createdAt"], patched["updatedAt"])
	}
}
