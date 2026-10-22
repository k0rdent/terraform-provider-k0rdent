package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// kubeconfigPlatform serves the kubeconfig read, the deprecated list and the
// cluster read. The kubeconfig is withheld for `withheld` reads, as the
// platform does while it issues one after the cluster becomes active.
type kubeconfigPlatform struct {
	withheld     int
	clusterState string // "" answers 404 for the cluster
	kubeStatus   int    // when non-zero, every kubeconfig read answers this
	clusterHangs bool   // the cluster read answers only once the caller gives up
	noState      bool   // the cluster read omits `state`
	issueFailed  bool   // the list reports a `failed` issuance
	gateway404   bool   // the kubeconfig read is a gateway 404: no route
	plain404     bool   // the kubeconfig read is a text/plain 404 from a router
	bodyHangs    bool   // the kubeconfig body stalls until the caller gives up
	secondPage   bool   // the list's first page is empty and points at a second

	body          string // the kubeconfig served, when not testKubeconfig
	failureReason string // the `failed` issuance's reason, when not the default
	stateMessage  string // the cluster read's `stateMessage`, when set

	// window is the provisional failure window, a minute when zero.
	window time.Duration

	kubeReads    atomic.Int32
	listReads    atomic.Int32
	clusterReads atomic.Int32
}

const testKubeconfig = "apiVersion: v1\nkind: Config\nusers:\n- user:\n    token: not-a-real-token\n"

func (p *kubeconfigPlatform) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/kubeconfig"):
			n := int(p.kubeReads.Add(1))
			switch {
			case p.kubeStatus != 0:
				writeTestError(t, w, p.kubeStatus, "service unavailable")
			case p.gateway404:
				writeTestJSON(t, w, http.StatusNotFound, map[string]any{
					"message": "no Route matched with those values", "requestId": "req-gw",
				})
			case p.plain404:
				http.NotFound(w, r)
			case p.bodyHangs:
				w.Header().Set("Content-Type", "application/yaml")
				w.WriteHeader(http.StatusOK)
				if _, err := w.Write([]byte("apiVersion: v1\n")); err != nil {
					t.Errorf("write: %v", err)
				}
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				<-r.Context().Done()
			case n <= p.withheld:
				writeTestError(t, w, http.StatusNotFound, `cluster "c1" has no kubeconfig`)
			default:
				w.Header().Set("Content-Type", "application/yaml")
				body := testKubeconfig
				if p.body != "" {
					body = p.body
				}
				if _, err := w.Write([]byte(body)); err != nil {
					t.Errorf("write: %v", err)
				}
			}
		case strings.HasSuffix(path, "/kubeconfigs"):
			if p.secondPage && r.URL.Query().Get("pageToken") == "" {
				writeTestJSON(t, w, http.StatusOK, map[string]any{"kubeconfigs": []any{}, "nextPageToken": "p2", "total": 1})
				return
			}
			n := int(p.listReads.Add(1))
			items := []any{}
			if p.issueFailed {
				reason := p.failureReason
				if reason == "" {
					reason = "the identity service refused the descriptor"
				}
				items = append(items, map[string]any{
					"id": "kc-0123456789abcdef0123456789abcdee", "status": "failed",
					"failureReason": reason,
					"uid":           "00000000-0000-4000-8000-000000000008",
					"selfLink":      "/v1/regions/test/projects/test/compute/clusters/c1/kubeconfig",
					"createdAt":     "2026-09-22T00:00:00Z",
				})
			} else if n > p.withheld {
				items = append(items, map[string]any{
					"id": "kc-0123456789abcdef0123456789abcdef", "status": "active",
					"uid": "00000000-0000-4000-8000-000000000009", "issuedFor": "someone",
					"selfLink":  "/v1/regions/test/projects/test/compute/clusters/c1/kubeconfig",
					"createdAt": "2026-09-22T00:00:00Z",
				})
			}
			writeTestJSON(t, w, http.StatusOK, map[string]any{"kubeconfigs": items, "total": len(items)})
		case strings.HasSuffix(path, "/clusters/c1"):
			p.clusterReads.Add(1)
			if p.clusterHangs {
				<-r.Context().Done()
				return
			}
			if p.clusterState == "" {
				writeTestError(t, w, http.StatusNotFound, `cluster "c1"`)
				return
			}
			cluster := map[string]any{
				"id": "c1", "clusterType": "/v1/regions/test/compute/cluster-types/test",
				"nodePools": []any{}, "nodeCount": 0, "state": p.clusterState, "securityGroups": []any{},
				"selfLink":  "/v1/regions/test/projects/test/compute/clusters/c1",
				"uid":       "00000000-0000-4000-8000-000000000003",
				"createdAt": "2026-09-22T00:00:00Z", "updatedAt": "2026-09-22T00:00:00Z",
				"health": map[string]string{"state": "unknown"},
			}
			if p.noState {
				delete(cluster, "state")
			}
			if p.stateMessage != "" {
				cluster["stateMessage"] = p.stateMessage
			}
			writeTestJSON(t, w, http.StatusOK, cluster)
		default:
			t.Errorf("unexpected request %s %s", r.Method, path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func writeTestJSON(t *testing.T, w http.ResponseWriter, status int, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Errorf("encode: %v", err)
	}
}

