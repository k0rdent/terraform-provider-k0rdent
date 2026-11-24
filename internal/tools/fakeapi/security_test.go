package main

import (
	"net/http"
	"testing"
)

// oneRule is a create body's `rules` with one ingress rule, the least a group
// is made with. It goes first in a body, so a test that appends to the body's
// end appends to the group, not to the rules.
const oneRule = `"rules":{"ingress":[{"action":"permit","protocol":"tcp","sourcePrefix":"0.0.0.0/0",` +
	`"destinationPrefix":"0.0.0.0/0","destinationPortRange":"22"}],"egress":[]}`

// API 1.3 accepts a group with no rule in either list, whether `rules` is
// omitted or sent with both lists empty. The fabric then leaves it failed.
func TestACreateWithNoRulesIsAcceptedThenFails(t *testing.T) {
	for name, body := range map[string]string{
		"rules omitted":     `{"id":"x"}`,
		"both lists empty":  `{"id":"x","rules":{"ingress":[],"egress":[]}}`,
		"both lists absent": `{"id":"x","rules":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newState()
			code, out := call(t, s.createSecurityGroup, http.MethodPost, "", body)
			if code != http.StatusCreated || out["ruleCount"] != float64(0) || out["state"] != "creating" {
				t.Fatalf("create: %d %v, want 201 with no rules, creating", code, out)
			}
			for range settleAfter + 1 {
				_, out = call(t, s.getSecurityGroup, http.MethodGet, "x", "")
			}
			if out["state"] != "failed" || out["statusReason"] != "the fabric rejected the request" {
				t.Errorf("settled group: %v, want failed through the fabric", out)
			}
		})
	}
}

// API 1.3 accepts a PATCH whose `rules` has both lists empty, applies the
// other fields in the request, and leaves the group failed through the fabric.
func TestAPatchThatEmptiesTheRulesIsAcceptedThenFails(t *testing.T) {
	s := newState()
	group(t, s, "x")
	code, out := call(t, s.updateSecurityGroup, http.MethodPatch, "x",
		`{"displayName":"renamed","rules":{"ingress":[],"egress":[]}}`)
	if code != http.StatusOK || out["ruleCount"] != float64(0) || out["displayName"] != "renamed" ||
		out["state"] != "updating" {
		t.Fatalf("patch: %d %v, want 200, renamed, with no rules, updating", code, out)
	}
	for range settleAfter + 1 {
		_, out = call(t, s.getSecurityGroup, http.MethodGet, "x", "")
	}
	if out["state"] != "failed" || out["statusReason"] != "the fabric rejected the request" {
		t.Errorf("settled group: %v, want failed through the fabric", out)
	}
}

// A PATCH without `rules`, such as a rename, leaves the set alone, so it goes
// through on the legacy fixture that has none.
func TestAPatchWithoutRulesIsAllowedOnAGroupWithNone(t *testing.T) {
	s := newState()
	group(t, s, "old")
	old := s.scopes[scope{"r", "p"}].groupsSec[at{id: "old"}]
	old.ingress, old.egress = nil, nil

	code, out := call(t, s.updateSecurityGroup, http.MethodPatch, "old", `{"displayName":"renamed"}`)
	if code != http.StatusOK || out["displayName"] != "renamed" || out["ruleCount"] != float64(0) {
		t.Fatalf("rename: %d %v, want 200, renamed, with no rules", code, out)
	}
}

// The legacy marker keeps a group active with no rules on a create and on a
// PATCH. Acceptance tests hold one in state this way.
func TestAGroupMadeBeforeRulesWereRequiredMayHaveNone(t *testing.T) {
	s := newState()
	code, out := call(t, s.createSecurityGroup, http.MethodPost, "",
		`{`+oneRule+`,"id":"old","displayName":"`+madeBeforeRulesRequired+`"}`)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v, want 201", code, out)
	}
	code, out = call(t, s.updateSecurityGroup, http.MethodPatch, "old", `{"rules":{"ingress":[],"egress":[]}}`)
	if code != http.StatusOK || out["ruleCount"] != float64(0) {
		t.Fatalf("patch to no rules: %d %v, want 200 with no rules", code, out)
	}
	code, out = call(t, s.createSecurityGroup, http.MethodPost, "",
		`{"id":"older","displayName":"`+madeBeforeRulesRequired+`"}`)
	if code != http.StatusCreated || out["ruleCount"] != float64(0) {
		t.Fatalf("create with no rules: %d %v, want 201 with no rules", code, out)
	}
}
