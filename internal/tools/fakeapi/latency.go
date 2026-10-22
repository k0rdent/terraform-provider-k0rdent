package main

import (
	"net/http"
	"strings"
	"time"
)

// delayed holds every API request for latency before it reaches the routes.
//
// The fake answers in microseconds, and a real stack doesn't. On a laptop the
// dev.4 kind stack answered a GET in about 3ms at the median (docs/performance.md
// has the measurement), and a real deployment across a network takes tens of
// milliseconds. Without the delay a benchmark against the fake would time the
// provider's and Terraform's own work only, and a provider that made ten
// times the requests it needs would look as fast as one that didn't.
//
// The wait happens before the handler takes the state lock, so concurrent
// requests wait concurrently, as they would against a real server. The
// harness's own routes aren't delayed: they aren't the API. A caller that
// leaves stops the wait, and its request is never served.
func delayed(latency time.Duration, next http.Handler) http.Handler {
	if latency <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			t := time.NewTimer(latency)
			select {
			case <-t.C:
			case <-r.Context().Done():
				t.Stop()
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