func writeTestError(t *testing.T, w http.ResponseWriter, status int, message string) {
	t.Helper()
	writeTestJSON(t, w, status, map[string]any{"error": map[string]any{
		"code": status, "message": message, "requestId": "req-123",
	}})
}

func (p *kubeconfigPlatform) meta(t *testing.T, timeout time.Duration) *Meta {
	t.Helper()
	meta := testClusterMeta(t, p.serve(t).URL, timeout)
	meta.Wait.ProvisionalFailureWindow = p.window
	if p.window == 0 {
		meta.Wait.ProvisionalFailureWindow = time.Minute
	}
	return meta
}

func openTestKubeconfig(t *testing.T, p *kubeconfigPlatform, timeout time.Duration) (string, diag.Diagnostics) {
	t.Helper()
	meta := p.meta(t, timeout)
	return downloadKubeconfig(context.Background(), meta, clusterTarget{region: "test", project: "test"}, "c1")
}

func detailOf(diags diag.Diagnostics) string {
	var b strings.Builder
	for _, d := range diags.Errors() {
		b.WriteString(d.Summary() + "\n" + d.Detail() + "\n")
	}
	return b.String()
}

// The contract calls the 404 transient: the platform creates a kubeconfig
// automatically once the cluster becomes active.
func TestTheKubeconfigIsWaitedForOnceTheClusterIsActive(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 2, clusterState: "active"}
	got, diags := openTestKubeconfig(t, p, time.Minute)
	if diags.HasError() {
		t.Fatalf("a kubeconfig still being issued failed the open: %s", detailOf(diags))
	}
	if got != testKubeconfig {
		t.Errorf("got %q", got)
	}
	if n := p.kubeReads.Load(); n != 3 {
		t.Errorf("kubeconfig read %d times, want 3", n)
	}
}

func TestTheKubeconfigIsWaitedForWhileTheClusterIsCreating(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 2, clusterState: "creating"}
	if _, diags := openTestKubeconfig(t, p, time.Minute); diags.HasError() {
		t.Fatalf("a cluster still creating failed the open: %s", detailOf(diags))
	}
}

func TestAMissingClusterFailsTheKubeconfigAtOnce(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 1000}
	_, diags := openTestKubeconfig(t, p, time.Minute)
	if !diags.HasError() {
		t.Fatal("a missing cluster opened a kubeconfig")
	}
	got := detailOf(diags)
	for _, want := range []string{`cluster "c1" does not exist`, "req-123"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic lacks %q:\n%s", want, got)
		}
	}
	if n := p.kubeReads.Load(); n != 1 {
		t.Errorf("kubeconfig read %d times, want 1", n)
	}
}

func TestADeletingClusterFailsTheKubeconfigAtOnce(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"deleting", "deleted"} {
		p := &kubeconfigPlatform{withheld: 1000, clusterState: state}
		_, diags := openTestKubeconfig(t, p, time.Minute)
		if !diags.HasError() {
			t.Fatalf("a %s cluster opened a kubeconfig", state)
		}
		if got := detailOf(diags); !strings.Contains(got, state) {
			t.Errorf("diagnostic does not name %q:\n%s", state, got)
		}
		if n := p.kubeReads.Load(); n != 1 {
			t.Errorf("%s: kubeconfig read %d times, want 1", state, n)
		}
	}
}

func TestAnUnrecognisedClusterStateFailsTheKubeconfigAtOnce(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 1000, clusterState: "quiescing"}
	_, diags := openTestKubeconfig(t, p, time.Minute)
	if got := detailOf(diags); !strings.Contains(got, "quiescing") {
		t.Fatalf("diagnostic does not name the state:\n%s", got)
	}
}

// The platform answers 503 for an identity service that is down, and also for
// a caller it will never mint for, so waiting cannot be told apart from hanging.
func TestAnUnavailableKubeconfigIsNotRetried(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{kubeStatus: http.StatusServiceUnavailable, clusterState: "active"}
	_, diags := openTestKubeconfig(t, p, time.Minute)
	if !diags.HasError() {
		t.Fatal("a 503 opened a kubeconfig")
	}
	got := detailOf(diags)
	for _, want := range []string{"503", "service unavailable", "req-123"} {
		if !strings.Contains(got, want) {
			t.Errorf("diagnostic lacks %q:\n%s", want, got)
		}
	}
	if n := p.kubeReads.Load(); n != 1 {
		t.Errorf("kubeconfig read %d times, want 1", n)
	}
}

