package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

func TestCreateConflictDiagnostic(t *testing.T) {
	t.Parallel()
	assertAPIStatusDiagnostic(t, http.StatusConflict, "ALREADY_EXISTS")
}

func TestMintLimitDiagnostic(t *testing.T) {
	t.Parallel()
	assertAPIStatusDiagnostic(t, http.StatusTooManyRequests, "RATE_LIMITED")
}

func assertAPIStatusDiagnostic(t *testing.T, status int, reason string) {
	t.Helper()
	var diags diag.Diagnostics
	resp := &http.Response{StatusCode: status, Status: http.StatusText(status)}
	addAPIError(&diags, "create the cluster", resp,
		[]byte(`{"error":{"message":"request rejected","reason":"`+reason+`"}}`), nil)
	if !diags.HasError() {
		t.Fatalf("status %d produced no diagnostic", status)
	}
	if got := diags.Errors()[0].Detail(); !strings.Contains(got, reason) {
		t.Fatalf("diagnostic %q does not contain reason %q", got, reason)
	}
}

func TestDelete404IsIdempotent(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The destroy reads the cluster before it sends a DELETE.
		if r.Method != http.MethodDelete && r.Method != http.MethodGet {
			t.Errorf("method = %s, want a read or a DELETE", r.Method)
		}
		writeTestError(t, w, http.StatusNotFound, "not found")
	}))
	defer server.Close()

	r := &clusterResource{meta: testClusterMeta(t, server.URL, DefaultOperationTimeout)}
	state := testClusterState(t, "already-gone", "/v1/regions/test/compute/cluster-types/test")
	resp := resource.DeleteResponse{State: state}
	r.Delete(context.Background(), resource.DeleteRequest{State: state}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete 404 must succeed, diagnostics: %v", resp.Diagnostics.Errors())
	}
}

