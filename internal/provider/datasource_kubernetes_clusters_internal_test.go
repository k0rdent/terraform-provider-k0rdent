package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func TestClusterCandidatesDataSourceReturnsEmptyOrOne(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		switch id {
		case "absent":
			writeTestError(t, w, http.StatusNotFound, "not found")
		case "deleting":
			writeTestJSON(t, w, http.StatusOK, map[string]any{"id": id, "state": "deleting"})
		default:
			writeTestJSON(t, w, http.StatusOK, map[string]any{
				"id": id, "uid": "00000000-0000-4000-8000-000000000001",
				"state": id,
				"tags":  map[string]string{createTokenTag(): "upstream-provenance-not-read"},
			})
		}
	}))
	t.Cleanup(server.Close)
	for _, tc := range []struct {
		name, id, filter string
		want             int
	}{
		{name: "absent", id: "absent"},
		{name: "deleting", id: "deleting"},
		{name: "active", id: "active", want: 1},
		{name: "state differs", id: "active", filter: "failed"},
		{name: "state matches", id: "failed", filter: "failed", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			config := map[string]tftypes.Value{"id": str(tc.id)}
			if tc.filter != "" {
				config["state"] = str(tc.filter)
			}
			resp := readTestDataSource(t, &clusterCandidatesDataSource{meta: testClusterMeta(t, server.URL, time.Minute)}, config)
			if resp.Diagnostics.HasError() {
				t.Fatalf("read: %v", resp.Diagnostics.Errors())
			}
			var got clusterCandidatesDataSourceModel
			resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
			if len(got.Clusters) != tc.want {
				t.Fatalf("clusters = %v, want length %d", got.Clusters, tc.want)
			}
			if raw := resp.State.Raw.String(); strings.Contains(raw, "upstream-provenance-not-read") {
				t.Fatalf("the lookup recorded platform tags: %s", raw)
			}
		})
	}
}
