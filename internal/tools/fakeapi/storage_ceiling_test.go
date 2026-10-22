package main

import (
	"net/http"
	"strings"
	"testing"
)

// v1.3.0-dev.4's cluster storage ceiling: set on a create, raised on an
// update, never lowered, and refused with a reason-less 409 while the
// cluster isn't active.

func ceilingOf(out map[string]any) any {
	if st, ok := out["storage"].(map[string]any); ok {
		return st["ceilingGiB"]
	}
	return nil
}

func TestAStorageCeilingIsSetRaisedAndNeverLowered(t *testing.T) {
	s := newState()
	body := `{"id":"c","clusterType":"` + inferenceRef + `/versions/` + inferenceV2 + `","storage":{"ceilingGiB":100}}`
	code, out := call(t, s.createCluster, http.MethodPost, "", body)
	if code != http.StatusCreated || ceilingOf(out) != float64(100) {
		t.Fatalf("create: %d %v", code, out)
	}

	// Still creating: a raise is the reason-less 409.
	code, out = call(t, s.updateCluster, http.MethodPatch, "c", `{"storage":{"ceilingGiB":200}}`)
	if code != http.StatusConflict || reasonOf(out) != "" || detailReason(out) != "" {
		t.Errorf("a raise while creating: %d %v", code, out)
	}

	settleNow(t, s)
	if got, raised := call(t, s.updateCluster, http.MethodPatch, "c", `{"storage":{"ceilingGiB":200}}`); got != http.StatusOK || ceilingOf(raised) != float64(200) {
		t.Errorf("raise: %d %v", got, raised)
	}
	if _, now := read(t, s); ceilingOf(now) != float64(200) {
		t.Errorf("read after the raise: %v", now["storage"])
	}
	code, out = call(t, s.updateCluster, http.MethodPatch, "c", `{"storage":{"ceilingGiB":150}}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(errorMessage(out), "cannot be lowered from 200 to 150") {
		t.Errorf("lower: %d %v", code, out)
	}
	for _, bad := range []string{`{"storage":{"ceilingGiB":0}}`, `{"storage":{"ceilingGiB":1048577}}`,
		`{"storage":{"ceilingGiB":300},"securityGroups":[]}`} {
		if code, out := call(t, s.updateCluster, http.MethodPatch, "c", bad); code != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %v", bad, code, out)
		}
	}
}

// dev.4 makes a cluster's tags and system tags required: {} when none.
func TestAClusterAlwaysAnswersItsTags(t *testing.T) {
	s := newState()
	createAt(t, s, inferenceV2, 2)
	_, out := read(t, s)
	for _, key := range []string{"tags", "systemTags"} {
		if m, ok := out[key].(map[string]any); !ok || len(m) != 0 {
			t.Errorf("%s = %v, want {}", key, out[key])
		}
	}
}
