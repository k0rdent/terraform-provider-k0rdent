package main

import (
	"net/http"
	"strconv"
	"testing"
)

// Shapes recorded from the real API on the v1.3.0-rc.5 kind stack by
// internal/tools/fakedrift, for the fields the provider reads. Each test is
// one row of the drift triage that was fixed in the fake rather than accepted.

// The real Cluster answers `health` with every optional field present, null
// when unset, and withholds the verdict with a reason while the cluster is
// created or deleted.
func TestAClustersHealthCarriesTheRealShape(t *testing.T) {
	s := newState()
	body := `{"id":"c","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 + `"}`
	code, created := call(t, s.createCluster, http.MethodPost, "", body)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, created)
	}
	health := object(t, created["health"])
	for _, key := range []string{"reason", "observedAt", "expiresAt"} {
		if _, ok := health[key]; !ok {
			t.Errorf("health on create has no %q key: %v", key, health)
		}
	}
	if health["state"] != "unknown" || health["reason"] != "health assessment is withheld while the cluster is being created" {
		t.Errorf("health on create = %v, want unknown with the creating reason", health)
	}

	for i := 0; i < settleAfter+1; i++ {
		_, _ = read(t, s)
	}
	_, active := read(t, s)
	if active["state"] != "active" {
		t.Fatalf("state = %v, want active", active["state"])
	}
	health = object(t, active["health"])
	if r, ok := health["reason"]; !ok || r != nil {
		t.Errorf("health once active = %v, want reason null", health)
	}

	if code, out := call(t, s.deleteCluster, http.MethodDelete, "c", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", code, out)
	}
	_, deleting := read(t, s)
	if got := object(t, deleting["health"])["reason"]; got != "health assessment is withheld while the cluster is being deleted" {
		t.Errorf("health reason while deleting = %v", got)
	}
}

// Every Cluster answer names its cluster type, the create's 201 included, and
// an operation in flight carries no message of its own: the operation's
// message is the cluster's stateMessage.
func TestACreatedClusterNamesItsTypeAndItsOperationHasNoMessage(t *testing.T) {
	s := newState()
	body := `{"id":"c","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 + `"}`
	_, created := call(t, s.createCluster, http.MethodPost, "", body)
	if created["clusterTypeName"] != "GPU inference" {
		t.Errorf("clusterTypeName on create = %v, want GPU inference", created["clusterTypeName"])
	}
	if _, ok := object(t, created["operation"])["message"]; ok {
		t.Errorf("operation on create = %v, want no message", created["operation"])
	}
	for i := 0; i < settleAfter+1; i++ {
		_, _ = read(t, s)
	}
	_, active := read(t, s)
	if op := object(t, active["operation"]); op["status"] != "succeeded" || op["message"] != nil {
		t.Errorf("operation once active = %v, want succeeded with no message", op)
	}
}

