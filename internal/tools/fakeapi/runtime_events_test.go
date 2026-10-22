package main

import (
	"net/http"
	"testing"
)

func events(t *testing.T, s *state, id, query string) (int, map[string]any) {
	t.Helper()
	return call(t, func(w http.ResponseWriter, r *http.Request) {
		r.URL.RawQuery = query
		s.listClusterRuntimeEvents(w, r)
	}, http.MethodGet, id, "")
}

// eventPage reads a page of the log as its lines and its cursor, "" when it
// has none.
func eventPage(t *testing.T, s *state, query string) ([]map[string]any, string) {
	t.Helper()
	code, out := events(t, s, "c", query)
	raw, ok := out["events"].([]any)
	if code != http.StatusOK || !ok {
		t.Fatalf("%s: %d %v", query, code, out)
	}
	lines := make([]map[string]any, 0, len(raw))
	for _, l := range raw {
		line, ok := l.(map[string]any)
		if !ok {
			t.Fatalf("%s: a line that isn't an object: %v", query, l)
		}
		lines = append(lines, line)
	}
	cursor := ""
	if c, ok := out["cursor"]; ok {
		if cursor, ok = c.(string); !ok || cursor == "" {
			t.Fatalf("%s: a cursor that isn't a string: %v", query, c)
		}
	}
	return lines, cursor
}

// A failed upgrade leaves its account in the event log: the warning and the
// error, scoped to the upgrade, behind a cursor a page of one at a time.
func TestAFailedUpgradeLeavesItsEventsInTheLog(t *testing.T) {
	s := newState()
	createAt(t, s, inferenceV4, 2)
	if code, out := upgrade(t, s, inferenceV5); code != http.StatusOK {
		t.Fatalf("upgrade: %d %v", code, out)
	}
	if out := settleNow(t, s); out["state"] != "failed" {
		t.Fatalf("state = %v", out["state"])
	}

	const filter = "level=warn&level=error&limit=1"
	lines, cursor := eventPage(t, s, filter)
	if len(lines) != 1 || cursor == "" || lines[0]["level"] != "warn" || lines[0]["component"] != "control-plane" {
		t.Fatalf("first page: %v %q", lines, cursor)
	}
	lines, cursor = eventPage(t, s, filter+"&after="+cursor)
	if len(lines) != 1 || cursor == "" || lines[0]["message"] != "the upgrade to clusterType version 5 did not reconcile" {
		t.Fatalf("second page: %v %q", lines, cursor)
	}
	if lines, cursor = eventPage(t, s, filter+"&after="+cursor); len(lines) != 0 || cursor != "" {
		t.Errorf("the log ran on: %v %q", lines, cursor)
	}

	// Unfiltered, the operation's own start line comes first, and the
	// create's lines are out of scope.
	if lines, _ = eventPage(t, s, ""); len(lines) != 3 || lines[0]["level"] != "info" {
		t.Errorf("unfiltered: %v", lines)
	}
}

func TestTheEventLogRefusesWhatTheContractRefuses(t *testing.T) {
	s := newState()
	createAt(t, s, inferenceV4, 2)
	for query, want := range map[string]int{
		"limit=0":            http.StatusUnprocessableEntity,
		"limit=501":          http.StatusUnprocessableEntity,
		"level=debug":        http.StatusBadRequest,
		"after=other:0":      http.StatusBadRequest,
		"after=c:99":         http.StatusBadRequest,
		"level=info&limit=1": http.StatusOK,
	} {
		if code, out := events(t, s, "c", query); code != want {
			t.Errorf("%s: %d %v, want %d", query, code, out, want)
		}
	}
	if code, _ := events(t, s, "missing", ""); code != http.StatusNotFound {
		t.Errorf("a missing cluster: %d", code)
	}
}
