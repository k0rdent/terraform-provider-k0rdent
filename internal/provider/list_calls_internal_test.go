package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// A 404 on a VPC's peerings names the VPC where configuration named it. The
// check on a peering's other side reads the same list and gets the
// platform's own failure back, which its warning quotes.
func TestAMissingVPCIsNamedOnlyWhereTheListReportsIt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/regions/us-texas-1/projects/demo/networking/vpcs/edge/peerings" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		writeTestJSON(t, w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404, "message": "not found"}})
	}))
	defer server.Close()
	meta := testClusterMeta(t, server.URL, time.Minute)
	target := clusterTarget{region: "us-texas-1", project: "demo"}

	var diags diag.Diagnostics
	if _, ok := listVPCPeerings(context.Background(), meta, target, "edge", &diags); ok {
		t.Fatal("a missing VPC listed")
	}
	if e := diags.Errors(); len(e) != 1 || e[0].Summary() != noSuchVPC ||
		e[0].Detail() != `No VPC named "edge" in project "demo" of region "us-texas-1".` {
		t.Errorf("got %v, want the missing VPC named", diags)
	}

	_, err := readVPCPeerings(context.Background(), meta, target, "edge")
	var resp pageResponse
	if !errors.As(err, &resp) || strings.Contains(err.Error(), "No VPC named") {
		t.Errorf("got %v, want the platform's own failure", err)
	}
}
