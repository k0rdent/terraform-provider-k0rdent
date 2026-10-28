package main

import (
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func group(t *testing.T, s *state, id string) {
	t.Helper()
	if code, out := call(t, s.createSecurityGroup, http.MethodPost, "", `{`+oneRule+`,"id":"`+id+`"}`); code != http.StatusCreated {
		t.Fatalf("create group %s: %d %v", id, code, out)
	}
	// Settled, as a group has to be before it can be deleted.
	for range settleAfter + 1 {
		call(t, s.getSecurityGroup, http.MethodGet, id, "")
	}
}

func createWithGroups(t *testing.T, s *state, typeID, groups string) (int, map[string]any) {
	t.Helper()
	return call(t, s.createCluster, http.MethodPost, "",
		`{"id":"c","clusterType":"/v1/regions/r/compute/cluster-types/`+typeID+`","securityGroups":`+groups+`}`)
}

func message(out map[string]any) string {
	if e, ok := out["error"].(map[string]any); ok {
		if m, ok := e["message"].(string); ok {
			return m
		}
	}
	return ""
}

func TestClusterGroupsAreKeptInOrderAndChangedInPlace(t *testing.T) {
	s := newState()
	group(t, s, "a")
	group(t, s, "b")
	if code, out := createWithGroups(t, s, "gpu-training", `["b","a"]`); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	read := settleNow(t, s)
	if got := read["securityGroups"]; !reflect.DeepEqual(got, []any{"b", "a"}) {
		t.Fatalf("read securityGroups = %v, want [b a]", got)
	}
	uid := read["uid"]

	code, out := call(t, s.updateCluster, http.MethodPatch, "c", `{"securityGroups":["a"]}`)
	if code != http.StatusAccepted || out["state"] != "updating" {
		t.Fatalf("PATCH: %d %v, want 202 and updating", code, out)
	}
	read = settleNow(t, s)
	if read["uid"] != uid || !reflect.DeepEqual(read["securityGroups"], []any{"a"}) {
		t.Errorf("after PATCH: uid %v securityGroups %v, want uid %v and [a]", read["uid"], read["securityGroups"], uid)
	}

	if code, out := call(t, s.updateCluster, http.MethodPatch, "c", `{"securityGroups":[]}`); code != http.StatusAccepted {
		t.Fatalf("PATCH []: %d %v", code, out)
	}
	if got := settleNow(t, s)["securityGroups"]; !reflect.DeepEqual(got, []any{}) {
		t.Errorf("after []: securityGroups = %v, want []", got)
	}
}

func TestClusterWithoutGroupsReadsAnEmptyList(t *testing.T) {
	s := newState()
	if code, out := call(t, s.createCluster, http.MethodPost, "",
		`{"id":"c","clusterType":"/v1/regions/r/compute/cluster-types/gpu-training"}`); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	if got := settleNow(t, s)["securityGroups"]; !reflect.DeepEqual(got, []any{}) {
		t.Errorf("securityGroups = %#v, want an empty list, never a missing field", got)
	}
}

func TestClusterGroupRefusals(t *testing.T) {
	s := newState()
	group(t, s, "a")
	// A VPC's default group, which a cluster may not name.
	call(t, s.createCluster, http.MethodPost, "",
		`{"id":"other","clusterType":"/v1/regions/r/compute/cluster-types/gpu-training"}`)
	otherDefault := defaultGroupOf(t, s, "vpc-other")

	many := make([]string, 17)
	for i := range many {
		many[i] = `"a"`
	}
	for name, tt := range map[string]struct {
		typeID, groups, want string
	}{
		"unknown ids, every one named": {"gpu-training", `["a","x","y"]`, "unknown security group(s): x, y"},
		"a duplicate":                  {"gpu-training", `["a","a"]`, `securityGroups[1]: id "a" is duplicated`},
		"over the limit":               {"gpu-training", "[" + strings.Join(many, ",") + "]", "at most 16"},
		"a default group":              {"gpu-training", `["` + otherDefault + `"]`, "default group"},
		"no interface to go on":        {"cpu-general", `["a"]`, "no node pool has an interface"},
	} {
		t.Run(name, func(t *testing.T) {
			code, out := createWithGroups(t, s, tt.typeID, tt.groups)
			if code != http.StatusUnprocessableEntity || !strings.Contains(message(out), tt.want) {
				t.Errorf("create: %d %q, want 422 saying %q", code, message(out), tt.want)
			}
		})
	}

	// The same type takes a create with no groups, and `[]` on it.
	if code, out := createWithGroups(t, s, "cpu-general", `[]`); code != http.StatusCreated {
		t.Fatalf("create with []: %d %v", code, out)
	}
	settleNow(t, s)
	code, out := call(t, s.updateCluster, http.MethodPatch, "c", `{"securityGroups":["a"]}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(message(out), "no node pool has an interface") {
		t.Errorf("PATCH on a type with no interface: %d %q, want 422", code, message(out))
	}
}

func TestClusterGroupsGoInAPatchOfTheirOwn(t *testing.T) {
	s := newState()
	group(t, s, "a")
	createWithGroups(t, s, "gpu-training", `[]`)
	settleNow(t, s)
	code, out := call(t, s.updateCluster, http.MethodPatch, "c", `{"securityGroups":["a"],"description":"x"}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(message(out), "send it on its own") {
		t.Errorf("combined PATCH: %d %q, want 422", code, message(out))
	}
}

func TestAHeldGroupIsNotDeleted(t *testing.T) {
	s := newState()
	group(t, s, "a")
	createWithGroups(t, s, "gpu-training", `["a"]`)
	settleNow(t, s)

	code, out := call(t, s.deleteSecurityGroup, http.MethodDelete, "a", "")
	if code != http.StatusConflict || !strings.Contains(message(out), "cluster c") {
		t.Fatalf("delete a held group: %d %v, want 409 naming the cluster", code, out)
	}
	if code, out := call(t, s.listSecurityGroupAttachments, http.MethodGet, "a", ""); code != http.StatusOK ||
		!reflect.DeepEqual(out["attachments"], []any{map[string]any{"kind": "cluster", "id": "c", "state": "active"}}) {
		t.Errorf("attachments: %d %v", code, out)
	}

	call(t, s.updateCluster, http.MethodPatch, "c", `{"securityGroups":[]}`)
	settleNow(t, s)
	if code, out := call(t, s.deleteSecurityGroup, http.MethodDelete, "a", ""); code != http.StatusNoContent {
		t.Errorf("delete once detached: %d %v, want 204", code, out)
	}
}
