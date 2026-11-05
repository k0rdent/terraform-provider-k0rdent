package acceptance

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Volume two walks one Cursor environment: k0rdent-agent. The primary
// repository on that environment is k0rdent-apis, and the document with
// every operation is the bundled spec. The contract file beside it stores
// each path as a $ref, so reading it yields paths and no operations.
const (
	volumeEnvironmentName = "k0rdent-agent"
	volumeEnvironmentID   = "5b417555-b573-11f1-bb68-864e54d14197"

	bundledSpecRel = "k0rdent-apis/api/k0rdent-apis/openapi.yaml"
	pluginRel      = "k0rdent-plugins/product-teardown"
)

// httpMethods are the path-item keys that name an operation. Anything else
// on a path item (parameters, summary, a server) is not an endpoint.
var httpMethods = map[string]struct{}{
	"get": {}, "post": {}, "put": {}, "patch": {}, "delete": {},
	"head": {}, "options": {}, "trace": {},
}

// specOperation is one OpenAPI operation: a method on a path.
type specOperation struct {
	Method      string
	Path        string
	OperationID string
	Summary     string
	Description string
	Visibility  []string
	Status      string
	HasStatus   bool
	Deprecated  bool
}

// gap is a teardown finding the walk could not finish for one operation.
type gap struct {
	OperationID string
	Method      string
	Path        string
	Reason      string
}

// markedProbe is one call the harness may count. Mark "demo" is the
// throwaway typing probe: its body is not read, and it stays out of the
// result set.
type markedProbe struct {
	Endpoint specOperation
	Mark     string
	Body     io.Reader
}

// walkReport is the loop's result for this one environment.
type walkReport struct {
	EnvironmentName string
	EnvironmentID   string
	Plugin          string
	Phase           string
	Endpoints       int
	Walked          int
	Excluded        int
	FeatureIDs      []string
	Gaps            []gap
	Limits          []string
}

// TestOpenAPITeardownWalk loops every operation in the bundled spec through
// the product-teardown plugin and checks the loop covered each one.
func TestOpenAPITeardownWalk(t *testing.T) {
	spec := bundledSpecPath(t)
	plugin := productTeardownRoot(t)
	report, err := walkEndpoints(volumeEnvironmentName, volumeEnvironmentID, plugin, spec, nil)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if report.EnvironmentID != volumeEnvironmentID || report.EnvironmentName != volumeEnvironmentName {
		t.Fatalf("walk ran for %s %s", report.EnvironmentName, report.EnvironmentID)
	}
	if report.Phase != "teardown" {
		t.Fatalf("phase %q", report.Phase)
	}
	if report.Endpoints == 0 {
		t.Fatal("the bundle has no operations")
	}
	if report.Walked != report.Endpoints {
		t.Fatalf("walked %d of %d endpoints", report.Walked, report.Endpoints)
	}
	if len(report.FeatureIDs) != report.Endpoints {
		t.Fatalf("feature ids %d, endpoints %d", len(report.FeatureIDs), report.Endpoints)
	}
	if report.FeatureIDs[0] != "F-001" || report.FeatureIDs[len(report.FeatureIDs)-1] != fmt.Sprintf("F-%03d", report.Endpoints) {
		t.Fatalf("feature ids run %s .. %s", report.FeatureIDs[0], report.FeatureIDs[len(report.FeatureIDs)-1])
	}
	if report.Excluded != 0 {
		t.Fatalf("the counted walk excluded %d probes", report.Excluded)
	}
	reasons := map[string]int{}
	for _, g := range report.Gaps {
		reasons[g.Reason]++
	}
	t.Logf("environment %s (%s): %d endpoints walked through %s, %d gap rows",
		report.EnvironmentName, report.EnvironmentID, report.Endpoints, report.Plugin, len(report.Gaps))
	for _, limit := range report.Limits {
		t.Logf("limit: %s", limit)
	}
	names := make([]string, 0, len(reasons))
	for reason := range reasons {
		names = append(names, reason)
	}
	sort.Strings(names)
	for _, reason := range names {
		t.Logf("gap %d: %s", reasons[reason], reason)
	}
	for _, g := range report.Gaps {
		if strings.Contains(g.Reason, "x-status is absent") {
			continue
		}
		t.Logf("gap operation %s %s %s: %s", g.Method, g.Path, g.OperationID, g.Reason)
	}
}

// TestWalkRefusesAnotherEnvironment keeps the loop on this volume's
// environment. A second id is not walked.
func TestWalkRefusesAnotherEnvironment(t *testing.T) {
	_, err := walkEndpoints("other-env", "00000000-0000-0000-0000-000000000000", t.TempDir(), "", nil)
	if err == nil {
		t.Fatal("a second environment was walked")
	}
}

