package main

import (
	"fmt"
	"net/http"
	"testing"
)

// What dev.4 answered for a resource straight after its DELETE
// (live-shapes-dev4.txt, section E): an instance group reads `"sshAccess":[]`
// and a cluster `"nodePools":[]` with `"nodeCount":0`, because the platform
// releases both as it moves the resource to `deleting`.

func activeGroup(t *testing.T, s *state, displayName string) {
	t.Helper()
	body := `{"id":"x","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 +
		`","nodePools":[{"nodePool":"workers","size":2}],"displayName":"` + displayName +
		`","sshAccess":[{"username":"ops","sshPublicKeys":["k"]}]}`
	if code, out := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil, body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	for range 10 {
		if _, out := in(t, s.getInstanceGroup, http.MethodGet, "r", "p", map[string]string{"id": "x"}, ""); out["state"] == "active" {
			return
		}
	}
	t.Fatal("the group never settled")
}

func TestADeletingGroupReadsItsSSHAccessBlank(t *testing.T) {
	s := newState()
	activeGroup(t, s, "g")
	if code, out := in(t, s.deleteInstanceGroup, http.MethodDelete, "r", "p", map[string]string{"id": "x"}, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", code, out)
	}
	_, got := in(t, s.getInstanceGroup, http.MethodGet, "r", "p", map[string]string{"id": "x"}, "")
	if got["state"] != "deleting" {
		t.Fatalf("state = %v, want deleting", got["state"])
	}
	if access, present := got["sshAccess"]; !present || fmt.Sprint(access) != "[]" {
		t.Errorf("sshAccess = %v (present %v), want []", access, present)
	}
}

// A test that has to plan against a group mid-teardown needs the teardown to
// last longer than a plan's reads, and still to finish so the test's destroy
// does.
func TestAGroupMarkedToDeleteSlowlyStaysDeletingThenGoes(t *testing.T) {
	s := newState()
	activeGroup(t, s, deletesSlowly)
	in(t, s.deleteInstanceGroup, http.MethodDelete, "r", "p", map[string]string{"id": "x"}, "")
	for i := range slowTeardownReads - 1 {
		if code, got := in(t, s.getInstanceGroup, http.MethodGet, "r", "p", map[string]string{"id": "x"}, ""); code != http.StatusOK || got["state"] != "deleting" {
			t.Fatalf("read %d: %d %v, want deleting", i+1, code, got["state"])
		}
	}
	if code, _ := in(t, s.getInstanceGroup, http.MethodGet, "r", "p", map[string]string{"id": "x"}, ""); code != http.StatusNotFound {
		t.Errorf("read %d answered %d, want the teardown finished", slowTeardownReads, code)
	}
}

