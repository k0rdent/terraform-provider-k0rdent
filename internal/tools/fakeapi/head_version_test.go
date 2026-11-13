package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// From API 1.4 getClusterType reports a ClusterType at its newest `available`
// version, the one an unversioned create binds. gpu-training's newest row is a
// v4 draft, so a read that reported the newest row would differ here.
func TestClusterTypeReadReportsTheVersionACreateBinds(t *testing.T) {
	s := newState()
	code, head := call(t, s.getClusterType, http.MethodGet, "gpu-training", "")
	if code != http.StatusOK {
		t.Fatalf("getClusterType: %d %v", code, head)
	}
	resolved, _, _, ok := resolveClusterTypeRef("r", clusterTypePath("r", "gpu-training"))
	if !ok {
		t.Fatal("the head reference didn't resolve")
	}
	_, bound, _ := strings.Cut(resolved, "/versions/")
	if bound != "33333333-3333-3333-3333-333333333333" {
		t.Fatalf("an unversioned create bound %s, and the newest available version is v3", bound)
	}
	if head["versionId"] != bound || head["version"] != float64(3) || head["status"] != "available" {
		t.Fatalf("the read reports versionId %v, version %v, status %v, and a create binds v3 %s",
			head["versionId"], head["version"], head["status"], bound)
	}
	if head["selfLink"] != clusterTypePath("r", "gpu-training") {
		t.Fatalf("the read's selfLink is %v, and it should be head-scoped", head["selfLink"])
	}
}

// From API 1.4 getClusterType answers 404 for a ClusterType with no
// `available` version unless the request carries `includeUnavailable=true`,
// and 422 for a value that isn't a boolean.
func TestClusterTypeReadOfATypeWithNoAvailableVersion(t *testing.T) {
	s := newState()
	for _, c := range []struct {
		query string
		code  int
	}{
		{"", http.StatusNotFound},
		{"?includeUnavailable=false", http.StatusNotFound},
		{"?includeDeprecated=true", http.StatusNotFound},
		{"?includeUnavailable=true", http.StatusOK},
		{"?includeUnavailable=yes", http.StatusUnprocessableEntity},
	} {
		req := httptest.NewRequest(http.MethodGet, "/"+c.query, nil)
		req.SetPathValue("region", "r")
		req.SetPathValue("id", "gpu-preview")
		rec := httptest.NewRecorder()
		s.getClusterType(rec, req)
		if rec.Code != c.code {
			t.Errorf("GET gpu-preview%s answered %d, want %d: %s", c.query, rec.Code, c.code, rec.Body)
			continue
		}
		if c.code != http.StatusOK {
			continue
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got["status"] != "draft" || got["version"] != float64(1) {
			t.Errorf("GET gpu-preview%s reported status %v version %v, want its newest version, the v1 draft",
				c.query, got["status"], got["version"])
		}
	}
	// A type with an available version reads the same either way.
	for _, q := range []string{"", "?includeUnavailable=true"} {
		req := httptest.NewRequest(http.MethodGet, "/"+q, nil)
		req.SetPathValue("region", "r")
		req.SetPathValue("id", "gpu-training")
		rec := httptest.NewRecorder()
		s.getClusterType(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"versionId":"33333333-3333-3333-3333-333333333333"`) {
			t.Errorf("GET gpu-training%s answered %d %s, want v3", q, rec.Code, rec.Body)
		}
	}
}

// The list reports each type's status and version from the catalog's own
// fields, and the read derives them from the history. They have to agree, or
// the two data sources would disagree about the same type.
func TestClusterTypeListAndReadReportTheSameVersion(t *testing.T) {
	for _, ct := range catalog {
		v, ok := reportedVersion(ct, true)
		if !ok {
			t.Errorf("%s: the read reports no version", ct.id)
			continue
		}
		if v.version != ct.version || v.status != ct.status {
			t.Errorf("%s: the list reports version %d %s, the read %d %s", ct.id, ct.version, ct.status, v.version, v.status)
		}
	}
}

// A ClusterType with no `available` version has nothing for a create to bind,
// in either form.
func TestATypeWithNoAvailableVersionBindsNothing(t *testing.T) {
	for _, ref := range []string{
		clusterTypePath("r", "gpu-preview"),
		clusterTypePath("r", "gpu-preview") + "/versions/ffffffff-0000-4000-8000-000000000001",
	} {
		if resolved, _, _, ok := resolveClusterTypeRef("r", ref); ok {
			t.Errorf("%s bound %s", ref, resolved)
		}
	}
}