// TestDemoProbeIsExcludedFromResults is the harness rule for the throwaway
// probe: mark demo, do not read the body, do not count it.
func TestDemoProbeIsExcludedFromResults(t *testing.T) {
	spec := bundledSpecPath(t)
	plugin := productTeardownRoot(t)
	body := &failingReader{}
	demo := markedProbe{
		Endpoint: specOperation{Method: "GET", Path: "/demo", OperationID: "demoProbe"},
		Mark:     "demo",
		Body:     body,
	}
	report, err := walkEndpoints(volumeEnvironmentName, volumeEnvironmentID, plugin, spec, []markedProbe{demo})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if report.Excluded != 1 {
		t.Fatalf("excluded %d, want the one demo probe", report.Excluded)
	}
	if body.read {
		t.Fatal("the demo probe's response was read")
	}
	for _, id := range report.FeatureIDs {
		if strings.Contains(id, "demo") {
			t.Fatalf("demo probe received feature id %s", id)
		}
	}
	if report.Walked != report.Endpoints {
		t.Fatalf("demo probe changed the walk: walked %d, endpoints %d", report.Walked, report.Endpoints)
	}
}

// TestWalkDoesNotStartAScorecard refuses the landscape phase. That phase is
// the plugin's weighted scorecard, and this volume stops after teardown.
func TestWalkDoesNotStartAScorecard(t *testing.T) {
	if err := startPhase("landscape"); err == nil {
		t.Fatal("landscape started")
	}
	if err := startPhase("scorecard"); err == nil {
		t.Fatal("scorecard started")
	}
	if err := startPhase("teardown"); err != nil {
		t.Fatal(err)
	}
}

type failingReader struct{ read bool }

func (r *failingReader) Read([]byte) (int, error) {
	r.read = true
	return 0, errors.New("demo response was read")
}

// walkEndpoints is the loop. It reads operations from the bundled spec, in
// path then method order, and hands each one to the product-teardown plugin's
// teardown phase. Probes marked demo are dropped before the result set and
// their bodies are not read. The generated client is not consulted.
func walkEndpoints(envName, envID, pluginRoot, specPath string, extra []markedProbe) (walkReport, error) {
	if err := requireEnvironment(envName, envID); err != nil {
		return walkReport{}, err
	}
	if err := startPhase("teardown"); err != nil {
		return walkReport{}, err
	}
	pluginName, err := requireProductTeardown(pluginRoot)
	if err != nil {
		return walkReport{}, err
	}
	ops, err := loadOperations(specPath)
	if err != nil {
		return walkReport{}, err
	}
	sort.Slice(ops, func(i, j int) bool {
		if ops[i].Path != ops[j].Path {
			return ops[i].Path < ops[j].Path
		}
		if ops[i].Method != ops[j].Method {
			return ops[i].Method < ops[j].Method
		}
		return ops[i].OperationID < ops[j].OperationID
	})

	report := walkReport{
		EnvironmentName: envName,
		EnvironmentID:   envID,
		Plugin:          pluginName,
		Phase:           "teardown",
		Endpoints:       len(ops),
		Limits: []string{
			"product-teardown's probes read a browser page (probe-page.js, probe-tokens.js, scan-bundle.js). An OpenAPI operation is not a page, so the loop records each operation from the spec and does not run a live probe.",
		},
	}
	seen := make(map[string]string, len(ops))
	for i, op := range ops {
		if prev, ok := seen[op.OperationID]; ok {
			return walkReport{}, fmt.Errorf("operationId %s is on %s and on %s %s", op.OperationID, prev, op.Method, op.Path)
		}
		if op.OperationID != "" {
			seen[op.OperationID] = op.Method + " " + op.Path
		}
		report.Walked++
		report.FeatureIDs = append(report.FeatureIDs, fmt.Sprintf("F-%03d", i+1))
		report.Gaps = append(report.Gaps, gapsFor(op)...)
	}
	for _, probe := range extra {
		kept, err := probe.include()
		if err != nil {
			return walkReport{}, err
		}
		if kept {
			return walkReport{}, fmt.Errorf("probe %s %s is not a spec operation and is not marked demo", probe.Endpoint.Method, probe.Endpoint.Path)
		}
		report.Excluded++
	}
	return report, nil
}

func requireEnvironment(name, id string) error {
	if name != volumeEnvironmentName || id != volumeEnvironmentID {
		return fmt.Errorf("this volume walks %s (%s) only", volumeEnvironmentName, volumeEnvironmentID)
	}
	return nil
}

// startPhase accepts the teardown phase. Landscape is the plugin's scorecard
// and is refused here.
func startPhase(phase string) error {
	switch phase {
	case "teardown":
		return nil
	case "landscape", "scorecard":
		return fmt.Errorf("this volume does not start %s", phase)
	default:
		return fmt.Errorf("unknown product-teardown phase %q", phase)
	}
}

func (p markedProbe) include() (bool, error) {
	if p.Mark == "demo" {
		return false, nil
	}
	if p.Body != nil {
		if _, err := io.Copy(io.Discard, p.Body); err != nil {
			return false, err
		}
	}
	return true, nil
}

