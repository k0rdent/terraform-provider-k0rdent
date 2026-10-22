package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The benchmark runs the fake with the round trip a real stack takes, so a
// figure measured here carries the cost of each request the provider makes.
// Without it, a provider that made ten times the requests it needs would time
// the same as one that didn't.
func TestLatencyDelaysEveryAPIRequest(t *testing.T) {
	const latency = 40 * time.Millisecond
	h := delayed(latency, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, path := range []string{"/v1/regions", "/v1/regions/global/auth/token"} {
		start := time.Now()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if took := time.Since(start); took < latency {
			t.Errorf("%s answered in %s, under the %s asked for", path, took, latency)
		}
		if rec.Code != http.StatusNoContent {
			t.Errorf("%s answered %d, and the handler behind the delay answers 204", path, rec.Code)
		}
	}
}

// The harness's own routes aren't the API, so a run with latency still starts
// and stops as quickly as one without.
func TestLatencyLeavesTheHarnessRoutesAlone(t *testing.T) {
	h := delayed(time.Second, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	start := time.Now()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/_harness/ready", nil))
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("/_harness/ready took %s, and the latency is for API routes only", took)
	}
}

// A request whose caller has gone stops waiting, so a cancelled run doesn't
// hold the fake's goroutines for the rest of the delay.
func TestLatencyStopsWhenTheCallerLeaves(t *testing.T) {
	called := false
	h := delayed(time.Hour, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodGet, "/v1/regions", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel()
	h.ServeHTTP(httptest.NewRecorder(), req.WithContext(ctx))
	if called {
		t.Error("the handler ran for a caller that had already gone")
	}
}
