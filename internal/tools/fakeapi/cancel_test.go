package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func createStuck(t *testing.T, s *state) {
	t.Helper()
	body := `{"id":"c","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 +
		`","displayName":"` + neverSettles + `"}`
	if code, out := call(t, s.createCluster, http.MethodPost, "", body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
}

func read(t *testing.T, s *state) (int, map[string]any) {
	t.Helper()
	return call(t, s.getCluster, http.MethodGet, "c", "")
}

func errorMessage(out map[string]any) string {
	if e, ok := out["error"].(map[string]any); ok {
		return fmt.Sprint(e["message"])
	}
	return ""
}

func TestAClusterThatNeverSettlesStaysCreating(t *testing.T) {
	s := newState()
	createStuck(t, s)
	for range 10 {
		if _, out := read(t, s); out["state"] != "creating" {
			t.Fatalf("state = %v", out["state"])
		}
	}
}

func TestDeleteIsRefusedMidOperationAndNamesTheCancel(t *testing.T) {
	s := newState()
	createStuck(t, s)
	code, out := call(t, s.deleteCluster, http.MethodDelete, "c", "")
	if code != http.StatusConflict {
		t.Fatalf("delete while creating: %d %v", code, out)
	}
	if msg := errorMessage(out); !strings.Contains(msg, "creating") || !strings.Contains(msg, "/cancel") {
		t.Errorf("the refusal does not name the state and the remedy: %q", msg)
	}
}

func TestACancelWindsDownThenSettlesFailedAndThenItDeletes(t *testing.T) {
	s := newState()
	createStuck(t, s)

	code, out := cancelNow(t, s)
	if code != http.StatusAccepted || out["state"] != "creating" {
		t.Fatalf("cancel: %d %v", code, out)
	}
	if op, ok := out["operation"].(map[string]any); !ok || op["status"] != "cancelling" || op["executionId"] == "" {
		t.Errorf("operation = %v", out["operation"])
	}
	if code, out := call(t, s.deleteCluster, http.MethodDelete, "c", ""); code != http.StatusConflict {
		t.Fatalf("delete while cancelling: %d %v", code, out)
	}

	settled := false
	for range settleAfter + 1 {
		if _, got := read(t, s); got["state"] == "failed" {
			settled = true
			if got["stateReason"] != "CANCELLED" || object(t, got["operation"])["status"] != "cancelled" {
				t.Fatalf("settled = %v", got)
			}
			break
		}
	}
	if !settled {
		t.Fatal("the cancel never settled")
	}
	for range 5 {
		if _, got := read(t, s); got["state"] != "failed" {
			t.Fatalf("a cancelled cluster moved on to %v", got["state"])
		}
	}

	if code, out := call(t, s.deleteCluster, http.MethodDelete, "c", ""); code != http.StatusNoContent {
		t.Fatalf("delete after cancel: %d %v", code, out)
	}
	for range 10 {
		if code, _ := read(t, s); code == http.StatusNotFound {
			return
		}
	}
	t.Fatal("the cancelled cluster never went away")
}

func TestCancelIsRefusedWithNothingInFlight(t *testing.T) {
	s := newState()
	createAt(t, s, inferenceV2, 2)

	if code, out := cancelNow(t, s); code != http.StatusConflict ||
		!strings.Contains(errorMessage(out), "active") {
		t.Errorf("cancel while active: %d %v", code, out)
	}

	if code, out := call(t, s.deleteCluster, http.MethodDelete, "c", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", code, out)
	}
	if code, out := cancelNow(t, s); code != http.StatusConflict ||
		!strings.Contains(errorMessage(out), "deleting") {
		t.Errorf("cancel while deleting: %d %v", code, out)
	}
	if code, out := call(t, s.deleteCluster, http.MethodDelete, "c", ""); code != http.StatusConflict {
		t.Errorf("a second delete while deleting: %d %v", code, out)
	}

	if code, _ := call(t, s.cancelCluster, http.MethodPost, "nope", `{"executionId":"x"}`); code != http.StatusNotFound {
		t.Errorf("cancel of an unknown cluster: %d", code)
	}
}

// The cancelled settle is a failure. The same cancel sent again is answered
// with the cluster as it stands, winding down or settled, as the contract
// makes it idempotent, and a cancel naming any other operation has nothing
// to stop.
func TestASecondCancelIsIdempotentAndAnyOtherIsRefused(t *testing.T) {
	s := newState()
	createStuck(t, s)
	code, first := cancelNow(t, s)
	if code != http.StatusAccepted {
		t.Fatalf("cancel: %d %v", code, first)
	}
	same := fmt.Sprintf(`{"executionId":%q}`, object(t, first["operation"])["executionId"])
	if code, out := call(t, s.cancelCluster, http.MethodPost, "c", same); code != http.StatusAccepted || out["state"] != "creating" {
		t.Errorf("the same cancel while it winds down: %d %v", code, out)
	}
	for range settleAfter {
		read(t, s)
	}
	if code, out := call(t, s.cancelCluster, http.MethodPost, "c", same); code != http.StatusOK || out["state"] != "failed" {
		t.Errorf("the same cancel once settled: %d %v", code, out)
	}
	if code, out := call(t, s.cancelCluster, http.MethodPost, "c", `{"executionId":"c-op-9"}`); code != http.StatusConflict ||
		!strings.Contains(errorMessage(out), "failed") {
		t.Errorf("a cancel naming another operation: %d %v", code, out)
	}
}

// A cancel names the operation it stops. One without a name is a 400, and one
// naming an operation that isn't the one in flight is a 409 with
// CONFLICT_VERSION, and the operation keeps running.
func TestACancelMustNameTheOperationInFlight(t *testing.T) {
	s := newState()
	createStuck(t, s)
	for _, body := range []string{"", "{}", `{"executionId":""}`, `{"executionId":"c-op-1","force":true}`} {
		if code, out := call(t, s.cancelCluster, http.MethodPost, "c", body); code != http.StatusBadRequest {
			t.Errorf("cancel with %q: %d %v", body, code, out)
		}
	}
	code, out := call(t, s.cancelCluster, http.MethodPost, "c", `{"executionId":"c-op-0"}`)
	if code != http.StatusConflict || detailReason(out) != "CONFLICT_VERSION" {
		t.Errorf("a stale cancel: %d %v", code, out)
	}
	if _, now := read(t, s); now["state"] != "creating" {
		t.Errorf("a refused cancel moved the cluster to %v", now["state"])
	}
}

// cancelNow cancels the operation the cluster reads as running, as a client
// that read it just before would.
func cancelNow(t *testing.T, s *state) (int, map[string]any) {
	t.Helper()
	_, now := read(t, s)
	id := ""
	if op, ok := now["operation"].(map[string]any); ok {
		id = fmt.Sprint(op["executionId"])
	}
	return call(t, s.cancelCluster, http.MethodPost, "c", fmt.Sprintf(`{"executionId":%q}`, id))
}

// A cancelled upgrade leaves the cluster bound where it was.
func TestCancelledUpgradeKeepsTheBinding(t *testing.T) {
	s := newState()
	createAt(t, s, inferenceV2, 2)
	if code, out := upgrade(t, s, inferenceV4); code != http.StatusOK {
		t.Fatalf("upgrade: %d %v", code, out)
	}
	if code, out := cancelNow(t, s); code != http.StatusAccepted {
		t.Fatalf("cancel: %d %v", code, out)
	}
	out := settleNow(t, s)
	if out["state"] != "failed" {
		t.Fatalf("settled after the cancel: %v", out)
	}
	if !strings.HasSuffix(fmt.Sprint(out["clusterType"]), inferenceV2) || out["targetClusterTypeVersionId"] != nil {
		t.Errorf("binding after a cancelled upgrade: %v target %v", out["clusterType"], out["targetClusterTypeVersionId"])
	}
}

func object(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("not an object: %v", v)
	}
	return m
}

