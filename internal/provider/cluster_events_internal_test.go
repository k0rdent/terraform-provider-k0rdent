package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// eventLog serves listClusterRuntimeEvents the way the contract describes it:
// oldest first, `limit` lines a page, a cursor after each non-empty page, and
// only the levels asked for. status, when set, answers every read with it.
type eventLog struct {
	mu      sync.Mutex
	lines   []map[string]any
	status  int
	queries []url.Values
}

func (l *eventLog) serve(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		q := r.URL.Query()
		l.queries = append(l.queries, q)
		if l.status != 0 {
			writeTestError(t, w, l.status, "the event store is unavailable")
			return
		}
		limit := 100
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 500 {
				writeTestError(t, w, http.StatusUnprocessableEntity, "limit out of range")
				return
			}
			limit = n
		}
		start := 0
		if v := q.Get("after"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				writeTestError(t, w, http.StatusBadRequest, "bad cursor")
				return
			}
			start = n
		}
		var page []any
		next := start
		for ; next < len(l.lines) && len(page) < limit; next++ {
			level, ok := l.lines[next]["level"].(string)
			if !ok {
				t.Errorf("line %d has no level", next)
				continue
			}
			if levels := q["level"]; len(levels) > 0 && !slices.Contains(levels, level) {
				continue
			}
			page = append(page, l.lines[next])
		}
		body := map[string]any{"events": page}
		if page == nil {
			body["events"] = []any{}
		} else {
			body["cursor"] = strconv.Itoa(next)
		}
		writeJSON(w, body)
	}
}

// failureQueries are the reads that quote a failure, which filter by level.
// The wait's own reads, which follow the log as it polls, don't.
func failureQueries(all []url.Values) []url.Values {
	var out []url.Values
	for _, q := range all {
		if q.Has("level") {
			out = append(out, q)
		}
	}
	return out
}

func eventAt(i int, level, message string) map[string]any {
	return map[string]any{
		"occurredAt": time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Second).Format(time.RFC3339),
		"level":      level, "component": "control-plane", "stage": "updating-cluster",
		"message": message, "operation": "op-1",
	}
}

// A failed upgrade quotes the platform's own account of why: the last
// warnings and errors of the operation's event log, newest last, bounded, and
// escaped so a line can neither drive the terminal nor pass for another line.
func TestAFailedUpgradeQuotesTheRuntimeEvents(t *testing.T) {
	api := &upgradeAPI{bound: 2, state: "active", workers: 2, failInto: 5}
	for i := range 1200 {
		level := []string{"info", "ok", "warn", "error"}[i%4]
		api.events.lines = append(api.events.lines, eventAt(i, level, fmt.Sprint("line ", i)))
	}
	api.events.lines = append(api.events.lines,
		eventAt(1200, "error", "etcd member did not rejoin\x1b[2J"),
		eventAt(1201, "error", "two\nlines"),
		eventAt(1202, "info", "the operation ended"))
	resp := runUpgrade(t, api, 5, 2)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a failed upgrade reported success")
	}
	detail := resp.Diagnostics.Errors()[0].Detail()
	for _, want := range []string{
		"did not reconcile", "does not retry",
		"last 5 of 602",
		"2026-09-28T12:19:55Z error control-plane, updating-cluster: line 1195",
		"2026-09-28T12:19:58Z warn control-plane, updating-cluster: line 1198",
		"2026-09-28T12:20:00Z error control-plane, updating-cluster: etcd member did not rejoin\\x1b[2J",
		"2026-09-28T12:20:01Z error control-plane, updating-cluster: two\\nlines",
	} {
		if !strings.Contains(detail, want) {
			t.Errorf("diagnostic does not say %q:\n%s", want, detail)
		}
	}
	for _, unwanted := range []string{"line 1194", "line 1197", "the operation ended", "\x1b", "two\nlines"} {
		if strings.Contains(detail, unwanted) {
			t.Errorf("diagnostic says %q:\n%s", unwanted, detail)
		}
	}
	// The advice still closes the diagnostic, after the lines it explains.
	if strings.Index(detail, "line 1195") > strings.Index(detail, "does not retry") {
		t.Errorf("the event lines come after the advice:\n%s", detail)
	}
	q := failureQueries(api.events.queries)
	if len(q) < 2 || fmt.Sprint(q[0]["level"]) != "[warn error]" || q[0].Get("limit") != "500" || q[1].Get("after") == "" {
		t.Errorf("queries = %v, want warn and error, 500 a page, and the cursor followed", q)
	}
}

