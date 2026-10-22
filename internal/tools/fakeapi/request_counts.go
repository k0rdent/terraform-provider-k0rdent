package main

import (
	"net/http"
	"sync"
)

// requestCounts counts every request the fake serves, by method and escaped
// path, for the harness to read.
//
// A test that has to show what the provider sent, such as no cancel or
// exactly two PATCHes, could read the request log, but the log reaches its
// file through a pipe the harness drains on its own, so a read straight after
// an apply can miss the last lines. The count is taken before the request is
// served, so by the time the provider has its answer, the count has it too.
//
// It's served at GET /_harness/requests?method=...&path=..., behind the
// owner token like the rest of /_harness, and the API has no such route.
type requestCounts struct {
	mu sync.Mutex
	n  map[string]int
}

func newRequestCounts() *requestCounts { return &requestCounts{n: map[string]int{}} }

func (c *requestCounts) add(method, path string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n[method+" "+path]++
}

// serve answers how many requests with the method and path in the query the
// fake has served so far.
func (c *requestCounts) serve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	c.mu.Lock()
	n := c.n[q.Get("method")+" "+q.Get("path")]
	c.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"count": n})
}