func TestADeletingClusterReadsItsNodePoolsBlank(t *testing.T) {
	s := newState()
	body := `{"id":"c","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 + `"}`
	if code, out := call(t, s.createCluster, http.MethodPost, "", body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	for range 10 {
		if _, out := read(t, s); out["state"] == "active" {
			break
		}
	}
	if _, out := read(t, s); fmt.Sprint(out["nodeCount"]) == "0" {
		t.Fatalf("the active cluster has no nodes, so this can't tell a teardown apart: %v", out)
	}
	if code, out := call(t, s.deleteCluster, http.MethodDelete, "c", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", code, out)
	}
	_, got := read(t, s)
	if got["state"] != "deleting" || fmt.Sprint(got["nodePools"]) != "[]" || fmt.Sprint(got["nodeCount"]) != "0" {
		t.Errorf("state %v, nodePools %v, nodeCount %v, want deleting, [] and 0", got["state"], got["nodePools"], got["nodeCount"])
	}
}

// A cluster marked to create and delete slowly stays `creating` for slowReads
// reads, then settles, and once deleted stays `deleting` as long, then goes,
// and a second DELETE meanwhile is the 409 the platform answers. A test can
// time out either wait and still watch the next run finish it.
func TestAClusterMarkedSlowOutlastsAShortWaitBothWays(t *testing.T) {
	s := newState()
	body := `{"id":"c","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 +
		`","displayName":"` + createsSlowly + " " + deletesSlowly + `"}`
	if code, out := call(t, s.createCluster, http.MethodPost, "", body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	for i := range slowReads - 1 {
		if _, out := read(t, s); out["state"] != "creating" {
			t.Fatalf("read %d: state %v, want creating", i+1, out["state"])
		}
	}
	if _, out := read(t, s); out["state"] != "active" {
		t.Fatalf("read %d: state %v, want active", slowReads, out["state"])
	}

	if code, out := call(t, s.deleteCluster, http.MethodDelete, "c", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", code, out)
	}
	if code, _ := call(t, s.deleteCluster, http.MethodDelete, "c", ""); code != http.StatusConflict {
		t.Errorf("a second delete answered %d, want 409", code)
	}
	for i := range slowReads - 1 {
		if code, out := read(t, s); code != http.StatusOK || out["state"] != "deleting" {
			t.Fatalf("read %d: %d %v, want deleting", i+1, code, out["state"])
		}
	}
	if code, _ := read(t, s); code != http.StatusNotFound {
		t.Errorf("read %d answered %d, want the teardown finished", slowReads, code)
	}
}

// A teardown that fails settles the group `failed`, and the users the delete
// released stay released. The next DELETE tears it down.
func TestAGroupWhoseTeardownFailsReadsFailedWithNoUsersThenDeletes(t *testing.T) {
	s := newState()
	activeGroup(t, s, teardownFails)
	in(t, s.deleteInstanceGroup, http.MethodDelete, "r", "p", map[string]string{"id": "x"}, "")
	var got map[string]any
	for range 10 {
		if _, got = in(t, s.getInstanceGroup, http.MethodGet, "r", "p", map[string]string{"id": "x"}, ""); got["state"] != "deleting" {
			break
		}
	}
	for range 5 {
		if got["state"] != "failed" || fmt.Sprint(got["sshAccess"]) != "[]" {
			t.Fatalf("state %v, sshAccess %v, want it to stay failed with []", got["state"], got["sshAccess"])
		}
		_, got = in(t, s.getInstanceGroup, http.MethodGet, "r", "p", map[string]string{"id": "x"}, "")
	}
	if code, out := in(t, s.deleteInstanceGroup, http.MethodDelete, "r", "p", map[string]string{"id": "x"}, ""); code != http.StatusNoContent {
		t.Fatalf("second delete: %d %v", code, out)
	}
	for range 10 {
		if code, _ := in(t, s.getInstanceGroup, http.MethodGet, "r", "p", map[string]string{"id": "x"}, ""); code == http.StatusNotFound {
			return
		}
	}
	t.Error("the second teardown never finished")
}

// A cluster marked createFails settles `failed` with RESERVATION_FAILED on the
// cluster and on its finished operation, stays `failed`, and a DELETE then
// tears it down.
func TestAClusterMarkedToFailItsCreateSettlesFailedAndDeletes(t *testing.T) {
	s := newState()
	body := `{"id":"c","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 +
		`","displayName":"` + createFails + `"}`
	if code, out := call(t, s.createCluster, http.MethodPost, "", body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	var out map[string]any
	for range 10 {
		if _, out = read(t, s); out["state"] != "creating" {
			break
		}
	}
	if out["state"] != "failed" || out["stateReason"] != "RESERVATION_FAILED" {
		t.Fatalf("state %v, stateReason %v, want failed and RESERVATION_FAILED", out["state"], out["stateReason"])
	}
	op := object(t, out["operation"])
	if op["status"] != "failed" || op["reason"] != "RESERVATION_FAILED" || op["completedAt"] == nil {
		t.Errorf("operation = %v, want finished failed with RESERVATION_FAILED", op)
	}
	if _, again := read(t, s); again["state"] != "failed" {
		t.Errorf("a later read answered %v, want it to stay failed", again["state"])
	}
	if code, out := call(t, s.deleteCluster, http.MethodDelete, "c", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", code, out)
	}
	for range 10 {
		if code, _ := read(t, s); code == http.StatusNotFound {
			return
		}
	}
	t.Error("the failed cluster was never torn down")
}

// An instance group marked createFails settles `failed`, stays there, and a
// DELETE then tears it down.
func TestAGroupMarkedToFailItsCreateSettlesFailedAndDeletes(t *testing.T) {
	s := newState()
	body := `{"id":"x","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 +
		`","nodePools":[{"nodePool":"workers","size":1}],"displayName":"` + createFails + `"}`
	if code, out := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil, body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	get := func() (int, map[string]any) {
		return in(t, s.getInstanceGroup, http.MethodGet, "r", "p", map[string]string{"id": "x"}, "")
	}
	for range 10 {
		get()
	}
	if _, out := get(); out["state"] != "failed" {
		t.Fatalf("state = %v, want failed", out["state"])
	}
	if code, out := in(t, s.deleteInstanceGroup, http.MethodDelete, "r", "p", map[string]string{"id": "x"}, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", code, out)
	}
	for range 10 {
		if code, _ := get(); code == http.StatusNotFound {
			return
		}
	}
	t.Error("the failed group was never torn down")
}