// A log too long to read in the pages allowed says that the lines quoted are
// the last ones read, not the last ones recorded.
func TestALongEventLogSaysItStoppedReading(t *testing.T) {
	api := &upgradeAPI{bound: 2, state: "active", workers: 2, failInto: 5}
	for i := range runtimeEventPages*runtimeEventPageSize + 3 {
		api.events.lines = append(api.events.lines, eventAt(i, "error", fmt.Sprint("line ", i)))
	}
	resp := runUpgrade(t, api, 5, 2)
	detail := resp.Diagnostics.Errors()[0].Detail()
	last := runtimeEventPages*runtimeEventPageSize - 1
	if !strings.Contains(detail, fmt.Sprint("line ", last)) || !strings.Contains(detail, "later ones may follow") {
		t.Errorf("diagnostic:\n%s", detail)
	}
	if got := len(failureQueries(api.events.queries)); got != runtimeEventPages {
		t.Errorf("%d pages read, want %d", got, runtimeEventPages)
	}
}

// A log with no warning or error says so, which is itself an answer.
func TestAQuietEventLogSaysSo(t *testing.T) {
	api := &upgradeAPI{bound: 2, state: "active", workers: 2, failInto: 5}
	api.events.lines = []map[string]any{eventAt(0, "info", "started")}
	resp := runUpgrade(t, api, 5, 2)
	if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, "records no warning or error") {
		t.Errorf("diagnostic:\n%s", detail)
	}
}

// The event log is a better diagnostic, not a condition of one. A read that
// fails leaves the failure reported exactly as it would be without it.
func TestAnEventReadThatFailsLeavesTheDiagnostic(t *testing.T) {
	quiet := &upgradeAPI{bound: 2, state: "active", workers: 2, failInto: 5, events: eventLog{status: http.StatusNotFound}}
	broken := &upgradeAPI{bound: 2, state: "active", workers: 2, failInto: 5, events: eventLog{status: http.StatusInternalServerError}}
	a, b := runUpgrade(t, quiet, 5, 2), runUpgrade(t, broken, 5, 2)
	if !a.Diagnostics.HasError() || !b.Diagnostics.HasError() {
		t.Fatal("a failed upgrade reported success")
	}
	da, db := a.Diagnostics.Errors()[0].Detail(), b.Diagnostics.Errors()[0].Detail()
	if da != db || strings.Contains(da, "event") || !strings.Contains(da, "did not reconcile") {
		t.Errorf("diagnostics:\n%s\n---\n%s", da, db)
	}
}

// Every wait on a cluster gets the lines, so a create, a scale and an update
// that end `failed` are explained the same way an upgrade is.
func TestAWaitThatEndsFailedQuotesTheRuntimeEvents(t *testing.T) {
	var log eventLog
	log.lines = []map[string]any{eventAt(0, "error", "no hardware matched the reservation")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/regions/{r}/projects/{p}/compute/clusters/{id}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"id": "c1", "clusterType": verRef(1), "state": "failed",
			"stateMessage": "provisioning failed", "nodePools": []any{}})
	})
	mux.HandleFunc("GET /v1/regions/{r}/projects/{p}/compute/clusters/{id}/runtime/events", log.serve(t))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	err := r.waitForSettled(context.Background(), clusterTarget{region: "test", project: "test"}, "c1", "the cluster to finish creating")
	if err == nil {
		t.Fatal("a failed cluster settled")
	}
	for _, want := range []string{"provisioning failed", "error control-plane, updating-cluster: no hardware matched the reservation"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not say %q:\n%s", want, err)
		}
	}
}

