package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// A read by id refused with a 403 falls back to the project list. When that
// list can't be read either, the diagnostic says so, rather than that the
// list doesn't show the project.
func TestAFallbackListThatFailsIsReportedAsAFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/regions/global/organizations/o/projects/p":
			w.WriteHeader(http.StatusForbidden)
			if _, err := w.Write([]byte(`{"error":{"code":403,"message":"forbidden","requestId":"req-get"}}`)); err != nil {
				t.Errorf("write: %v", err)
			}
		case "/v1/regions/global/projects":
			w.WriteHeader(http.StatusServiceUnavailable)
			if _, err := w.Write([]byte(`{"error":{"code":503,"message":"organizations unavailable","requestId":"req-list"}}`)); err != nil {
				t.Errorf("write: %v", err)
			}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	d := &projectDataSource{meta: testClusterMeta(t, server.URL, time.Minute)}
	var diags diag.Diagnostics
	if _, ok := d.readInOrg(context.Background(), "o", "p", &diags); ok || !diags.HasError() {
		t.Fatal("a refused read and a failed list read the project")
	}
	detail := diags.Errors()[0].Detail()
	for _, want := range []string{"403 Forbidden", "req-get", "couldn't be read either", "organizations unavailable"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the diagnostic doesn't say %q:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "doesn't show") {
		t.Errorf("a list that failed is reported as not showing the project:\n%s", detail)
	}
}