func TestTheKubeconfigWaitIsBounded(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 1 << 30, clusterState: "active"}
	_, diags := openTestKubeconfig(t, p, 50*time.Millisecond)
	if got := detailOf(diags); !strings.Contains(got, "timed out waiting") {
		t.Fatalf("an unbounded wait, or the wrong error:\n%s", got)
	}
}

// A deadline that lands mid-request is still the timeout, not a transport
// error that reads as the platform failing.
func TestADeadlineMidRequestReportsTheTimeout(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 1 << 30, clusterHangs: true}
	_, diags := openTestKubeconfig(t, p, 50*time.Millisecond)
	if got := detailOf(diags); !strings.Contains(got, "timed out waiting") {
		t.Fatalf("the timeout was reported as something else:\n%s", got)
	}
}

func readTestKubeconfigs(t *testing.T, p *kubeconfigPlatform) diag.Diagnostics {
	t.Helper()
	meta := p.meta(t, time.Minute)
	_, diags := listKubeconfigs(context.Background(), meta, clusterTarget{region: "test", project: "test"}, "c1")
	return diags
}

// In one apply the metadata is read the moment the cluster reports active,
// which is before the platform has issued the kubeconfig.
func TestTheMetadataWaitsForIssuanceOnAnActiveCluster(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 2, clusterState: "active"}
	if diags := readTestKubeconfigs(t, p); diags.HasError() {
		t.Fatalf("an active cluster's kubeconfig being issued failed the read: %s", detailOf(diags))
	}
	if n := p.listReads.Load(); n != 3 {
		t.Errorf("listed %d times, want 3", n)
	}
}

// The list is paged. A record on the second page is as much there as one on
// the first, and reading only the first would wait for an issuance that has
// already happened.
func TestTheMetadataReadsPastTheFirstPage(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{clusterState: "active", secondPage: true}
	meta := p.meta(t, time.Minute)
	items, diags := listKubeconfigs(context.Background(), meta, clusterTarget{region: "test", project: "test"}, "c1")
	if diags.HasError() {
		t.Fatalf("read: %s", detailOf(diags))
	}
	if len(items) != 1 || p.clusterReads.Load() != 0 {
		t.Errorf("got %d records after %d cluster reads, want the one on page two at once", len(items), p.clusterReads.Load())
	}
}

// A data source with a known cluster id reads during plan, and a cluster can
// take many minutes to provision. That is not worth blocking a plan on.
func TestTheMetadataDoesNotWaitForAProvisioningCluster(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 1000, clusterState: "creating"}
	diags := readTestKubeconfigs(t, p)
	if got := detailOf(diags); !strings.Contains(got, "creating") {
		t.Fatalf("diagnostic does not say the cluster is still creating:\n%s", got)
	}
	if n := p.listReads.Load(); n != 1 {
		t.Errorf("listed %d times, want 1", n)
	}
}

// `timeouts.open` bounds the wait, rather than the provider's default.
func TestTheEphemeralOpenTimeoutBoundsTheWait(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 1 << 30, clusterState: "active"}
	e := &clusterKubeconfigEphemeral{meta: p.meta(t, time.Hour)}

	ctx := context.Background()
	var schemaResp ephemeral.SchemaResponse
	e.Schema(ctx, ephemeral.SchemaRequest{}, &schemaResp)
	object, ok := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	if !ok {
		t.Fatal("ephemeral schema is not an object")
	}
	values := make(map[string]tftypes.Value, len(object.AttributeTypes))
	for name, attrType := range object.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	values["cluster_id"] = tftypes.NewValue(tftypes.String, "c1")
	timeoutsType, ok := object.AttributeTypes["timeouts"].(tftypes.Object)
	if !ok {
		t.Fatal("no timeouts block")
	}
	values["timeouts"] = tftypes.NewValue(timeoutsType, map[string]tftypes.Value{
		"open": tftypes.NewValue(tftypes.String, "50ms"),
	})
	config := tfsdk.Config{Raw: tftypes.NewValue(object, values), Schema: schemaResp.Schema}

	resp := ephemeral.OpenResponse{Result: tfsdk.EphemeralResultData{Schema: schemaResp.Schema}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.Open(ctx, ephemeral.OpenRequest{Config: config}, &resp)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timeouts.open did not bound the wait")
	}
	if got := detailOf(resp.Diagnostics); !strings.Contains(got, "timed out waiting") {
		t.Fatalf("wrong error:\n%s", got)
	}
}