// A wait follows the event log as it polls, from the cursor on, and a wait
// that times out names the last line it saw. Without it, an hour of
// "Still creating..." was all a hardware create showed.
func TestATimedOutWaitNamesTheLastRuntimeEvent(t *testing.T) {
	events := &eventLog{lines: []map[string]any{
		eventAt(0, "info", "Cluster provisioning started"),
		eventAt(1, "ok", "Machine created"),
	}}
	var polls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/runtime/events") {
			events.serve(t)(w, r)
			return
		}
		events.mu.Lock()
		if polls++; polls == 3 {
			events.lines = append(events.lines, eventAt(2, "ok", "Control plane is initialized"))
		}
		events.mu.Unlock()
		writeJSON(w, map[string]any{"id": "c1", "state": "creating"})
	}))
	t.Cleanup(server.Close)

	r := &clusterResource{meta: testClusterMeta(t, server.URL, 200*time.Millisecond)}
	err := r.waitForSettled(context.Background(), clusterTarget{region: "test", project: "test"}, "c1",
		"the cluster to finish creating")
	if err == nil || !strings.Contains(err.Error(), "waited") {
		t.Fatalf("waitForSettled() = %v, want a timeout", err)
	}
	if want := "last runtime event was:\n  2026-09-28T12:00:02Z ok control-plane, updating-cluster: Control plane is initialized"; !strings.Contains(err.Error(), want) {
		t.Errorf("error does not name the last event:\n%s", err)
	}
	events.mu.Lock()
	defer events.mu.Unlock()
	if len(events.queries) < 2 || events.queries[0].Has("after") || events.queries[len(events.queries)-1].Get("after") != "3" {
		t.Errorf("queries = %v, want the first without a cursor and the last after line 3", events.queries)
	}
}

// A delete is followed and explained the way a create is, and both name the
// execution id, which is what the platform's operators look a run up by.
// Without it, a delete that ended `failed` said only "an internal error occurred".
func TestADeleteThatEndsFailedNamesItsExecution(t *testing.T) {
	var log eventLog
	log.lines = []map[string]any{eventAt(0, "error", "volume detach timed out")}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/regions/{r}/projects/{p}/compute/clusters/{id}", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"id": "c1", "state": "failed", "stateReason": "INTERNAL",
			"stateMessage": "the operation failed: an internal error occurred",
			"operation":    map[string]any{"executionId": "exec-delete-1", "status": "failed"}})
	})
	mux.HandleFunc("GET /v1/regions/{r}/projects/{p}/compute/clusters/{id}/runtime/events", log.serve(t))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	err := r.waitForDeleted(context.Background(), clusterTarget{region: "test", project: "test"}, "c1")
	if err == nil {
		t.Fatal("a failed delete finished")
	}
	for _, want := range []string{"an internal error occurred", "volume detach timed out", "execution id for this operation is exec-delete-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not say %q:\n%s", want, err)
		}
	}
}

// A delete that times out names the last line it saw and the execution id.
func TestATimedOutDeleteNamesTheLastEventAndItsExecution(t *testing.T) {
	events := &eventLog{lines: []map[string]any{eventAt(0, "info", "Cluster is being deleted")}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/runtime/events") {
			events.serve(t)(w, r)
			return
		}
		writeJSON(w, map[string]any{"id": "c1", "state": "deleting",
			"operation": map[string]any{"executionId": "exec-delete-2", "status": "running"}})
	}))
	t.Cleanup(server.Close)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, 200*time.Millisecond)}
	err := r.waitForDeleted(context.Background(), clusterTarget{region: "test", project: "test"}, "c1")
	if err == nil || !strings.Contains(err.Error(), "waited") {
		t.Fatalf("waitForDeleted() = %v, want a timeout", err)
	}
	for _, want := range []string{"last runtime event was:\n  2026-09-28T12:00:00Z info control-plane, updating-cluster: Cluster is being deleted",
		"execution id for this operation is exec-delete-2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not say %q:\n%s", want, err)
		}
	}
}