// readStages is a get-by-id with includeStages=true, the only read that carries
// an operation's progress and stages.
func readStages(t *testing.T, s *state) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/?includeStages=true", nil)
	req.SetPathValue("region", "r")
	req.SetPathValue("project", "p")
	req.SetPathValue("id", "c")
	rec := httptest.NewRecorder()
	s.getCluster(rec, req)
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("read with stages: %d %s", rec.Code, rec.Body.String())
	}
	return out
}

// The operation is what a reader looks at to see how an act ended, so after a
// cancel every read has to say cancelled, with the reason and message the
// cluster itself carries, and the stages must not show anything still running.
func TestAReadAfterACancelReportsTheCancel(t *testing.T) {
	s := newState()
	createStuck(t, s)
	if code, out := cancelNow(t, s); code != http.StatusAccepted {
		t.Fatalf("cancel: %d %v", code, out)
	}
	settleNow(t, s)
	_, plain := read(t, s)
	for name, out := range map[string]map[string]any{"plain": plain, "includeStages": readStages(t, s)} {
		op := object(t, out["operation"])
		if op["status"] != "cancelled" || op["reason"] != out["stateReason"] || op["reason"] != "CANCELLED" ||
			op["message"] != out["stateMessage"] || op["completedAt"] == nil {
			t.Errorf("%s read: operation = %v, state reason %v message %v", name, op, out["stateReason"], out["stateMessage"])
		}
	}
	op := object(t, readStages(t, s)["operation"])
	counts := object(t, object(t, op["progress"])["counts"])
	if counts["inProgress"] != float64(0) {
		t.Errorf("a cancelled operation still counts a stage in progress: %v", counts)
	}
	stages, ok := op["stages"].([]any)
	if !ok {
		t.Fatalf("stages = %v", op["stages"])
	}
	for _, st := range stages {
		if status := object(t, st)["status"]; status != "pending" && status != "done" {
			t.Errorf("stage after a cancel: %v", st)
		}
	}
}