// The platform issues a kubeconfig once, on the settle that makes the cluster
// active, and drops a failed issue until the next settle. Waiting the whole
// timeout for it would hold a plan for half an hour.
func TestAnActiveClusterThatIsNeverIssuedAKubeconfigFailsAfterTheWindow(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 1 << 30, clusterState: "active", window: 50 * time.Millisecond}
	started := time.Now()
	_, diags := openTestKubeconfig(t, p, time.Hour)
	if got := detailOf(diags); !strings.Contains(got, `cluster "c1" is active and the platform has not issued it a kubeconfig`) {
		t.Fatalf("wrong error:\n%s", got)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("took %s, the window is 50ms", took)
	}
}

func TestTheMetadataGivesUpOnAnActiveClusterAfterTheWindow(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 1 << 30, clusterState: "active", window: 50 * time.Millisecond}
	diags := readTestKubeconfigs(t, p)
	if got := detailOf(diags); !strings.Contains(got, "has not issued it a kubeconfig") {
		t.Fatalf("wrong error:\n%s", got)
	}
}

// A `failed` entry does not end the wait, because the list also keeps earlier
// attempts and hides a pending one. It does explain the failure.
func TestAFailedIssuanceIsReportedWithItsReason(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 1 << 30, clusterState: "active", issueFailed: true, window: 50 * time.Millisecond}
	_, diags := openTestKubeconfig(t, p, time.Hour)
	if got := detailOf(diags); !strings.Contains(got, "the identity service refused the descriptor") {
		t.Fatalf("diagnostic does not carry the failure reason:\n%s", got)
	}
}

// The reason is read from every page, not the first.
func TestAFailedIssuanceOnALaterPageIsReportedWithItsReason(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 1 << 30, clusterState: "active", issueFailed: true, secondPage: true, window: 50 * time.Millisecond}
	_, diags := openTestKubeconfig(t, p, time.Hour)
	if got := detailOf(diags); !strings.Contains(got, "the identity service refused the descriptor") {
		t.Fatalf("diagnostic does not carry the failure reason:\n%s", got)
	}
}

// A landscape with no route for the read answers 404 from the gateway, which
// will not change by waiting.
func TestAGatewayNotFoundIsNotWaitedOn(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{gateway404: true, clusterState: "active"}
	_, diags := openTestKubeconfig(t, p, time.Hour)
	if got := detailOf(diags); !strings.Contains(got, "no Route matched") {
		t.Fatalf("diagnostic does not carry the gateway's answer:\n%s", got)
	}
	if n := p.clusterReads.Load(); n != 0 {
		t.Errorf("cluster read %d times, want 0", n)
	}
}

// A service's region guard and Go's default router answer a region or path
// they don't serve with a plain-text 404. That isn't the service saying the
// cluster has no kubeconfig yet, and it won't change by waiting either.
func TestAPlainTextNotFoundIsNotWaitedOn(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{plain404: true, clusterState: "active"}
	_, diags := openTestKubeconfig(t, p, time.Hour)
	if got := detailOf(diags); !strings.Contains(got, "without the service's error envelope") {
		t.Fatalf("diagnostic does not say what answered:\n%s", got)
	}
	if n := p.clusterReads.Load(); n != 0 {
		t.Errorf("cluster read %d times, want 0", n)
	}
}

func TestADeadlineDuringTheBodyReportsTheTimeout(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{bodyHangs: true, clusterState: "active"}
	_, diags := openTestKubeconfig(t, p, 50*time.Millisecond)
	if got := detailOf(diags); !strings.Contains(got, "timed out waiting") {
		t.Fatalf("the timeout was reported as something else:\n%s", got)
	}
}

func TestAClusterWithNoStateFailsAtOnce(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 1 << 30, clusterState: "active", noState: true}
	_, diags := openTestKubeconfig(t, p, time.Hour)
	if got := detailOf(diags); !strings.Contains(got, "no state") {
		t.Fatalf("wrong error:\n%s", got)
	}
}

// The timeout advice names a setting each surface actually has.
func TestTheTimeoutAdviceFitsTheSurface(t *testing.T) {
	t.Parallel()
	p := &kubeconfigPlatform{withheld: 1 << 30, clusterState: "active", window: time.Hour}
	_, diags := openTestKubeconfig(t, p, 50*time.Millisecond)
	got := detailOf(diags)
	if !strings.Contains(got, "`timeouts.open`") || strings.Contains(got, "resource's `timeouts` block") {
		t.Errorf("ephemeral advice is wrong:\n%s", got)
	}

	q := &kubeconfigPlatform{withheld: 1 << 30, clusterState: "active", window: time.Hour}
	meta := q.meta(t, 50*time.Millisecond)
	_, diags = listKubeconfigs(context.Background(), meta, clusterTarget{region: "test", project: "test"}, "c1")
	got = detailOf(diags)
	if !strings.Contains(got, "`operation_timeout`") || strings.Contains(got, "`timeouts") {
		t.Errorf("data source advice is wrong:\n%s", got)
	}
}
