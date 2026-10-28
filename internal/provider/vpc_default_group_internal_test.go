package provider

import (
	"net/http"
	"testing"
)

// The refusal is told apart by its message, because CONFLICT_IN_USE is also
// what a VPC mid-change answers. The first carries the message and reason
// API v1.3.0-rc.4 answered a destroy with.
func TestOnlyTheNoDefaultRefusalReadsAsOne(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"no default", http.StatusConflict, `{"error":{"code":409,"message":"conflict: vpc \"vpc-a\" ` +
			`has no default security group to fall back to, so its groups cannot be cleared; attach a group explicitly",` +
			`"details":[{"reason":"CONFLICT_IN_USE"}]}}`, true},
		{"mid-change", http.StatusConflict, `{"error":{"code":409,"message":"the VPC already has a binding change in flight",` +
			`"details":[{"reason":"CONFLICT_IN_USE"}]}}`, false},
		{"another status", http.StatusUnprocessableEntity, `{"error":{"code":422,"message":"has no default security group to fall back to"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: tc.status, Status: http.StatusText(tc.status), Header: http.Header{}}
			if got := refusedForNoDefault(resp, []byte(tc.body)); got != tc.want {
				t.Errorf("refusedForNoDefault = %v, want %v", got, tc.want)
			}
		})
	}
}

// The ledger names, per group, only the VPCs a destroy left it on.
func TestTheLedgerKeepsWhereAGroupWasLeft(t *testing.T) {
	var l bindingLedger
	at := clusterTarget{region: "r", project: "p"}
	l.leftOnVPC(at, "vpc-a", []string{"g1", "g2"})
	l.leftOnVPC(at, "vpc-b", []string{"g1"})
	l.leftOnVPC(at, "vpc-a", []string{"g1"})
	if got := l.leftHolding(at, "g1"); len(got) != 2 || got[0] != "vpc-a" || got[1] != "vpc-b" {
		t.Errorf("g1 left on %v, want [vpc-a vpc-b]", got)
	}
	if got := l.leftHolding(at, "g3"); len(got) != 0 {
		t.Errorf("g3 left on %v, want nothing", got)
	}
	if got := l.leftHolding(clusterTarget{region: "r", project: "other"}, "g1"); len(got) != 0 {
		t.Errorf("g1 in another project left on %v, want nothing", got)
	}
}