// A failed operation reads the same way: its reason and message are the
// cluster's own.
func TestAFailedUpgradeOperationCarriesTheClusterMessage(t *testing.T) {
	s := newState()
	createAt(t, s, inferenceV4, 2)
	if code, out := upgrade(t, s, inferenceV5); code != http.StatusOK {
		t.Fatalf("upgrade: %d %v", code, out)
	}
	out := settleNow(t, s)
	op := object(t, out["operation"])
	if out["state"] != "failed" || op["status"] != "failed" || op["message"] != out["stateMessage"] || op["message"] == "" {
		t.Errorf("failed upgrade: state %v message %v, operation %v", out["state"], out["stateMessage"], op)
	}
}

// The operation is the current or last one, so the delete that follows a
// cancel replaces it.
func TestTheOperationAfterACancelReplacesIt(t *testing.T) {
	s := newState()
	createStuck(t, s)
	cancelNow(t, s)
	cancelled := settleNow(t, s)
	if code, out := call(t, s.deleteCluster, http.MethodDelete, "c", ""); code != http.StatusNoContent {
		t.Fatalf("delete: %d %v", code, out)
	}
	_, out := read(t, s)
	op := object(t, out["operation"])
	was := object(t, cancelled["operation"])
	if out["state"] != "deleting" || op["status"] != "running" || op["reason"] != nil ||
		op["completedAt"] != nil || op["executionId"] == was["executionId"] {
		t.Errorf("operation after the delete: %v (cancelled one was %v)", op, was)
	}
}

// A cancel that loses the race is accepted like any other, and then the
// operation lands anyway: the cluster settles `active` and the operation
// `succeeded`, with no CANCELLED reason. A cluster without the marker still
// settles cancelled, which the test above holds.
func TestACancelThatLosesTheRaceSettlesActiveAndSucceeded(t *testing.T) {
	s := newState()
	body := `{"id":"c","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 +
		`","displayName":"` + neverSettles + ` ` + cancelLoses + `"}`
	if code, out := call(t, s.createCluster, http.MethodPost, "", body); code != http.StatusCreated {
		t.Fatalf("create: %d %v", code, out)
	}
	code, out := cancelNow(t, s)
	if code != http.StatusAccepted || object(t, out["operation"])["status"] != "cancelling" {
		t.Fatalf("cancel: %d %v", code, out)
	}
	for range settleAfter + 1 {
		_, got := read(t, s)
		if got["state"] == "creating" {
			continue
		}
		if got["state"] != "active" || got["stateReason"] != nil ||
			object(t, got["operation"])["status"] != "succeeded" {
			t.Fatalf("settled = %v", got)
		}
		return
	}
	t.Fatal("the cancel never settled")
}

// The platform clears a cancelled operation's targets when the cancel
// settles, not when it's asked for: until then a scale's pool still reads its
// targetNodeCount and an upgrade its target version. Afterwards the pool keeps
// its old count and the cluster its old version.
func TestACancelKeepsTheTargetsUntilItSettles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(t *testing.T, s *state) (int, map[string]any)
	}{
		{"scale", func(t *testing.T, s *state) (int, map[string]any) {
			return call(t, s.scaleCluster, http.MethodPost, "c", `{"nodePools":[{"id":"workers","nodeCount":3}]}`)
		}},
		{"upgrade", func(t *testing.T, s *state) (int, map[string]any) { return upgrade(t, s, inferenceV4) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newState()
			createAt(t, s, inferenceV2, 2)
			call(t, s.updateCluster, http.MethodPatch, "c", `{"displayName":"`+neverSettles+`"}`)
			if code, out := tc.start(t, s); code != http.StatusOK {
				t.Fatalf("%s: %d %v", tc.name, code, out)
			}
			if code, out := cancelNow(t, s); code != http.StatusAccepted {
				t.Fatalf("cancel: %d %v", code, out)
			}
			_, winding := read(t, s)
			workers := func(out map[string]any) map[string]any {
				pools, ok := out["nodePools"].([]any)
				if !ok {
					t.Fatalf("nodePools = %v", out["nodePools"])
				}
				for _, p := range pools {
					if pool := object(t, p); pool["id"] == "workers" {
						return pool
					}
				}
				t.Fatalf("no workers pool: %v", out)
				return nil
			}
			if tc.name == "scale" && workers(winding)["targetNodeCount"] != float64(3) {
				t.Errorf("winding down, the pool reads %v", workers(winding))
			}
			if tc.name == "upgrade" && winding["targetClusterTypeVersionId"] != inferenceV4 {
				t.Errorf("winding down, the target version reads %v", winding["targetClusterTypeVersionId"])
			}

			out := settleNow(t, s)
			if out["state"] != "failed" {
				t.Fatalf("settled: %v", out)
			}
			if pool := workers(out); pool["nodeCount"] != float64(2) || pool["targetNodeCount"] != nil {
				t.Errorf("settled, the pool reads %v", pool)
			}
			if out["targetClusterTypeVersionId"] != nil || !strings.HasSuffix(fmt.Sprint(out["clusterType"]), inferenceV2) {
				t.Errorf("settled bound to %v, target %v", out["clusterType"], out["targetClusterTypeVersionId"])
			}
		})
	}
}
