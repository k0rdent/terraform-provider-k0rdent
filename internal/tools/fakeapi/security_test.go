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

// A group with no rule in either list is refused on create, whether `rules`
// is omitted or sent with both lists empty, and nothing is made.
func TestACreateWithNoRulesIsRefused(t *testing.T) {
	for name, body := range map[string]string{
		"rules omitted":     `{"id":"x"}`,
		"both lists empty":  `{"id":"x","rules":{"ingress":[],"egress":[]}}`,
		"both lists absent": `{"id":"x","rules":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newState()
			code, out := call(t, s.createSecurityGroup, http.MethodPost, "", body)
			if code != http.StatusUnprocessableEntity || errorReason(out) != "INVALID_ARGUMENT" ||
				errorMessage(out) != noRulesMessage {
				t.Fatalf("create: %d %v, want 422 INVALID_ARGUMENT %q", code, out, noRulesMessage)
			}
			if code, out := call(t, s.getSecurityGroup, http.MethodGet, "x", ""); code != http.StatusNotFound {
				t.Errorf("read after the refused create: %d %v, want 404", code, out)
			}
		})
	}
}

// A PATCH whose `rules` has both lists empty is refused, and the group keeps
// its rules and its name.
func TestAPatchThatEmptiesTheRulesIsRefused(t *testing.T) {
	s := newState()
	group(t, s, "x")
	code, out := call(t, s.updateSecurityGroup, http.MethodPatch, "x",
		`{"displayName":"renamed","rules":{"ingress":[],"egress":[]}}`)
	if code != http.StatusUnprocessableEntity || errorReason(out) != "INVALID_ARGUMENT" ||
		errorMessage(out) != noRulesMessage {
		t.Fatalf("patch: %d %v, want 422 INVALID_ARGUMENT %q", code, out, noRulesMessage)
	}
	_, got := call(t, s.getSecurityGroup, http.MethodGet, "x", "")
	if got["ruleCount"] != float64(1) || got["displayName"] != nil || got["state"] != "active" {
		t.Errorf("after the refused patch: ruleCount %v, displayName %v, state %v, want 1, none and active",
			got["ruleCount"], got["displayName"], got["state"])
	}
}

// A PATCH without `rules`, such as a rename, isn't checked for rules, so it
// goes through on a group that has none: one made before the API refused
// them.
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

// A group whose display name carries madeBeforeRulesRequired stands for one
// made before the API refused a group with no rules, so it may have none, on a
// create and on a PATCH. Acceptance tests hold one in state this way.
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

func errorReason(out map[string]any) string {
	if e, ok := out["error"].(map[string]any); ok {
		if r, ok := e["reason"].(string); ok {
			return r
		}
	}
	return ""
}
