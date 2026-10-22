package main

import (
	"net/http"
	"testing"
)

// A path the fake doesn't route, and compute on the global tier, reach no
// service. Each answers 404 with the gateway's bare `{"message": ...}` and no
// contract envelope, so the provider can't take either as a resource gone.
func TestAnUnroutedPathAnswersTheGatewaysBare404(t *testing.T) {
	s := newState()
	for _, tc := range []struct {
		name   string
		h      http.HandlerFunc
		region string
	}{
		{"unrouted GET", notFound, "r"},
		{"cluster types on the global tier", s.listClusterTypes, "global"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out := in(t, tc.h, http.MethodGet, tc.region, "p", nil, "")
			if code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404", code)
			}
			if msg, ok := out["message"].(string); !ok || msg == "" {
				t.Errorf("body %v has no top-level message", out)
			}
			if _, ok := out["error"]; ok {
				t.Errorf("body %v carries the contract's error envelope", out)
			}
		})
	}
}
