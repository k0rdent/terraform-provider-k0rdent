package main

import (
	"net/http"
	"testing"
)

// A peering whose id carries the marker answers the documented lost-lock 503
// once, writes nothing, and is created when the same request comes again.
// Any other id is untouched.
func TestAMarkedPeeringLosesItsVPCLockOnce(t *testing.T) {
	s := newState()
	clusterInP(t, s)
	vpc := map[string]string{"vpcId": "vpc-c"}
	const marked = `{"id":"p-` + vpcLockLostMarker + `","remoteVpc":"/v1/regions/r/projects/elsewhere/networking/vpcs/vpc-elsewhere"}`

	code, out := in(t, s.createVPCPeering, http.MethodPost, "r", "p", vpc, marked)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("first create: %d %v, want 503", code, out)
	}
	if _, has := out["retryAfter"]; has {
		t.Errorf("503 body %v names a wait the contract does not", out)
	}
	if _, list := in(t, s.listVPCPeerings, http.MethodGet, "r", "p", vpc, ""); listed(list, "peerings") != 0 {
		t.Errorf("the 503 wrote a peering: %v", list)
	}

	if code, out := in(t, s.createVPCPeering, http.MethodPost, "r", "p", vpc, marked); code != http.StatusCreated {
		t.Fatalf("second create: %d %v, want 201", code, out)
	}
	if code, out := in(t, s.createVPCPeering, http.MethodPost, "r", "p", vpc, marked); code != http.StatusConflict {
		t.Errorf("third create: %d %v, want the duplicate refused", code, out)
	}
	if _, list := in(t, s.listVPCPeerings, http.MethodGet, "r", "p", vpc, ""); listed(list, "peerings") != 1 {
		t.Errorf("want exactly one peering, got %v", list)
	}

	if code, out := in(t, s.createVPCPeering, http.MethodPost, "r", "p", vpc,
		`{"id":"plain","remoteVpc":"/v1/regions/r/projects/elsewhere/networking/vpcs/vpc-elsewhere"}`); code != http.StatusCreated {
		t.Errorf("unmarked create: %d %v, want 201", code, out)
	}
}
