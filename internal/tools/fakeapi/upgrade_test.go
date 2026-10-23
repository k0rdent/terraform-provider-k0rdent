package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const (
	inferenceV1  = "aaaaaaaa-0000-4000-8000-000000000001"
	inferenceV2  = "aaaaaaaa-0000-4000-8000-000000000002"
	inferenceV3  = "aaaaaaaa-0000-4000-8000-000000000003"
	inferenceV4  = "aaaaaaaa-0000-4000-8000-000000000004"
	inferenceV5  = "aaaaaaaa-0000-4000-8000-000000000005"
	inferenceRef = "/v1/regions/r/compute/cluster-types/gpu-inference"
)

func call(t *testing.T, h http.HandlerFunc, method, id, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	req.SetPathValue("region", "r")
	req.SetPathValue("project", "p")
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	h(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil && rec.Body.Len() > 0 {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return rec.Code, out
}

func createAt(t *testing.T, s *state, version string, workers int) {
	t.Helper()
	createOf(t, s, "gpu-inference", version, workers)
}

func createOf(t *testing.T, s *state, typeID, version string, workers int) {
	t.Helper()
	body := `{"id":"c","clusterType":"/v1/regions/r/compute/cluster-types/` + typeID + `/versions/` + version +
		`","nodePools":[{"id":"workers","nodeCount":` + strconv.Itoa(workers) + `}]}`
	if code, out := call(t, s.createCluster, http.MethodPost, "", body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	settleNow(t, s)
}

// settleNow reads until the cluster leaves its transient state.
func settleNow(t *testing.T, s *state) map[string]any {
	t.Helper()
	for range 10 {
		_, out := call(t, s.getCluster, http.MethodGet, "c", "")
		if out["state"] != "creating" && out["state"] != "updating" {
			return out
		}
	}
	t.Fatal("the cluster never settled")
	return nil
}

func upgrade(t *testing.T, s *state, version string) (int, map[string]any) {
	t.Helper()
	body := ""
	if version != "" {
		body = `{"clusterTypeVersionId":"` + version + `"}`
	}
	return call(t, s.upgradeCluster, http.MethodPost, "c", body)
}

func TestUpgradeStepsToTheNextAvailableVersionSkippingADraft(t *testing.T) {
	s := newState()
	createAt(t, s, inferenceV2, 2)

	code, out := upgrade(t, s, inferenceV4)
	if code != http.StatusOK || out["state"] != "updating" || out["targetClusterTypeVersionId"] != inferenceV4 {
		t.Fatalf("upgrade: %d %v", code, out)
	}
	if !strings.HasSuffix(fmt.Sprint(out["clusterType"]), inferenceV2) {
		t.Errorf("clusterType moved before the upgrade settled: %v", out["clusterType"])
	}
	got := settleNow(t, s)
	if got["state"] != "active" || got["clusterType"] != inferenceRef+"/versions/"+inferenceV4 {
		t.Errorf("settled: %v", got)
	}
	if _, inFlight := got["targetClusterTypeVersionId"]; inFlight {
		t.Errorf("target still reported after settling: %v", got)
	}
}

func TestUpgradeWithNoBodyResolvesTheNextVersion(t *testing.T) {
	s := newState()
	createAt(t, s, inferenceV1, 2)
	if code, out := upgrade(t, s, ""); code != http.StatusOK || out["targetClusterTypeVersionId"] != inferenceV2 {
		t.Fatalf("upgrade: %d %v", code, out)
	}
}

func TestUpgradeRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, from, to string
		workers        int
		want           int
	}{
		{"more than one available version ahead", inferenceV1, inferenceV4, 2, http.StatusUnprocessableEntity},
		{"backwards", inferenceV2, inferenceV1, 2, http.StatusUnprocessableEntity},
		{"to the version it is on", inferenceV2, inferenceV2, 2, http.StatusUnprocessableEntity},
		{"to a draft", inferenceV2, inferenceV3, 2, http.StatusUnprocessableEntity},
		{"to another ClusterType's version", inferenceV1, "22222222-2222-2222-2222-222222222222", 2, http.StatusNotFound},
		{"an existing pool outside the target's bounds", inferenceV1, inferenceV2, 6, http.StatusUnprocessableEntity},
		{"nothing newer", inferenceV5, "", 2, http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newState()
			createAt(t, s, tc.from, tc.workers)
			if code, out := upgrade(t, s, tc.to); code != tc.want {
				t.Errorf("got %d %v, want %d", code, out, tc.want)
			}
			if got := settleNow(t, s); got["state"] != "active" {
				t.Errorf("a refused upgrade moved the cluster: %v", got)
			}
		})
	}
}

func TestAFailedUpgradeKeepsTheVersionAndIsNotRetryable(t *testing.T) {
	s := newState()
	createAt(t, s, inferenceV4, 2)
	if code, out := upgrade(t, s, inferenceV5); code != http.StatusOK {
		t.Fatalf("upgrade: %d %v", code, out)
	}
	got := settleNow(t, s)
	if got["state"] != "failed" || got["clusterType"] != inferenceRef+"/versions/"+inferenceV4 {
		t.Fatalf("settled: %v", got)
	}
	if code, out := upgrade(t, s, inferenceV5); code != http.StatusConflict {
		t.Errorf("retry from failed: %d %v, want 409", code, out)
	}
}

