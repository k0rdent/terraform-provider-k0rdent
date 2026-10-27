package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// setVPCSecurityGroups: "A second binding change while this VPC is still
// `updating` is refused with `409` rather than queued", with CONFLICT_IN_USE
// under `details`. Once it settles a change is accepted again.
func TestASecondBindingWhileUpdatingIs409InUse(t *testing.T) {
	s := newState()
	clusterInP(t, s)
	vpc := map[string]string{"id": "vpc-c"}
	settleIn(t, s.getVPC, "p", vpc)

	if code, out := in(t, s.setVPCSecurityGroups, http.MethodPost, "r", "p", vpc, `{"securityGroups":[]}`); code != http.StatusAccepted {
		t.Fatalf("first binding: %d %v", code, out)
	}
	code, out := in(t, s.setVPCSecurityGroups, http.MethodPost, "r", "p", vpc, `{"securityGroups":[]}`)
	if code != http.StatusConflict {
		t.Fatalf("second binding while updating: %d %v, want 409", code, out)
	}
	if !strings.Contains(fmt.Sprint(out["error"]), "reason:CONFLICT_IN_USE") {
		t.Errorf("the 409 carries no CONFLICT_IN_USE detail: %v", out)
	}

	settleIn(t, s.getVPC, "p", vpc)
	if code, out := in(t, s.setVPCSecurityGroups, http.MethodPost, "r", "p", vpc, `{"securityGroups":[]}`); code != http.StatusAccepted {
		t.Errorf("binding once settled: %d %v", code, out)
	}
}

// "Another VPC's default group is refused with `422`." A VPC's own default
// group is accepted.
func TestABindingTakesOnlyItsOwnVPCsDefaultGroup(t *testing.T) {
	s := newState()
	clusterInP(t, s)
	if code, out := in(t, s.createCluster, http.MethodPost, "r", "p", nil,
		`{"id":"d","clusterType":"`+inferenceRef+`/versions/`+inferenceV2+`"}`); code != http.StatusCreated {
		t.Fatalf("second cluster: %d %v", code, out)
	}
	vpc := map[string]string{"id": "vpc-c"}
	settleIn(t, s.getVPC, "p", vpc)

	code, out := in(t, s.setVPCSecurityGroups, http.MethodPost, "r", "p", vpc, `{"securityGroups":["vpc-d-default"]}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(message(out), "another VPC's default group") {
		t.Fatalf("another VPC's default: %d %q, want 422 naming it", code, message(out))
	}
	if code, out := in(t, s.setVPCSecurityGroups, http.MethodPost, "r", "p", vpc, `{"securityGroups":["vpc-c-default"]}`); code != http.StatusAccepted {
		t.Errorf("its own default: %d %v, want 202", code, out)
	}
}
