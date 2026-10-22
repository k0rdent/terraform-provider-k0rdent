package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// v1.3.0-dev.4 locks a project to one region. These hold the fake to the
// shapes the kind stack answered: a create without a
// region is a 422 saying so, the region comes back on every read, and a
// project-scoped route in another region is a 409 PROJECT_REGION_MISMATCH.

func reasonOf(out map[string]any) string {
	if e, ok := out["error"].(map[string]any); ok {
		if r, ok := e["reason"].(string); ok {
			return r
		}
	}
	return ""
}

func TestAProjectCreateNeedsARegionThatHostsProjects(t *testing.T) {
	s := newState()
	const org = "org-demo"
	orgPath := map[string]string{"orgId": org}
	for _, tc := range []struct {
		body, want string
	}{
		{`{"id":"pr-none"}`, "validation: region is required"},
		{`{"id":"pr-empty","region":""}`, "validation: region is required"},
		{`{"id":"pr-global","region":"global"}`, "does not host projects"},
		{`{"id":"pr-nowhere","region":"nowhere"}`, "does not host projects"},
		{`{"id":"pr-bad","region":"-x"}`, "pattern"},
	} {
		code, out := in(t, s.createProjectInOrg, http.MethodPost, "", "", orgPath, tc.body)
		if code != http.StatusUnprocessableEntity || !strings.Contains(errorMessage(out), tc.want) {
			t.Errorf("%s: %d %v, want 422 saying %q", tc.body, code, out, tc.want)
		}
	}

	code, out := in(t, s.createProjectInOrg, http.MethodPost, "", "", orgPath, `{"id":"pr-ok","region":"us-texas-1"}`)
	if code != http.StatusCreated || out["region"] != "us-texas-1" {
		t.Fatalf("create: %d %v", code, out)
	}
	path := map[string]string{"orgId": org, "project": "pr-ok"}
	if got, read := in(t, s.getProjectInOrg, http.MethodGet, "", "", path, ""); got != http.StatusOK || read["region"] != "us-texas-1" {
		t.Errorf("read: %d %v", got, read)
	}

	// The region can't change, and naming it on an update says so.
	code, out = in(t, s.updateProjectInOrg, http.MethodPatch, "", "", path, `{"region":"us-texas-1"}`)
	if code != http.StatusConflict || reasonOf(out) != "CONFLICT_IMMUTABLE" {
		t.Errorf("update naming the region: %d %v", code, out)
	}
}

func TestAProjectRouteInAnotherRegionIsARegionMismatch(t *testing.T) {
	s := newState()
	h := s.gateway(s.listNetworks)

	code, out := in(t, h, http.MethodGet, "eu-west-1", "demo", nil, "")
	if code != http.StatusConflict || reasonOf(out) != "PROJECT_REGION_MISMATCH" {
		t.Fatalf("demo in eu-west-1: %d %v", code, out)
	}
	// The message doesn't name the project's region, as the contract says.
	if msg := errorMessage(out); strings.Contains(msg, "us-texas-1") || !strings.Contains(msg, "eu-west-1") {
		t.Errorf("message %q", msg)
	}
	if code, out := in(t, h, http.MethodGet, "us-texas-1", "demo", nil, ""); code != http.StatusOK {
		t.Errorf("demo in its own region: %d %v", code, out)
	}
	// A project made before regions were required isn't checked.
	if code, out := in(t, h, http.MethodGet, "eu-west-1", legacyProject, nil, ""); code != http.StatusOK {
		t.Errorf("the project with no region: %d %v", code, out)
	}
}

func TestTheRegionListSaysWhichRegionsHostProjects(t *testing.T) {
	s := newState()
	_, out := in(t, s.listRegions, http.MethodGet, "", "", nil, "")
	hosts := map[string]any{}
	rows, ok := out["regions"].([]any)
	if !ok {
		t.Fatalf("regions = %v", out["regions"])
	}
	for _, row := range rows {
		r := object(t, row)
		hosts[fmt.Sprint(r["id"])] = r["hostsProjects"]
	}
	if hosts["global"] != false || hosts["us-texas-1"] != true {
		t.Errorf("hostsProjects = %v", hosts)
	}
}
