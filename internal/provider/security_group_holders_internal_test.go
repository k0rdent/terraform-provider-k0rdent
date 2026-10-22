package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
)

// A group a failed cluster still holds can't be detached, so the warning says
// the platform releases it, and how to stop managing the group meanwhile with
// a removed block. A live holder gets only the list.
func TestAGroupHeldByAFailedClusterSaysWhatNext(t *testing.T) {
	for name, tt := range map[string]struct {
		state string
		next  bool
	}{
		"failed": {"failed", true},
		"active": {"active", false},
	} {
		t.Run(name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /v1/regions/test/projects/test/networking/security-groups/sg/attachments",
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if _, err := io.WriteString(w, `{"attachments":[{"kind":"cluster","id":"c1","state":"`+tt.state+`"}]}`); err != nil {
						t.Errorf("write: %v", err)
					}
				})
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)

			r := &securityGroupResource{meta: testClusterMeta(t, server.URL, time.Minute)}
			var diags diag.Diagnostics
			r.nameTheHolders(context.Background(), clusterTarget{region: "test", project: "test"}, "sg", &diags)
			if diags.WarningsCount() != 1 {
				t.Fatalf("warnings = %v", diags)
			}
			detail := diags.Warnings()[0].Detail()
			if !strings.Contains(detail, "cluster c1 ("+tt.state+")") {
				t.Errorf("the holder isn't named:\n%s", detail)
			}
			if got := strings.Contains(detail, "removed {"); got != tt.next {
				t.Errorf("removed block advised = %v, want %v:\n%s", got, tt.next, detail)
			}
		})
	}
}
