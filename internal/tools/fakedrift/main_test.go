package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// A shape names every field and its JSON type and never a value, so a token
// or a key in a body never reaches a recording.
func TestAShapeHoldsFieldsAndTypesButNoValues(t *testing.T) {
	body := `{"token":"s3cret-value","count":3,"ok":true,"gone":null,"keys":[{"id":"k1"},{"id":"k2","extra":1}]}`
	got := shapeOf([]byte(body))
	want := []string{
		"$ object", "$.count number", "$.gone null", "$.keys array",
		"$.keys[] object", "$.keys[].extra number", "$.keys[].id string",
		"$.ok bool", "$.token string",
	}
	if !slices.Equal(got, want) {
		t.Errorf("shapeOf = %q, want %q", got, want)
	}
	for _, s := range got {
		for _, value := range []string{"s3cret-value", "k1", "k2", "true", "3"} {
			if strings.Contains(s, value) {
				t.Errorf("shape %q carries the value %q", s, value)
			}
		}
	}
	if got := shapeOf([]byte("not json")); got != nil {
		t.Errorf("shapeOf(not json) = %q, want none", got)
	}
}

// The recorder answers the client exactly as the upstream did, and writes the
// request's shape beside the answer's.
func TestTheRecorderPassesAnswersThroughAndRecordsBothShapes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		if _, err := w.Write([]byte(`{"echo":` + string(sent) + `}`)); err != nil {
			t.Errorf("the upstream's write: %v", err)
		}
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var got []exchange
	rec := httptest.NewServer(recorder(target, func(e exchange) error { mu.Lock(); got = append(got, e); mu.Unlock(); return nil }))
	defer rec.Close()

	// The server's own client, not http.Post: the shared default transport is
	// one another test's CloseIdleConnections can break a request on.
	resp, err := rec.Client().Post(rec.URL+"/v1/things", "application/json", strings.NewReader(`{"name":"n"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || string(body) != `{"echo":{"name":"n"}}` {
		t.Errorf("the client got %d %s, want the upstream's 201 and body", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("recorded %d exchanges, want 1", len(got))
	}
	e := got[0]
	if e.Method != http.MethodPost || e.Path != "/v1/things" || e.Status != http.StatusCreated {
		t.Errorf("recorded %s %s %d", e.Method, e.Path, e.Status)
	}
	if !slices.Equal(e.Request, []string{"$ object", "$.name string"}) {
		t.Errorf("request shape = %q", e.Request)
	}
	if !slices.Contains(e.Response, "$.echo.name string") {
		t.Errorf("response shape = %q", e.Response)
	}
}

const testSpec = `
paths:
  /v1/clusters/{id}:
    get: {}
  /v1/clusters/{id}/cancel:
    post: {}
  /v1/clusters/default:
    get: {}
`

// compareFiles writes the spec, both recordings and the accepted list, and
// runs compare on them.
func compareFiles(t *testing.T, fake, real []exchange, accepted []acceptedDiff, flags ...string) (bool, string, error) {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, data []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	recording := func(es []exchange) []byte {
		var b bytes.Buffer
		for _, e := range es {
			line, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			b.Write(append(line, '\n'))
		}
		return b.Bytes()
	}
	args := []string{
		"-spec", write("spec.yaml", []byte(testSpec)),
		"-fake", write("fake.ndjson", recording(fake)),
		"-real", write("real.ndjson", recording(real)),
	}
	if accepted != nil {
		raw, err := json.Marshal(accepted)
		if err != nil {
			t.Fatal(err)
		}
		args = append(args, "-accepted", write("accepted.json", raw))
	}
	args = append(args, flags...)
	var out bytes.Buffer
	clean, err := compare(args, &out)
	return clean, out.String(), err
}

func get(path string, response ...string) exchange {
	return exchange{Method: "GET", Path: path, Status: 200, Response: response}
}

const getCluster = "GET /v1/clusters/{id} 200"

// An answer the recorder can't write fails the request, so the step that
// sent it fails, and no run compares a recording with holes in it.
func TestAnAnswerTheRecorderCannotWriteFailsTheRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewServer(recorder(target, func(exchange) error { return errors.New("disk full") }))
	defer rec.Close()
	resp, err := rec.Client().Get(rec.URL + "/v1/things")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("the client got %d, want 502 when the answer can't be recorded", resp.StatusCode)
	}
}

func TestCompareNamesEachDifferenceAndItsSide(t *testing.T) {
	fake := []exchange{get("/v1/clusters/a", "$ object", "$.id string", "$.message string", "$.count string")}
	real := []exchange{get("/v1/clusters/b", "$ object", "$.id string", "$.uid string", "$.count number")}
	clean, out, err := compareFiles(t, fake, real, nil)
	if err != nil {
		t.Fatal(err)
	}
	if clean {
		t.Errorf("compare passed with three differences:\n%s", out)
	}
	for _, want := range []string{
		"response $.message: the fake answers it, the real API never did",
		"response $.uid: the real API answers it, the fake never did",
		"response $.count: the fake answers string, the real API number",
		"1 operations compared, 3 unexcused differences",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in:\n%s", want, out)
		}
	}
}

func TestAnAcceptedLineExcusesItsDifferenceAndASubtree(t *testing.T) {
	fake := []exchange{get("/v1/clusters/a", "$ object", "$.id string")}
	real := []exchange{get("/v1/clusters/a", "$ object", "$.id string", "$.uid string", "$.health object", "$.health.reason string")}
	accepted := []acceptedDiff{
		{Operation: getCluster, Field: "response $.uid", Side: "real", Reason: "not read"},
		{Operation: getCluster, Field: "response $.health*", Side: "real", Reason: "landscape data"},
	}
	clean, out, err := compareFiles(t, fake, real, accepted)
	if err != nil {
		t.Fatal(err)
	}
	if !clean || !strings.Contains(out, "0 unexcused differences, 3 accepted, 0 stale") {
		t.Errorf("the accepted lines didn't excuse every difference:\n%s", out)
	}

	// The same line on the wrong side excuses nothing, and goes stale.
	accepted[0].Side = "fake"
	clean, out, err = compareFiles(t, fake, real, accepted)
	if err != nil {
		t.Fatal(err)
	}
	if clean || !strings.Contains(out, "accepted, but no longer differs: "+getCluster+" response $.uid (fake)") {
		t.Errorf("a line for the wrong side passed:\n%s", out)
	}
}

func TestAStaleAcceptedLineFails(t *testing.T) {
	same := []exchange{get("/v1/clusters/a", "$ object", "$.id string")}
	accepted := []acceptedDiff{{Operation: getCluster, Field: "response $.uid", Side: "real", Reason: "was a difference once"}}
	clean, out, err := compareFiles(t, same, same, accepted)
	if err != nil {
		t.Fatal(err)
	}
	if clean || !strings.Contains(out, "1 stale accepted lines") {
		t.Errorf("a line that excuses nothing passed:\n%s", out)
	}
}

// A run that covers only some operations, such as hardware mode's, lists the
// accepted lines it didn't see without failing on them: the full run on kind
// is what keeps the list from going stale. A difference still fails it.
func TestAPartialRunListsAcceptedLinesItDidNotSeeAndPasses(t *testing.T) {
	same := []exchange{get("/v1/clusters/a", "$ object", "$.id string")}
	accepted := []acceptedDiff{{Operation: getCluster, Field: "response $.uid", Side: "real", Reason: "was a difference once"}}
	clean, out, err := compareFiles(t, same, same, accepted, "-partial")
	if err != nil {
		t.Fatal(err)
	}
	if !clean || !strings.Contains(out, "not seen in this partial run: "+getCluster+" response $.uid (real)") ||
		!strings.Contains(out, "0 stale accepted lines, 1 not seen") {
		t.Errorf("a partial run failed on, or didn't list, a line it didn't see:\n%s", out)
	}

	differs := []exchange{get("/v1/clusters/a", "$ object", "$.id string", "$.extra string")}
	clean, out, err = compareFiles(t, same, differs, accepted, "-partial")
	if err != nil {
		t.Fatal(err)
	}
	if clean || !strings.Contains(out, "1 unexcused differences") {
		t.Errorf("a partial run passed with a difference:\n%s", out)
	}
}

func TestAnAcceptedLineNeedsAReason(t *testing.T) {
	same := []exchange{get("/v1/clusters/a", "$ object")}
	_, _, err := compareFiles(t, same, same, []acceptedDiff{{Operation: getCluster, Field: "response $.uid", Side: "real"}})
	if err == nil || !strings.Contains(err.Error(), "has no reason") {
		t.Errorf("err = %v, want one naming the missing reason", err)
	}
}

// A path is named by the most literal template that matches it, so
// /clusters/default isn't read as a cluster called "default", and a path no
// template matches, such as the fake's own harness calls, is left out.
func TestOperationsAreNamedByTheMostLiteralTemplate(t *testing.T) {
	// The fake's default answers one shape and a cluster another, so if
	// both were read as GET /v1/clusters/{id}, compare would find drift.
	fakeDefault := get("/v1/clusters/default", "$ object", "$.only string")
	realCluster := get("/v1/clusters/a", "$ object", "$.id string")
	harness := exchange{Method: "POST", Path: "/_harness/refuse-reads", Status: 200, Response: []string{"$ object"}}
	clean, out, err := compareFiles(t, []exchange{fakeDefault, harness}, []exchange{realCluster}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !clean || !strings.Contains(out, "0 operations compared") || strings.Contains(out, "_harness") {
		t.Errorf("want GET /v1/clusters/default and GET /v1/clusters/{id} kept apart, and the harness call left out:\n%s", out)
	}
}