// A failed operation still carries the cluster's message.
func TestAFailedOperationCarriesTheClustersMessage(t *testing.T) {
	s := newState()
	body := `{"id":"c","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 +
		`","displayName":"` + createFails + `"}`
	if code, out := call(t, s.createCluster, http.MethodPost, "", body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	for i := 0; i < settleAfter+1; i++ {
		_, _ = read(t, s)
	}
	_, failed := read(t, s)
	op := object(t, failed["operation"])
	if failed["state"] != "failed" || op["message"] != failed["stateMessage"] || op["message"] == nil {
		t.Errorf("failed cluster: state %v, stateMessage %v, operation %v", failed["state"], failed["stateMessage"], op)
	}
}

func groupSized(size int) string {
	return `{"id":"g","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 +
		`","nodePools":[{"nodePool":"workers","size":` + strconv.Itoa(size) + `}]}`
}

func onlyPool(t *testing.T, group map[string]any) map[string]any {
	t.Helper()
	pools := listOf(t, group["nodePools"])
	if len(pools) != 1 {
		t.Fatalf("nodePools = %v, want one", pools)
	}
	return object(t, pools[0])
}

// An instance group in flight names its execution, and a settled one doesn't.
// Each pool names its machine type.
func TestAnInstanceGroupNamesItsExecutionInFlight(t *testing.T) {
	s := newState()
	g := map[string]string{"id": "g"}
	_, created := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil, groupSized(2))
	if created["state"] != "creating" || created["executionId"] == nil {
		t.Errorf("create: state %v executionId %v, want creating with an execution", created["state"], created["executionId"])
	}
	if mt := onlyPool(t, created)["machineType"]; mt != "/v1/regions/r/compute/machine-types/fake-workers" {
		t.Errorf("machineType on create = %v", mt)
	}
	var active map[string]any
	for i := 0; i < settleAfter+1; i++ {
		_, active = in(t, s.getInstanceGroup, http.MethodGet, "r", "p", g, "")
	}
	if active["state"] != "active" {
		t.Fatalf("state = %v, want active", active["state"])
	}
	if _, ok := active["executionId"]; ok {
		t.Errorf("active group answers executionId %v, want none", active["executionId"])
	}
	if code, out := in(t, s.deleteInstanceGroup, http.MethodDelete, "r", "p", g, ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", code, out)
	}
	_, deleting := in(t, s.getInstanceGroup, http.MethodGet, "r", "p", g, "")
	if deleting["state"] != "deleting" || deleting["executionId"] == nil || deleting["executionId"] == created["executionId"] {
		t.Errorf("deleting: state %v executionId %v, want a new execution", deleting["state"], deleting["executionId"])
	}
}

// A pool created at size 0 is never reserved, so its ready counts stay absent
// and the group never records progress. A pool with hosts has both counts once
// provisioned, and progress only while the create is in flight.
func TestAPoolCreatedAtZeroHasNoCountsAndNoProgress(t *testing.T) {
	for _, tc := range []struct {
		size         int
		counts, prog bool
	}{{0, false, false}, {2, true, true}} {
		s := newState()
		_, created := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil, groupSized(tc.size))
		_, inFlight := in(t, s.getInstanceGroup, http.MethodGet, "r", "p", map[string]string{"id": "g"}, "")
		if inFlight["state"] != "creating" {
			t.Fatalf("size %d: state on the first read = %v, want creating", tc.size, inFlight["state"])
		}
		var active map[string]any
		for i := 0; i < settleAfter+1; i++ {
			_, active = in(t, s.getInstanceGroup, http.MethodGet, "r", "p", map[string]string{"id": "g"}, "")
		}
		if active["state"] != "active" {
			t.Fatalf("size %d: state = %v, want active", tc.size, active["state"])
		}
		pool := onlyPool(t, active)
		_, ready := pool["readyCount"]
		_, notReady := pool["notReadyCount"]
		if ready != tc.counts || notReady != tc.counts {
			t.Errorf("size %d: pool %v, want counts present %v", tc.size, pool, tc.counts)
		}
		for _, read := range []struct {
			name string
			body map[string]any
			want bool
		}{{"create", created, false}, {"read in flight", inFlight, tc.prog}, {"read once active", active, false}} {
			if _, ok := read.body["progressUpdatedAt"]; ok != read.want {
				t.Errorf("size %d %s: progressUpdatedAt present %v, want %v", tc.size, read.name, ok, read.want)
			}
		}
	}
}

