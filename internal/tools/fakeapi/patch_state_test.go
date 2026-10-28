package main

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Which states a cluster's PATCH is admitted in, as v1.3.0-rc.3 has it: tags,
// displayName and description in any state but `deleting`, and
// securityGroups only from `active` or `failed`, and never once a delete was
// accepted.
func TestAClusterPatchIsAdmittedByState(t *testing.T) {
	for _, tc := range []struct {
		name     string
		to       func(t *testing.T, s *state)
		metadata int
		groups   int
	}{
		{"active", func(t *testing.T, s *state) { createAt(t, s, inferenceV2, 2) }, http.StatusOK, http.StatusAccepted},
		{"creating", func(t *testing.T, s *state) { createStuck(t, s) }, http.StatusOK, http.StatusConflict},
		{"updating", func(t *testing.T, s *state) {
			createAt(t, s, inferenceV2, 2)
			upgrade(t, s, inferenceV4)
		}, http.StatusOK, http.StatusConflict},
		{"failed", cancelledScale, http.StatusOK, http.StatusAccepted},
		{"failed after a teardown", func(t *testing.T, s *state) {
			createAt(t, s, inferenceV2, 2)
			call(t, s.updateCluster, http.MethodPatch, "c", `{"displayName":"`+teardownFails+`"}`)
			call(t, s.deleteCluster, http.MethodDelete, "c", "")
			for range settleAfter + 1 {
				if _, now := read(t, s); now["state"] == "failed" {
					return
				}
			}
		}, http.StatusOK, http.StatusConflict},
		{"deleting", func(t *testing.T, s *state) {
			createAt(t, s, inferenceV2, 2)
			call(t, s.deleteCluster, http.MethodDelete, "c", "")
		}, http.StatusConflict, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newState()
			group(t, s, "a")
			tc.to(t, s)
			if _, now := read(t, s); !strings.HasPrefix(tc.name, fmt.Sprint(now["state"])) {
				t.Fatalf("the cluster is %v, want %s", now["state"], tc.name)
			}
			code, out := call(t, s.updateCluster, http.MethodPatch, "c",
				`{"tags":{"team":"ml"},"displayName":"renamed","description":"kept"}`)
			if code != tc.metadata {
				t.Errorf("tags, displayName and description: %d %v, want %d", code, out, tc.metadata)
			}
			if code == http.StatusOK {
				if object(t, out["tags"])["team"] != "ml" ||
					out["displayName"] != "renamed" || out["description"] != "kept" {
					t.Errorf("the PATCH didn't land: %v", out)
				}
			}
			if code, out := call(t, s.updateCluster, http.MethodPatch, "c", `{"securityGroups":["a"]}`); code != tc.groups {
				t.Errorf("securityGroups: %d %v, want %d", code, out, tc.groups)
			}
		})
	}
}

