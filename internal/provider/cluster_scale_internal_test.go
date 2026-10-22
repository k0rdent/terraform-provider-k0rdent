package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

func scaleServer(t *testing.T, conflict string, conflicts int32) (*httptest.Server, *atomic.Int32) {
	return scaleServerIn(t, "active", conflict, conflicts)
}

func scaleServerIn(t *testing.T, state, conflict string, conflicts int32) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && posts.Add(1) <= conflicts {
			w.WriteHeader(http.StatusConflict)
			if _, err := w.Write([]byte(conflict)); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		body := map[string]any{"id": "c1", "clusterType": "/v1/regions/test/compute/cluster-types/t", "state": state}
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server, &posts
}

// "Retry shortly; the same request is then admitted." Without the retry, an
// apply that grows one pool and shrinks another fails on its second request.
func TestScaleRetriesTheConflictWithNoReason(t *testing.T) {
	server, posts := scaleServer(t, `{"error":{"code":409,"message":"settling"}}`, 2)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	diags := r.scaleOnce(context.Background(), clusterTarget{region: "test", project: "test"}, "c1",
		k0rdent.ClusterScaleRequest{})
	if diags.HasError() || posts.Load() != 3 {
		t.Errorf("posts=%d diags=%v", posts.Load(), diags.Errors())
	}
}

// A refusal with a reason is the answer, not a window to wait out.
func TestScaleReportsAQuotaRefusalAtOnce(t *testing.T) {
	server, posts := scaleServer(t, `{"error":{"code":409,"message":"over quota","reason":"QUOTA_EXCEEDED"}}`, 100)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	diags := r.scaleOnce(context.Background(), clusterTarget{region: "test", project: "test"}, "c1",
		k0rdent.ClusterScaleRequest{})
	if !diags.HasError() || posts.Load() != 1 {
		t.Errorf("posts=%d diags=%v", posts.Load(), diags.Errors())
	}
}

// The same reason-less 409 on a failed cluster is not a window: retrying it
// held an apply for the whole update timeout.
func TestScaleReportsAConflictOnAFailedClusterAtOnce(t *testing.T) {
	server, posts := scaleServerIn(t, "failed", `{"error":{"code":409,"message":"not scalable"}}`, 100)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	diags := r.scaleOnce(context.Background(), clusterTarget{region: "test", project: "test"}, "c1",
		k0rdent.ClusterScaleRequest{})
	if !diags.HasError() || posts.Load() != 1 {
		t.Errorf("posts=%d diags=%v", posts.Load(), diags.Errors())
	}
}

// A Ctrl-C while the scale is being waited out is reported as the interrupt,
// not as the platform's last refusal.
func TestScaleInterruptedWhileRefusedSaysSo(t *testing.T) {
	server, posts := scaleServer(t, `{"error":{"code":409,"message":"settling"}}`, 1000)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for posts.Load() < 2 {
			time.Sleep(time.Millisecond)
		}
		cancel()
	}()
	text := diagText(r.scaleOnce(ctx, clusterTarget{region: "test", project: "test"}, "c1",
		k0rdent.ClusterScaleRequest{}))
	if !strings.Contains(text, "interrupted") {
		t.Errorf("an interrupted scale reads as a refusal:\n%s", text)
	}
}