// The id has to reach state before the wait, or a create that times out leaves
// a cluster on the platform that nothing refers to. The plan names no id and
// the response starts null, the way Terraform passes a new resource to Create,
// so the id can only be in state afterwards if Create wrote the one the
// platform returned.
func TestPartialApplyStatePreservesID(t *testing.T) {
	// The platform keeps the id a create sends, and the provider sends one on
	// every create, so this server answers with whatever id it was sent.
	var mu sync.Mutex
	sent := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPost:
			var body struct {
				ID string `json:"id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("read create request: %v", err)
			}
			mu.Lock()
			sent = body.ID
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			if err := json.NewEncoder(w).Encode(map[string]string{"id": body.ID}); err != nil {
				t.Errorf("write create response: %v", err)
			}
		case http.MethodGet:
			mu.Lock()
			id := sent
			mu.Unlock()
			if err := json.NewEncoder(w).Encode(map[string]any{
				"id": id, "clusterType": "/v1/regions/test/compute/cluster-types/test",
				"nodePools": []any{}, "nodeCount": 0, "state": "creating", "securityGroups": []any{},
				"selfLink":  "/v1/regions/test/projects/test/compute/clusters/" + id,
				"uid":       "00000000-0000-4000-8000-000000000001",
				"createdAt": "2026-09-22T00:00:00Z", "updatedAt": "2026-09-22T00:00:00Z",
				"health": map[string]string{"state": "unknown"},
			}); err != nil {
				t.Errorf("write read response: %v", err)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	plan := testClusterState(t, "", "/v1/regions/test/compute/cluster-types/test")
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Millisecond)}
	resp := resource.CreateResponse{State: tfsdk.State{
		Raw:    tftypes.NewValue(plan.Raw.Type(), nil),
		Schema: plan.Schema,
	}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan(plan)}, &resp)
	// It fails when the deadline beats the POST, and warns when the wait is
	// what runs out. Either way it must not pass in silence.
	if !resp.Diagnostics.HasError() && len(resp.Diagnostics.Warnings()) == 0 {
		t.Fatal("a create that never settles said nothing")
	}
	var recovered clusterModel
	if diags := resp.State.Get(context.Background(), &recovered); diags.HasError() {
		t.Fatalf("read partial state: %v", diags.Errors())
	}
	mu.Lock()
	want := sent
	mu.Unlock()
	got := recovered.ID.ValueString()
	// The create can time out before the request reaches the server. The
	// state still holds the id the provider generated for it, so the next
	// run can find the cluster if the create did land.
	if want == "" {
		want = got
		if !strings.HasPrefix(got, "tf-") {
			t.Fatalf("partial apply state id = %q, want the generated id the create was about to send", got)
		}
	}
	if got != want {
		t.Fatalf("partial apply state id = %q, want the id the create sent, %q", got, want)
	}
}

func TestCredentialRedaction(t *testing.T) {
	t.Setenv(EnvVar(envClientSecret), "super-secret")
	t.Setenv(EnvVar(envClientID), "service-account-id")
	t.Setenv(EnvVar(envAPIKey), "api-key-value")

	got := redactDiagnosticText("super-secret service-account-id api-key-value Bearer access-token")
	for _, secret := range []string{"super-secret", "service-account-id", "api-key-value", "access-token"} {
		if strings.Contains(got, secret) {
			t.Fatalf("redacted diagnostic still contains %q: %q", secret, got)
		}
	}
	if strings.Count(got, "[REDACTED]") != 4 {
		t.Fatalf("redacted diagnostic = %q, want four redactions", got)
	}
}

func TestAPIErrorRedactsTransportErrors(t *testing.T) {
	t.Setenv(EnvVar(envClientSecret), "super-secret")

	var diags diag.Diagnostics
	addAPIError(&diags, "create the cluster", nil, nil, errors.New("dial failed: super-secret"))
	if !diags.HasError() {
		t.Fatal("transport error produced no diagnostic")
	}
	if got := diags.Errors()[0].Detail(); strings.Contains(got, "super-secret") {
		t.Fatalf("transport diagnostic leaks secret: %q", got)
	}
}

// testTransport is a connection pool for one test alone, the way newMeta gives
// each provider its own. On http.DefaultTransport, a parallel test's
// httptest.Server.Close closes this test's idle connections under it, and a
// DELETE that had just picked one up fails with "http: CloseIdleConnections
// called". Closing this pool at cleanup closes nobody else's.
func testTransport(t *testing.T) *http.Transport {
	t.Helper()
	tr := NewPlatformTransport()
	t.Cleanup(tr.CloseIdleConnections)
	return tr
}

func testClusterMeta(t *testing.T, endpoint string, timeout time.Duration) *Meta {
	t.Helper()
	client, err := k0rdent.New(endpoint, k0rdent.WithTransport(testTransport(t)))
	if err != nil {
		t.Fatalf("new test client: %v", err)
	}
	return &Meta{
		Client: client, Region: "test", Project: "test",
		Wait: waitConfig{Timeout: timeout, Interval: time.Microsecond},
	}
}

// testClusterState builds a cluster object with every attribute null but id and
// cluster_type. An empty id stays null, the way a plan carries an id the
// platform has yet to assign.
func testClusterState(t *testing.T, id, clusterType string) tfsdk.State {
	t.Helper()
	r := &clusterResource{}
	var schemaResp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &schemaResp)
	if schemaResp.Diagnostics.HasError() {
		t.Fatalf("cluster schema: %v", schemaResp.Diagnostics.Errors())
	}
	object, ok := schemaResp.Schema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("cluster schema is not an object")
	}
	values := make(map[string]tftypes.Value, len(object.AttributeTypes))
	for name, attrType := range object.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	if id != "" {
		values["id"] = tftypes.NewValue(tftypes.String, id)
	}
	values["cluster_type"] = tftypes.NewValue(tftypes.String, clusterType)
	return tfsdk.State{Raw: tftypes.NewValue(object, values), Schema: schemaResp.Schema}
}

// A state outside the contract must stop the wait, not sit in it. The timeout
// here is long enough that returning nil would hang the test.
func TestAnUnknownStateStopsTheWait(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id": "odd", "clusterType": "/v1/regions/test/compute/cluster-types/test",
			"nodePools": []any{}, "nodeCount": 0, "state": "quiescing", "securityGroups": []any{},
			"selfLink":  "/v1/regions/test/projects/test/compute/clusters/odd",
			"uid":       "00000000-0000-4000-8000-000000000002",
			"createdAt": "2026-09-22T00:00:00Z", "updatedAt": "2026-09-22T00:00:00Z",
			"health": map[string]string{"state": "unknown"},
		}); err != nil {
			t.Errorf("write read response: %v", err)
		}
	}))
	defer server.Close()

	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Hour)}
	err := r.waitForSettled(context.Background(), clusterTarget{region: "test", project: "test"},
		"odd", "the cluster to settle")
	if err == nil {
		t.Fatal("an unrecognised state was waited on")
	}
	if !strings.Contains(err.Error(), "quiescing") {
		t.Errorf("error does not name the state: %v", err)
	}
}

// The kubeconfig body is YAML and the generated parser rejects it, so the
// download has to read the unparsed response. Serving a body the typed client
// refuses is the point of this test.
func TestTheKubeconfigBodyIsReadWithoutTheTypedParser(t *testing.T) {
	t.Parallel()
	const body = "apiVersion: v1\nkind: Config\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()

	meta := testClusterMeta(t, server.URL, DefaultOperationTimeout)
	got, diags := downloadKubeconfig(context.Background(), meta,
		clusterTarget{region: "test", project: "test"}, "c1")
	if diags.HasError() {
		t.Fatalf("download failed: %v", diags.Errors())
	}
	if got != body {
		t.Errorf("got %q, want %q", got, body)
	}
}

// The contract calls a cluster's `failed` "not a stop condition for a poller",
// so a cluster that recovers must not taint. Two polls of failure then active.
func TestAClusterThatRecoversFromFailedIsNotTainted(t *testing.T) {
	t.Parallel()
	var reads int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reads++
		state := "failed"
		if reads > 2 {
			state = "active"
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id": "flaky", "clusterType": "/v1/regions/test/compute/cluster-types/test",
			"nodePools": []any{}, "nodeCount": 0, "state": state, "securityGroups": []any{},
			"selfLink":  "/v1/regions/test/projects/test/compute/clusters/flaky",
			"uid":       "00000000-0000-4000-8000-000000000003",
			"createdAt": "2026-09-22T00:00:00Z", "updatedAt": "2026-09-22T00:00:00Z",
			"health": map[string]string{"state": "unknown"},
		}); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()

	meta := testClusterMeta(t, server.URL, time.Minute)
	meta.Wait.Interval = time.Millisecond
	meta.Wait.ProvisionalFailureWindow = time.Minute
	r := &clusterResource{meta: meta}
	if err := r.waitForSettled(context.Background(),
		clusterTarget{region: "test", project: "test"}, "flaky", "the cluster to settle"); err != nil {
		t.Fatalf("a recovered cluster was reported as failed: %v", err)
	}
}

// The other half: a cluster that stays failed must be reported, not waited out.
func TestAClusterThatStaysFailedIsReportedWellInsideTheTimeout(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id": "doomed", "clusterType": "/v1/regions/test/compute/cluster-types/test",
			"nodePools": []any{}, "nodeCount": 0, "state": "failed", "securityGroups": []any{},
			"selfLink":  "/v1/regions/test/projects/test/compute/clusters/doomed",
			"uid":       "00000000-0000-4000-8000-000000000004",
			"createdAt": "2026-09-22T00:00:00Z", "updatedAt": "2026-09-22T00:00:00Z",
			"stateMessage": "the workflow gave up",
			"health":       map[string]string{"state": "unknown"},
		}); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()

	meta := testClusterMeta(t, server.URL, time.Hour)
	meta.Wait.Interval = time.Millisecond
	meta.Wait.ProvisionalFailureWindow = 10 * time.Millisecond
	r := &clusterResource{meta: meta}

	err := r.waitForSettled(context.Background(),
		clusterTarget{region: "test", project: "test"}, "doomed", "the cluster to settle")
	if err == nil {
		t.Fatal("a cluster that never recovered was treated as settled")
	}
	if !strings.Contains(err.Error(), "the workflow gave up") {
		t.Errorf("the error drops the platform's reason: %v", err)
	}
}

// A credential exported with a trailing newline is sent trimmed, because
// resolveSettings trims every environment read. Redaction has to look for the
// same form the wire carries or it replaces nothing.
func TestRedactionMatchesTheTrimmedCredential(t *testing.T) {
	t.Setenv(EnvVar(envClientSecret), "super-secret\n")

	got := redactDiagnosticText("upstream echoed super-secret back")
	if strings.Contains(got, "super-secret") {
		t.Fatalf("redacted diagnostic still contains the trimmed secret: %q", got)
	}
}

// The kubeconfig download is the one call whose response body is a credential.
// A 2xx that is not 200 must not put that body in a diagnostic: CheckResponse
// returns nil for every 2xx, so addAPIError would print it.
func TestKubeconfigDownloadNeverQuotesTheBody(t *testing.T) {
	const kubeconfig = "apiVersion: v1\nusers:\n- user:\n    token: not-a-real-token\n"

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.WriteHeader(http.StatusAccepted)
		if _, err := w.Write([]byte(kubeconfig)); err != nil {
			t.Errorf("writing the kubeconfig: %v", err)
		}
	}))
	defer origin.Close()

	meta := testClusterMeta(t, origin.URL, time.Second)
	_, diags := downloadKubeconfig(context.Background(), meta,
		clusterTarget{region: "test", project: "test"}, "cluster-1")

	if !diags.HasError() {
		t.Fatal("a 202 on the kubeconfig download produced no error")
	}
	for _, d := range diags.Errors() {
		if strings.Contains(d.Detail(), "not-a-real-token") || strings.Contains(d.Detail(), "apiVersion") {
			t.Fatalf("the kubeconfig body reached the diagnostic: %q", d.Detail())
		}
	}
}

// A 307 replays the request body, and on the token mint the body is the
// credential, so the redirect must not be followed. The refusal has to be on
// the client the mint actually uses, not merely implemented, so this builds the
// client through newMeta and asserts the credential never reaches the second
// host.
func TestTheMintItselfRefusesARedirect(t *testing.T) {
	var reachedElsewhere int

	elsewhere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reachedElsewhere++
		w.WriteHeader(http.StatusOK)
	}))
	defer elsewhere.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	meta, err := newMeta(Settings{
		Endpoint: origin.URL,
		Credential: CredentialSource{
			Kind:         ServiceAccountCredential,
			ClientID:     "id",
			ClientSecret: "super-secret",
		},
	}, UserAgent("test", "", ""))
	if err != nil {
		t.Fatalf("new meta: %v", err)
	}

	if _, err := meta.Client.ListRegionsWithResponse(context.Background()); err == nil {
		t.Fatal("the mint followed a 307")
	}
	if reachedElsewhere != 0 {
		t.Fatalf("the credential reached the redirect target %d time(s)", reachedElsewhere)
	}
}