// rc.3 checks the body, then whether a delete was accepted, then the groups,
// and only then the state. A storage raise needs `active`, and an unchanged
// ceiling is no raise.
func TestAClusterPatchChecksInThePlatformsOrder(t *testing.T) {
	s := newState()
	group(t, s, "a")
	createStuck(t, s)
	for body, want := range map[string]int{
		`{"securityGroups":["a"],"displayName":"x"}`: http.StatusUnprocessableEntity,
		`{"securityGroups":["nope"]}`:                http.StatusUnprocessableEntity,
	} {
		if code, out := call(t, s.updateCluster, http.MethodPatch, "c", body); code != want {
			t.Errorf("%s while creating: %d %v, want %d", body, code, out, want)
		}
	}

	s = newState()
	body := `{"id":"c","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 + `","storage":{"ceilingGiB":100}}`
	if code, out := call(t, s.createCluster, http.MethodPost, "", body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	settleNow(t, s)
	call(t, s.updateCluster, http.MethodPatch, "c", `{"displayName":"`+neverSettles+`"}`)
	call(t, s.scaleCluster, http.MethodPost, "c", `{"nodePools":[{"id":"workers","nodeCount":3}]}`)
	cancelNow(t, s)
	if out := settleNow(t, s); out["state"] != "failed" {
		t.Fatalf("settled after the cancel: %v", out)
	}
	for body, want := range map[string]int{
		`{"storage":{"ceilingGiB":50}}`:                   http.StatusUnprocessableEntity,
		`{"storage":{"ceilingGiB":200}}`:                  http.StatusConflict,
		`{"storage":{"ceilingGiB":100},"tags":{"a":"b"}}`: http.StatusOK,
	} {
		if code, out := call(t, s.updateCluster, http.MethodPatch, "c", body); code != want {
			t.Errorf("%s on a failed cluster: %d %v, want %d", body, code, out, want)
		}
	}
}

// A securityGroups PATCH admitted on a `failed` cluster runs like any other:
// `updating`, with the failure cleared, then `active`.
func TestAGroupsPatchOnAFailedClusterSettlesActive(t *testing.T) {
	s := newState()
	group(t, s, "a")
	cancelledScale(t, s)
	call(t, s.updateCluster, http.MethodPatch, "c", `{"displayName":"settles"}`)
	code, out := call(t, s.updateCluster, http.MethodPatch, "c", `{"securityGroups":["a"]}`)
	if code != http.StatusAccepted || out["state"] != "updating" || out["stateReason"] != nil || out["stateMessage"] != nil {
		t.Fatalf("PATCH: %d %v", code, out)
	}
	if got := settleNow(t, s); got["state"] != "active" {
		t.Errorf("settled %v", got["state"])
	}
}

// cancelledScale leaves cluster c `failed` by cancelling a scale from 2 to 3
// workers.
func cancelledScale(t *testing.T, s *state) {
	t.Helper()
	createAt(t, s, inferenceV2, 2)
	call(t, s.updateCluster, http.MethodPatch, "c", `{"displayName":"`+neverSettles+`"}`)
	if code, out := call(t, s.scaleCluster, http.MethodPost, "c", `{"nodePools":[{"id":"workers","nodeCount":3}]}`); code != http.StatusOK {
		t.Fatalf("scale: %d %v", code, out)
	}
	if code, out := cancelNow(t, s); code != http.StatusAccepted {
		t.Fatalf("cancel: %d %v", code, out)
	}
	if out := settleNow(t, s); out["state"] != "failed" {
		t.Fatalf("settled after the cancel: %v", out)
	}
}

// A network's or an instance group's metadata PATCH lands in any state and
// leaves the state as it was, as v1.3.0-rc.3 does: it starts no workflow, and
// its query neither reads nor writes the state.
func TestANetworkOrGroupPatchKeepsItsState(t *testing.T) {
	for _, tc := range []struct {
		kind, body string
		make       func(t *testing.T, s *state)
		put        func(s *state, to string)
		patch      func(s *state) http.HandlerFunc
		landed     func(t *testing.T, out map[string]any) bool
	}{
		{
			kind: "network",
			body: `{"tags":{"team":"ml"},"displayName":"renamed","description":"kept"}`,
			make: func(t *testing.T, s *state) {
				if code, out := in(t, s.createNetwork, http.MethodPost, "r", "p", nil,
					`{"id":"x","type":"evpn","evpn":{"type":"l2vpn"}}`); code != http.StatusCreated {
					t.Fatalf("create: %d %v", code, out)
				}
			},
			put:   func(s *state, to string) { s.scopes[scope{"r", "p"}].networks[at{id: "x"}].state = to },
			patch: func(s *state) http.HandlerFunc { return s.updateNetwork },
			landed: func(t *testing.T, out map[string]any) bool {
				return object(t, out["tags"])["team"] == "ml" &&
					out["displayName"] == "renamed" && out["description"] == "kept"
			},
		},
		{
			kind:  "instance group",
			body:  `{"displayName":"renamed","description":"kept"}`,
			make:  func(t *testing.T, s *state) { activeGroup(t, s, "g") },
			put:   func(s *state, to string) { s.scopes[scope{"r", "p"}].groups[at{id: "x"}].state = to },
			patch: func(s *state) http.HandlerFunc { return s.updateInstanceGroup },
			landed: func(_ *testing.T, out map[string]any) bool {
				return out["displayName"] == "renamed" && out["description"] == "kept"
			},
		},
	} {
		for _, st := range []string{"active", "creating", "updating", "failed", "deleting"} {
			t.Run(tc.kind+" "+st, func(t *testing.T) {
				s := newState()
				tc.make(t, s)
				tc.put(s, st)
				code, out := call(t, tc.patch(s), http.MethodPatch, "x", tc.body)
				if code != http.StatusOK {
					t.Fatalf("PATCH: %d %v, want 200", code, out)
				}
				if !tc.landed(t, out) {
					t.Errorf("the PATCH didn't land: %v", out)
				}
				if out["state"] != st {
					t.Errorf("the PATCH answered %v, want %s", out["state"], st)
				}
			})
		}
	}
}
