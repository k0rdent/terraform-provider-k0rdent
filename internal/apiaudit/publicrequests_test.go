package apiaudit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The route table is derived from the spec, so a regeneration that lost the
// paths would leave every request unmatched and the audit failing for the
// wrong reason. Every operation with a visibility has to have a route.
func TestEveryOperationHasARoute(t *testing.T) {
	for op := range APIVisibility {
		if _, ok := APIRoutes[op]; !ok {
			t.Errorf("%s has a visibility and no method and path: re-run scripts/gen-api-visibility.py", op)
		}
	}
}

func TestOperationForMatchesBySegmentAndMethod(t *testing.T) {
	for _, c := range []struct {
		method, path, want string
	}{
		{"GET", "/v1/regions/us-texas-1/compute/cluster-types", "listClusterTypes"},
		// The same path, a different verb, a different operation: tenants
		// list cluster types and operators create them.
		{"POST", "/v1/regions/us-texas-1/compute/cluster-types", "createClusterType"},
		{"GET", "/v1/regions/us-texas-1/projects/demo/compute/clusters/c-1", "getCluster"},
		{"GET", "/v1/regions/us-texas-1/projects/demo/compute/clusters/c-1?view=full", "getCluster"},
		{"POST", "/v1/regions/global/auth/token", "mintToken"},
		{"GET", "/v1/regions", "listRegions"},
		// A parameter is one segment, never two and never none.
		{"GET", "/v1/regions/us-texas-1/projects/demo/compute/clusters/c-1/extra", ""},
		{"GET", "/v1/regions/us-texas-1/projects//compute/clusters/c-1", ""},
		{"PUT", "/v1/regions/us-texas-1/projects/demo/compute/clusters/c-1", ""},
		{"GET", "/_demo/healthz", ""},
	} {
		got, _ := OperationFor(c.method, c.path)
		if got != c.want {
			t.Errorf("OperationFor(%s %s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// The audit has to be able to fail, or it's a grep with no producer. These
// lines are what the fake writes, one per request.
//
// It audits every request except the harness's own. A request built by hand
// goes out as Go-http-client, never as the provider, and it's exactly the one
// that could reach an operator-only operation, so it's checked, not skipped.
func TestAuditProviderRequestsNamesEachNonPublicRequest(t *testing.T) {
	const provider = `ua="terraform-provider-k0rdent/acc" auth=present`
	const harness = `ua="` + HarnessUserAgent + `"`
	log := strings.Join([]string{
		`2026/10/10 12:00:00 POST /v1/regions/global/auth/token ` + provider,
		`2026/10/10 12:00:00 GET /v1/regions/us-texas-1/compute/cluster-types ` + provider,
		`2026/10/10 12:00:01 POST /v1/regions/us-texas-1/compute/cluster-types ` + provider,
		`2026/10/10 12:00:02 GET /v1/regions/us-texas-1/projects/demo/compute/clusters/c-1/nonsense ` + provider,
		`2026/10/10 12:00:03 GET /_demo/healthz ` + provider,
		// Built by hand with Go's default User-Agent. createOrganization is
		// internal, so this is refused whoever built it.
		`2026/10/10 12:00:04 POST /v1/regions/global/organizations ua="Go-http-client/1.1" auth=present`,
		`2026/10/10 12:00:05 GET /_demo/healthz ua="curl/8.7.1"`,
		// A request line the audit can't read is refused, never dropped.
		`2026/10/10 12:00:06 GET /v1/regions/a b ua="Go-http-client/1.1"`,
		// The harness's own setup calls are what's left out, whatever they ask for.
		`2026/10/10 12:00:07 POST /v1/regions/us-texas-1/compute/cluster-types ` + harness + ` auth=present`,
		`2026/10/10 12:00:08 GET /_harness/ready ` + harness + ` auth=present`,
		// The fake's own messages aren't requests.
		`2026/10/10 12:00:09 fakeapi: write response: broken pipe`,
		``,
	}, "\n")

	got := AuditProviderRequests(log)
	if got.Requests != 8 {
		t.Errorf("read %d requests, want 8", got.Requests)
	}
	if got.Harness != 2 {
		t.Errorf("left out %d harness requests, want 2", got.Harness)
	}
	if want := []string{"listClusterTypes", "mintToken"}; !slices.Equal(got.Public, want) {
		t.Errorf("public operations %v, want %v", got.Public, want)
	}
	want := []string{
		"POST /v1/regions/us-texas-1/compute/cluster-types is createClusterType, which the contract marks internal",
		"GET /v1/regions/us-texas-1/projects/demo/compute/clusters/c-1/nonsense matches no operation in the contract",
		"GET /_demo/healthz matches no operation in the contract",
		"POST /v1/regions/global/organizations is createOrganization, which the contract marks internal",
		"GET /_demo/healthz matches no operation in the contract",
		`can't read this request line, so it can't be shown public: "2026/10/10 12:00:06 GET /v1/regions/a b ua=\"Go-http-client/1.1\""`,
	}
	if !slices.Equal(got.Refused, want) {
		t.Errorf("refused:\n  %s\nwant:\n  %s", strings.Join(got.Refused, "\n  "), strings.Join(want, "\n  "))
	}
	if got.Err() == nil {
		t.Error("an audit with refused requests reported no error")
	}

	clean := AuditProviderRequests(strings.Join(strings.Split(log, "\n")[:2], "\n"))
	if err := clean.Err(); err != nil {
		t.Errorf("a log of public requests failed: %v", err)
	}
	// No provider request at all proves nothing, so it isn't a pass.
	if err := AuditProviderRequests("").Err(); err == nil {
		t.Error("an empty log passed")
	}
	harnessOnly := AuditProviderRequests(strings.Join(strings.Split(log, "\n")[8:10], "\n"))
	if harnessOnly.Harness != 2 || harnessOnly.Err() == nil {
		t.Errorf("a log of only harness requests gave %+v and %v, want two left out and an error", harnessOnly, harnessOnly.Err())
	}
}

// Two routes equally literal that both match a path have to resolve the same
// way every run, not by map order.
func TestOperationForBreaksTiesByOperationID(t *testing.T) {
	routes := compileRoutes(map[string]Route{
		"zeta":  {Method: "GET", Path: "/v1/{a}/x"},
		"alpha": {Method: "GET", Path: "/v1/{b}/x"},
		"mid":   {Method: "GET", Path: "/v1/{c}/x"},
	})
	for range 50 {
		if got, _ := operationFor(routes, "GET", "/v1/q/x"); got != "alpha" {
			t.Fatalf("a tie resolved to %q, want alpha", got)
		}
	}
}

// The provider records the contract it's built against in data/visibility.json,
// and internal/k0rdent/spec/SOURCE.json records the contract the client's spec
// was cut from. They differ when the client was re-cut from a newer contract
// without regenerating the map, and then nothing else fails by name: an
// operation the contract has since added reads as absent, and one it has since
// removed reads as present.
func TestVisibilityWasGeneratedFromThePinnedSpec(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "k0rdent", "spec", "SOURCE.json"))
	if err != nil {
		t.Fatalf("read the client's source record: %v", err)
	}
	var src struct {
		Contract struct {
			ReleaseTag string `json:"releaseTag"`
			SHA256     string `json:"sha256"`
		} `json:"contract"`
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		t.Fatalf("parse internal/k0rdent/spec/SOURCE.json: %v", err)
	}
	if VisibilitySource.ReleaseTag != src.Contract.ReleaseTag || VisibilitySource.SHA256 != src.Contract.SHA256 {
		t.Errorf("data/visibility.json records contract %s (sha256 %s), and the client is generated from %s "+
			"(sha256 %s). Run scripts/gen-api-visibility.py against that contract",
			VisibilitySource.ReleaseTag, VisibilitySource.SHA256, src.Contract.ReleaseTag, src.Contract.SHA256)
	}
}