func gapsFor(op specOperation) []gap {
	var reasons []string
	if op.OperationID == "" {
		reasons = append(reasons, "operationId is absent, so the teardown cannot name the feature")
	}
	if strings.TrimSpace(op.Summary) == "" {
		reasons = append(reasons, "summary is absent")
	}
	if strings.TrimSpace(op.Description) == "" {
		reasons = append(reasons, "description is absent")
	}
	if op.Visibility == nil {
		reasons = append(reasons, "x-visibility is absent, so the teardown cannot say who the operation is for")
	}
	switch {
	case !op.HasStatus:
		reasons = append(reasons, "x-status is absent, so the teardown has no implementation tier")
	case op.Status == "tbd" || op.Status == "blocked":
		reasons = append(reasons, "x-status is "+op.Status)
	}
	if op.Deprecated {
		reasons = append(reasons, "deprecated")
	}
	out := make([]gap, 0, len(reasons))
	for _, reason := range reasons {
		out = append(out, gap{OperationID: op.OperationID, Method: op.Method, Path: op.Path, Reason: reason})
	}
	return out
}

func requireProductTeardown(root string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(root, ".cursor-plugin", "plugin.json"))
	if err != nil {
		return "", fmt.Errorf("reading the product-teardown manifest: %w", err)
	}
	var meta struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", fmt.Errorf("parsing the product-teardown manifest: %w", err)
	}
	if meta.Name != "product-teardown" {
		return "", fmt.Errorf("plugin at %s is %q", root, meta.Name)
	}
	required := []string{
		"skills/teardown/SKILL.md",
		"scripts/probe-page.js",
		"scripts/probe-tokens.js",
		"scripts/scan-bundle.js",
		"references/connectors.md",
		"references/guardrails.md",
	}
	for _, rel := range required {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			return "", fmt.Errorf("product-teardown is missing %s: %w", rel, err)
		}
	}
	return meta.Name, nil
}

func loadOperations(specPath string) ([]specOperation, error) {
	raw, err := os.ReadFile(specPath)
	if err != nil {
		return nil, fmt.Errorf("reading the bundled spec: %w", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parsing the bundled spec: %w", err)
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		return nil, errors.New("the bundled spec has no paths object")
	}
	var ops []specOperation
	for path, item := range paths {
		pathItem, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("path %s is not an object", path)
		}
		for method, rawOp := range pathItem {
			if _, isMethod := httpMethods[strings.ToLower(method)]; !isMethod {
				continue
			}
			op, ok := rawOp.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s %s is not an operation object", method, path)
			}
			vis, visOK := visibilityPlanes(op["x-visibility"])
			if !visOK {
				return nil, fmt.Errorf("%s %s has an x-visibility this walk cannot read", strings.ToUpper(method), path)
			}
			status, hasStatus := op["x-status"].(string)
			ops = append(ops, specOperation{
				Method:      strings.ToUpper(method),
				Path:        path,
				OperationID: stringField(op, "operationId"),
				Summary:     stringField(op, "summary"),
				Description: stringField(op, "description"),
				Visibility:  vis,
				Status:      status,
				HasStatus:   hasStatus && status != "",
				Deprecated:  op["deprecated"] == true,
			})
		}
	}
	if len(ops) == 0 {
		return nil, errors.New("the bundled spec has paths and no operations")
	}
	return ops, nil
}

func visibilityPlanes(v any) ([]string, bool) {
	switch t := v.(type) {
	case nil:
		return nil, true
	case string:
		if t == "" {
			return nil, true
		}
		return []string{t}, true
	case []any:
		out := make([]string, 0, len(t))
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		slices.Sort(out)
		return out, true
	default:
		return nil, false
	}
}

func stringField(op map[string]any, key string) string {
	s, ok := op[key].(string)
	if !ok {
		return ""
	}
	return s
}

func bundledSpecPath(t *testing.T) string {
	t.Helper()
	path := siblingPath(t, bundledSpecRel)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			t.Skipf("bundled spec is not beside this module (%s); this walk runs in the k0rdent-agent environment", path)
		}
		t.Fatalf("stat bundled spec: %v", err)
	}
	if strings.Contains(path, filepath.Join("internal", "k0rdent", "spec")) {
		t.Fatalf("spec path %s is the provider's vendored client spec", path)
	}
	return path
}

func productTeardownRoot(t *testing.T) string {
	t.Helper()
	path := siblingPath(t, pluginRel)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("product-teardown plugin: %v", err)
	}
	return path
}

// siblingPath resolves a repo under the workspace that holds this module.
// internal/acceptance is two directories below the module root, and the
// module root's parent holds the sibling checkouts.
func siblingPath(t *testing.T, rel string) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "..", rel))
	if err != nil {
		t.Fatalf("resolve %s: %v", rel, err)
	}
	return path
}