func TestUpgradeIsRefusedWhileAnotherChangeRuns(t *testing.T) {
	s := newState()
	createAt(t, s, inferenceV1, 2)
	if code, out := upgrade(t, s, inferenceV2); code != http.StatusOK {
		t.Fatalf("upgrade: %d %v", code, out)
	}
	if code, out := upgrade(t, s, inferenceV2); code != http.StatusConflict {
		t.Errorf("second upgrade while updating: %d %v, want 409", code, out)
	}
}

func TestScaleIsCheckedAgainstTheBoundVersionsBounds(t *testing.T) {
	s := newState()
	createAt(t, s, inferenceV2, 2)
	code, out := call(t, s.scaleCluster, http.MethodPost, "c", `{"nodePools":[{"id":"workers","nodeCount":6}]}`)
	if code != http.StatusUnprocessableEntity {
		t.Errorf("scale past v2's maximum: %d %v, want 422", code, out)
	}
}

func TestCreateBindsAPinnedVersion(t *testing.T) {
	s := newState()
	createAt(t, s, inferenceV1, 2)
	got := settleNow(t, s)
	if got["clusterType"] != inferenceRef+"/versions/"+inferenceV1 {
		t.Errorf("clusterType = %v", got["clusterType"])
	}
	if got["nextAvailableClusterTypeVersion"] != inferenceRef+"/versions/"+inferenceV2 {
		t.Errorf("nextAvailableClusterTypeVersion = %v", got["nextAvailableClusterTypeVersion"])
	}
}

func boundsVersion(prefix string, n int) string {
	return fmt.Sprintf("%s-0000-4000-8000-%012d", prefix, n)
}

// A minimum moves too. A scale is checked against the bound version and an
// upgrade against the target, so which comes first decides whether either is
// admitted.
func TestAMovedMinimumIsCheckedOnBothSides(t *testing.T) {
	scale := func(s *state, n int) int {
		code, _ := call(t, s.scaleCluster, http.MethodPost, "c", `{"nodePools":[{"id":"workers","nodeCount":`+strconv.Itoa(n)+`}]}`)
		return code
	}

	raise := newState()
	createOf(t, raise, "gpu-minraise", boundsVersion("bbbbbbbb", 1), 2)
	if code, out := upgrade(t, raise, boundsVersion("bbbbbbbb", 2)); code != http.StatusUnprocessableEntity {
		t.Errorf("upgrade with 2 workers into a minimum of 3: %d %v", code, out)
	}
	if code := scale(raise, 3); code != http.StatusOK {
		t.Errorf("scale to 3 at v1: %d", code)
	}

	lower := newState()
	createOf(t, lower, "gpu-minlower", boundsVersion("cccccccc", 1), 2)
	if code := scale(lower, 1); code != http.StatusUnprocessableEntity {
		t.Errorf("scale to 1 under a minimum of 2: %d", code)
	}
}

func TestABMaaSOnlyVersionIsNotAnUpgradeTarget(t *testing.T) {
	s := newState()
	createOf(t, s, "gpu-chain", boundsVersion("dddddddd", 3), 1)
	if code, out := upgrade(t, s, boundsVersion("dddddddd", 4)); code != http.StatusUnprocessableEntity ||
		!strings.Contains(fmt.Sprint(out), "no k8s configuration") {
		t.Errorf("upgrade into a BMaaS-only version: %d %v", code, out)
	}
}

func TestAMarkedClusterHoldsItsUpgradeInFlight(t *testing.T) {
	s := newState()
	body := `{"id":"c","displayName":"` + holdsUpgrade + `","clusterType":"` + "/v1/regions/r/compute/cluster-types/gpu-inference/versions/" + inferenceV1 + `"}`
	if code, out := call(t, s.createCluster, http.MethodPost, "", body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	settleNow(t, s)
	if code, out := upgrade(t, s, inferenceV2); code != http.StatusOK {
		t.Fatalf("upgrade: %d %v", code, out)
	}
	for range 5 {
		_, out := call(t, s.getCluster, http.MethodGet, "c", "")
		if out["state"] != "updating" || out["targetClusterTypeVersionId"] != inferenceV2 {
			t.Fatalf("the upgrade did not stay in flight: %v", out)
		}
	}
}

// A held upgrade refuses a delete like any operation in flight, and a cancel
// releases it.
func TestAHeldUpgradeIsReleasedByACancel(t *testing.T) {
	s := newState()
	body := `{"id":"c","displayName":"` + holdsUpgrade + `","clusterType":"` + "/v1/regions/r/compute/cluster-types/gpu-inference/versions/" + inferenceV1 + `"}`
	if code, out := call(t, s.createCluster, http.MethodPost, "", body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	settleNow(t, s)
	if code, out := upgrade(t, s, inferenceV2); code != http.StatusOK {
		t.Fatalf("upgrade: %d %v", code, out)
	}
	if code, _ := call(t, s.deleteCluster, http.MethodDelete, "c", ""); code != http.StatusConflict {
		t.Fatalf("delete during the upgrade: %d, want 409", code)
	}
	if code, out := cancelNow(t, s); code != http.StatusAccepted {
		t.Fatalf("cancel: %d %v", code, out)
	}
	for range settleAfter {
		call(t, s.getCluster, http.MethodGet, "c", "")
	}
	if code, out := call(t, s.deleteCluster, http.MethodDelete, "c", ""); code != http.StatusNoContent {
		t.Fatalf("delete after the cancel: %d %v", code, out)
	}
	for range 10 {
		if code, _ := call(t, s.getCluster, http.MethodGet, "c", ""); code == http.StatusNotFound {
			return
		}
	}
	t.Error("the delete never finished")
}
