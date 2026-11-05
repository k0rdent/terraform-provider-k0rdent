package acceptance

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// The walk for the Cursor environment k0rdent-agent whose primary
// repository is k0rdent-ui (environment edf4fe91-b186-11f1-a3d8-362438fd9788).
// The operation list is that repo's enriched OpenAPI, the document the
// UI client is built from. Each operation is one HTTP call through
// harnessHTTP, the client this suite already uses to talk to the fake.
//
// testdata/k0rdent-ui-operations.json holds the list so the walk runs
// in a checkout that has only this repository. When k0rdent-ui is
// checked out beside this repo, the inventory test reads the spec and
// fails if the list has drifted from it.

const uiOperationsFile = "testdata/k0rdent-ui-operations.json"

// uiAbsentID fills every path parameter that is not one of the fake's
// seeded names, so an item route resolves to an object the suite did
// not create.
const uiAbsentID = "acc-spec-absent"

// uiSeedParams are the fake's seeded names. A path that only contains
// these, on a write, would create or change an object other tests read.
var uiSeedParams = map[string]string{
	"region":  "us-texas-1",
	"project": "demo",
	"orgId":   "org-demo",
}

// uiOperation is one operation from the UI spec: the method, the path
// template, and the operationId.
type uiOperation struct {
	OperationID string `json:"operationId"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Deprecated  bool   `json:"deprecated"`
}

type uiOperationFile struct {
	Source     string        `json:"source"`
	Operations []uiOperation `json:"operations"`
}

// uiProbe is one call in the walk. Auth is the authenticated status.
// AuthSkipped says why the authenticated call was not sent. Err is a
// transport failure. The body of the response is discarded.
//
// UnauthUs and AuthUs are the local fake's round trip for that call,
// in microseconds, from the send until the body has been drained.
// UnauthBytes and AuthBytes are how many body bytes were drained.
// A skipped authenticated write has no AuthUs.
type uiProbe struct {
	OperationID string `json:"operationId"`
	Method      string `json:"method"`
	Path        string `json:"path"`
	Unauth      int    `json:"unauth"`
	UnauthUs    int64  `json:"unauthUs"`
	UnauthBytes int64  `json:"unauthBytes"`
	Auth        int    `json:"auth,omitempty"`
	AuthUs      int64  `json:"authUs,omitempty"`
	AuthBytes   int64  `json:"authBytes,omitempty"`
	AuthSkipped string `json:"authSkipped,omitempty"`
	Err         string `json:"err,omitempty"`
}

// uiLatency is one sample of round trips. Times are microseconds.
type uiLatency struct {
	N     int   `json:"n"`
	MinUs int64 `json:"minUs"`
	P50Us int64 `json:"p50Us"`
	P95Us int64 `json:"p95Us"`
	MaxUs int64 `json:"maxUs"`
	SumUs int64 `json:"sumUs"`
	Bytes int64 `json:"bytes"`
}

// uiWalkReport is what the walk writes. Target is the acceptance fake
// on loopback. The demo probe is not in Results.
type uiWalkReport struct {
	Target     string    `json:"target"`
	Operations int       `json:"operations"`
	Calls      int       `json:"calls"`
	WallUs     int64     `json:"wallUs"`
	Unauth     uiLatency `json:"unauth"`
	Auth       uiLatency `json:"auth"`
	Results    []uiProbe `json:"results"`
}

func TestUISpecWalkInventory(t *testing.T) {
	ops := loadUIOperations(t)
	if len(ops) == 0 {
		t.Fatal("the UI operation list is empty")
	}
	seen := make(map[string]uiOperation, len(ops))
	for _, op := range ops {
		if op.OperationID == "" || op.Method == "" || op.Path == "" {
			t.Errorf("an operation is missing a field: %+v", op)
			continue
		}
		if prev, ok := seen[op.OperationID]; ok {
			t.Errorf("operation %s is listed twice: %s %s and %s %s",
				op.OperationID, prev.Method, prev.Path, op.Method, op.Path)
		}
		seen[op.OperationID] = op
	}
	specPath, ok := findUISpec()
	if !ok {
		t.Logf("UI spec not beside this repo; checking %d operations from %s", len(ops), uiOperationsFile)
		return
	}
	fromSpec := parseUISpec(t, specPath)
	if len(fromSpec) != len(ops) {
		t.Errorf("spec %s has %d operations, %s has %d", specPath, len(fromSpec), uiOperationsFile, len(ops))
	}
	specByID := make(map[string]uiOperation, len(fromSpec))
	for _, op := range fromSpec {
		specByID[op.OperationID] = op
	}
	for id, op := range seen {
		specOp, ok := specByID[id]
		if !ok {
			t.Errorf("%s is in %s and not in %s", id, uiOperationsFile, specPath)
			continue
		}
		if specOp.Method != op.Method || specOp.Path != op.Path || specOp.Deprecated != op.Deprecated {
			t.Errorf("%s: list has %s %s deprecated=%v, spec has %s %s deprecated=%v",
				id, op.Method, op.Path, op.Deprecated, specOp.Method, specOp.Path, specOp.Deprecated)
		}
	}
	for id := range specByID {
		if _, ok := seen[id]; !ok {
			t.Errorf("%s is in %s and not in %s", id, specPath, uiOperationsFile)
		}
	}
}

// TestAccUISpecWalk calls every operation in the UI list. A status,
// including 404, is a result. A transport error fails the test. An
// authenticated write that only names seeded objects is skipped, and
// the skip is a result too: the fake is shared with the rest of the suite.
func TestAccUISpecWalk(t *testing.T) {
	if os.Getenv("TF_ACC") == "" {
		t.Skip("needs the fake, under TF_ACC")
	}
	if endpoint == "" {
		t.Fatal("the fake's endpoint is empty")
	}
	ops := loadUIOperations(t)
	token, err := mintFakeToken()
	if err != nil {
		t.Fatalf("minting a token on the fake: %v", err)
	}

	results := make([]uiProbe, 0, len(ops))
	unauthSample := make([]int64, 0, len(ops))
	authSample := make([]int64, 0, len(ops))
	var unauthBytes, authBytes int64
	var skipped, notOnFake, transport, calls int
	wallStart := time.Now()
	for _, op := range ops {
		path, absent := fillUIPath(op.Path)
		row := uiProbe{OperationID: op.OperationID, Method: op.Method, Path: op.Path}
		call := probeUI(op.Method, endpoint+path, "")
		calls++
		row.UnauthUs = call.us
		row.UnauthBytes = call.bytes
		if call.err != nil {
			transport++
			row.Err = call.err.Error()
			results = append(results, row)
			t.Errorf("%s %s: %v", op.Method, op.Path, call.err)
			continue
		}
		row.Unauth = call.code
		unauthSample = append(unauthSample, call.us)
		unauthBytes += call.bytes
		if call.code == http.StatusNotFound {
			notOnFake++
		}
		if skipAuthWrite(op.Method, op.Path, absent) {
			skipped++
			row.AuthSkipped = "authenticated write names only seeded objects on the shared fake"
			results = append(results, row)
			continue
		}
		call = probeUI(op.Method, endpoint+path, token)
		calls++
		row.AuthUs = call.us
		row.AuthBytes = call.bytes
		if call.err != nil {
			transport++
			row.Err = call.err.Error()
			results = append(results, row)
			t.Errorf("%s %s authenticated: %v", op.Method, op.Path, call.err)
			continue
		}
		row.Auth = call.code
		authSample = append(authSample, call.us)
		authBytes += call.bytes
		results = append(results, row)
	}
	wallUs := time.Since(wallStart).Microseconds()
	if len(results) != len(ops) {
		t.Errorf("recorded %d results for %d operations", len(results), len(ops))
	}
	unauth := summarizeUILatency(unauthSample, unauthBytes)
	auth := summarizeUILatency(authSample, authBytes)
	t.Logf("ui spec walk: %d operations, %d calls, wall %dus, %d not on the fake, %d authenticated writes skipped, %d transport errors",
		len(ops), calls, wallUs, notOnFake, skipped, transport)
	t.Logf("ui spec walk unauth latency µs: n=%d min=%d p50=%d p95=%d max=%d sum=%d bytes=%d",
		unauth.N, unauth.MinUs, unauth.P50Us, unauth.P95Us, unauth.MaxUs, unauth.SumUs, unauth.Bytes)
	t.Logf("ui spec walk auth latency µs: n=%d min=%d p50=%d p95=%d max=%d sum=%d bytes=%d",
		auth.N, auth.MinUs, auth.P50Us, auth.P95Us, auth.MaxUs, auth.SumUs, auth.Bytes)
	writeUIWalk(t, uiWalkReport{
		Target:     "acceptance-fake",
		Operations: len(ops),
		Calls:      calls,
		WallUs:     wallUs,
		Unauth:     unauth,
		Auth:       auth,
		Results:    results,
	})
}

// TestDemoThrowawayLiveTyping is a throwaway. It is excluded from the
// walk: the name does not match TestAccUISpecWalk, the acceptance run
// leaves K0RDENT_DEMO_PROBE unset, and the status is discarded. A
// recorded live-typing probe is what sets the variable.
func TestDemoThrowawayLiveTyping(t *testing.T) {
	if os.Getenv("K0RDENT_DEMO_PROBE") != "1" {
		t.Skip("excluded from the endpoint walk")
	}
	if endpoint == "" {
		t.Log("demo probe: no endpoint")
		return
	}
	_ = probeUI(http.MethodGet, endpoint+"/v1/regions", "")
}

// TestUILatencySummaryRanksTheSample checks the percentile ranks the
// walk logs. The sample is fixed, so the fake is not involved.
func TestUILatencySummaryRanksTheSample(t *testing.T) {
	got := summarizeUILatency([]int64{40, 10, 100, 20, 30}, 9)
	want := uiLatency{N: 5, MinUs: 10, P50Us: 30, P95Us: 100, MaxUs: 100, SumUs: 200, Bytes: 9}
	if got != want {
		t.Fatalf("summarizeUILatency = %+v, want %+v", got, want)
	}
	if zero := summarizeUILatency(nil, 0); zero != (uiLatency{}) {
		t.Fatalf("empty sample = %+v", zero)
	}
}

func loadUIOperations(t *testing.T) []uiOperation {
	t.Helper()
	raw, err := os.ReadFile(uiOperationsFile)
	if err != nil {
		t.Fatalf("reading %s: %v", uiOperationsFile, err)
	}
	var file uiOperationFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parsing %s: %v", uiOperationsFile, err)
	}
	sortUIOperations(file.Operations)
	return file.Operations
}

func parseUISpec(t *testing.T, path string) []uiOperation {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var spec struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	methods := map[string]bool{
		"get": true, "put": true, "post": true, "delete": true,
		"patch": true, "head": true, "options": true, "trace": true,
	}
	var ops []uiOperation
	for path, item := range spec.Paths {
		for method, node := range item {
			if !methods[method] {
				continue
			}
			var decoded struct {
				OperationID string `yaml:"operationId"`
				Deprecated  bool   `yaml:"deprecated"`
			}
			if err := node.Decode(&decoded); err != nil {
				t.Fatalf("decode %s %s: %v", method, path, err)
			}
			ops = append(ops, uiOperation{
				OperationID: decoded.OperationID,
				Method:      strings.ToUpper(method),
				Path:        path,
				Deprecated:  decoded.Deprecated,
			})
		}
	}
	sortUIOperations(ops)
	return ops
}

func sortUIOperations(ops []uiOperation) {
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Path != ops[j].Path {
			return ops[i].Path < ops[j].Path
		}
		if ops[i].Method != ops[j].Method {
			return ops[i].Method < ops[j].Method
		}
		return ops[i].OperationID < ops[j].OperationID
	})
}

// findUISpec looks up the tree from this file for the UI repo's
// enriched spec. K0RDENT_UI_OPENAPI names a spec directly.
func findUISpec() (string, bool) {
	if p := os.Getenv("K0RDENT_UI_OPENAPI"); p != "" {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, true
		}
	}
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", false
	}
	dir := filepath.Dir(file)
	for range 8 {
		cand := filepath.Join(dir, "k0rdent-ui", "packages", "client-ts", "specs", "openapi.enriched.yaml")
		if st, err := os.Stat(cand); err == nil && !st.IsDir() {
			return cand, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", false
}

// fillUIPath substitutes each {param} in an OpenAPI path template.
// absent is true when a parameter took uiAbsentID.
func fillUIPath(template string) (string, bool) {
	var b strings.Builder
	absent := false
	for i, seg := range strings.Split(template, "/") {
		if i > 0 {
			b.WriteByte('/')
		}
		if len(seg) < 3 || seg[0] != '{' || seg[len(seg)-1] != '}' {
			b.WriteString(seg)
			continue
		}
		name := seg[1 : len(seg)-1]
		name = strings.TrimSuffix(name, "*")
		if cut := strings.IndexByte(name, ':'); cut >= 0 {
			name = name[:cut]
		}
		val, ok := uiSeedParams[name]
		if !ok {
			val = uiAbsentID
			absent = true
		}
		b.WriteString(val)
	}
	return b.String(), absent
}

func safeUIMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

// skipAuthWrite reports whether the authenticated call would be a write
// against only seeded objects. The token mint is the one write that
// stays: it creates a token, not a project object.
func skipAuthWrite(method, template string, absent bool) bool {
	if safeUIMethod(method) || absent {
		return false
	}
	return !strings.HasSuffix(template, "/auth/token")
}

// uiCall is one probeUI round trip. us is microseconds. bytes is the
// drained body. err is a transport failure, and the status is then 0.
type uiCall struct {
	code  int
	us    int64
	bytes int64
	err   error
}

// probeUI sends one request and drops the body. The clock starts
// before the send and stops after the body is drained, so us is the
// whole round trip against whatever URL it was given. This walk gives
// it the acceptance fake.
func probeUI(method, rawURL, token string) uiCall {
	var body io.Reader
	if !safeUIMethod(method) {
		body = strings.NewReader("{}")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return uiCall{err: err}
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	start := time.Now()
	resp, err := harnessHTTP.Do(req)
	if err != nil {
		return uiCall{us: time.Since(start).Microseconds(), err: err}
	}
	defer func() { _ = resp.Body.Close() }()
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return uiCall{code: resp.StatusCode, us: time.Since(start).Microseconds(), bytes: n}
}

// summarizeUILatency ranks a sample. p50 and p95 use the nearest rank
// at or above the percentile. An empty sample is the zero value.
func summarizeUILatency(sample []int64, bytes int64) uiLatency {
	if len(sample) == 0 {
		return uiLatency{}
	}
	sorted := append([]int64(nil), sample...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	var sum int64
	for _, v := range sorted {
		sum += v
	}
	return uiLatency{
		N:     len(sorted),
		MinUs: sorted[0],
		P50Us: uiPercentile(sorted, 0.50),
		P95Us: uiPercentile(sorted, 0.95),
		MaxUs: sorted[len(sorted)-1],
		SumUs: sum,
		Bytes: bytes,
	}
}

func uiPercentile(sorted []int64, p float64) int64 {
	i := int(math.Ceil(p*float64(len(sorted)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(sorted) {
		i = len(sorted) - 1
	}
	return sorted[i]
}

func writeUIWalk(t *testing.T, report uiWalkReport) {
	t.Helper()
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatalf("encoding the walk: %v", err)
	}
	raw = append(raw, '\n')
	out := os.Getenv("K0RDENT_UI_WALK_OUT")
	if out == "" {
		out = filepath.Join(t.TempDir(), "ui-spec-walk.json")
	}
	if err := os.WriteFile(out, raw, 0o600); err != nil {
		t.Fatalf("writing %s: %v", out, err)
	}
	t.Logf("ui spec walk results: %s", out)
}