// A scale's answer has no progress yet, its reads while updating do, and the
// group settled again has none.
func TestAScaleHasProgressOnlyWhileUpdating(t *testing.T) {
	s := newState()
	g := map[string]string{"id": "g"}
	in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil, groupSized(2))
	for i := 0; i < settleAfter+1; i++ {
		in(t, s.getInstanceGroup, http.MethodGet, "r", "p", g, "")
	}
	code, scaled := in(t, s.scaleInstanceGroup, http.MethodPost, "r", "p", g, `{"nodePools":[{"nodePool":"workers","size":3}]}`)
	if code != http.StatusOK || scaled["state"] != "updating" {
		t.Fatalf("scale: %d %v", code, scaled)
	}
	_, updating := in(t, s.getInstanceGroup, http.MethodGet, "r", "p", g, "")
	var settled map[string]any
	for i := 0; i < settleAfter+1; i++ {
		_, settled = in(t, s.getInstanceGroup, http.MethodGet, "r", "p", g, "")
	}
	if updating["state"] != "updating" || settled["state"] != "active" {
		t.Fatalf("states %v then %v, want updating then active", updating["state"], settled["state"])
	}
	for _, read := range []struct {
		name string
		body map[string]any
		want bool
	}{{"scale", scaled, false}, {"read while updating", updating, true}, {"read once active", settled, false}} {
		if _, ok := read.body["progressUpdatedAt"]; ok != read.want {
			t.Errorf("%s: progressUpdatedAt present %v, want %v", read.name, ok, read.want)
		}
	}
}

// A group whose create failed says why, in the three fields the real API
// answers it with, and a group still creating says nothing.
func TestAFailedGroupCreateSaysWhy(t *testing.T) {
	s := newState()
	g := map[string]string{"id": "g"}
	body := `{"id":"g","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 +
		`","nodePools":[{"nodePool":"workers","size":2}],"displayName":"` + createFails + `"}`
	_, created := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil, body)
	var failed map[string]any
	for i := 0; i < settleAfter+1; i++ {
		_, failed = in(t, s.getInstanceGroup, http.MethodGet, "r", "p", g, "")
	}
	if failed["state"] != "failed" || failed["stateReason"] != "INSUFFICIENT_CAPACITY" ||
		failed["stateMessage"] == nil || failed["statusReason"] != failed["stateMessage"] {
		t.Errorf("failed group: state %v, stateReason %v, stateMessage %v, statusReason %v",
			failed["state"], failed["stateReason"], failed["stateMessage"], failed["statusReason"])
	}
	for _, key := range []string{"stateReason", "stateMessage", "statusReason"} {
		if _, ok := created[key]; ok {
			t.Errorf("creating group answers %s", key)
		}
	}
}

// The real API names an owned VPC after its networkSchema entry.
func TestAnOwnedVPCHasADisplayName(t *testing.T) {
	s := newState()
	if code, out := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil, groupSized(2)); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	_, out := in(t, s.listVPCs, http.MethodGet, "r", "p", nil, "")
	vpcs := listOf(t, out["vpcs"])
	if len(vpcs) != 1 || object(t, vpcs[0])["displayName"] != "fabric" {
		t.Errorf("vpcs = %v, want one named fabric", vpcs)
	}
}

// A project answers its uid on the list, the get and the create, and two
// projects never share one.
func TestAProjectAnswersItsUID(t *testing.T) {
	s := newState()
	org := map[string]string{"orgId": "org-demo", "project": "fresh"}
	_, created := in(t, s.createProjectInOrg, http.MethodPost, "", "", org, `{"id":"fresh","region":"us-texas-1"}`)
	_, got := in(t, s.getProjectInOrg, http.MethodGet, "", "", org, "")
	if created["uid"] == nil || created["uid"] == "" || got["uid"] != created["uid"] {
		t.Errorf("uid: create %v, get %v, want one uid", created["uid"], got["uid"])
	}
	_, list := in(t, s.listProjects, http.MethodGet, "", "", nil, "")
	seen := map[any]string{}
	for _, row := range listOf(t, list["projects"]) {
		p := object(t, row)
		if p["uid"] == nil || p["uid"] == "" {
			t.Errorf("listed project %v has no uid", p["id"])
		}
		if other, dup := seen[p["uid"]]; dup {
			t.Errorf("projects %v and %v share uid %v", other, p["id"], p["uid"])
		}
		id, ok := p["id"].(string)
		if !ok {
			t.Fatalf("listed project id %v isn't a string", p["id"])
		}
		seen[p["uid"]] = id
	}
}
