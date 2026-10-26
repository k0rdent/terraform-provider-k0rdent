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
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// groupServer answers the first `refusals` scales with status and body, every
// later one with 200, and every read with a group in state.
func groupServer(t *testing.T, state string, refusals int32, status int, body string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && posts.Add(1) <= refusals {
			w.WriteHeader(status)
			if _, err := w.Write([]byte(body)); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		group := map[string]any{
			"id": "g1", "clusterType": "/v1/regions/test/compute/cluster-types/t", "state": state,
			"nodePools": []any{map[string]any{"nodePool": "workers", "desiredCount": 2}},
		}
		if err := json.NewEncoder(w).Encode(group); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server, &posts
}

func scaleGroup(t *testing.T, server *httptest.Server, timeout time.Duration) diag.Diagnostics {
	t.Helper()
	return scaleGroupWithin(t, server, timeout, time.Minute)
}

// scaleGroupWithin is scaleGroup with the window a 503 is tolerated for.
func scaleGroupWithin(t *testing.T, server *httptest.Server, timeout, window time.Duration) diag.Diagnostics {
	t.Helper()
	meta := testClusterMeta(t, server.URL, timeout)
	meta.Wait.ProvisionalFailureWindow = window
	r := &instanceGroupResource{meta: meta}
	var body k0rdent.InstanceGroupScaleRequest
	pools := derefOrNil(body.NodePools)
	entry := zeroElem(pools)
	entry.NodePool, entry.Size = "workers", 3
	pools = append(pools, entry)
	body.NodePools = &pools
	return r.scaleOnce(context.Background(), clusterTarget{region: "test", project: "test"}, "g1", body)
}

func diagText(diags diag.Diagnostics) string {
	var b strings.Builder
	for _, d := range diags {
		b.WriteString(d.Summary() + "\n" + d.Detail() + "\n")
	}
	return b.String()
}

const (
	settlingBody  = `{"error":{"code":409,"message":"a previous size change on this resource is still settling; retry shortly","requestId":"req-1"}}`
	inUseBody     = `{"error":{"code":409,"message":"conflict","requestId":"req-1","details":[{"reason":"CONFLICT_IN_USE","message":"conflict"}]}}`
	unconfirmed   = `{"error":{"code":503,"message":"the infrastructure service did not answer","requestId":"req-1"}}`
	overQuotaBody = `{"error":{"code":409,"reason":"QUOTA_EXCEEDED","requestId":"req-1",
		"message":"conflict: instance group scale would exceed the quota",
		"details":[{"reason":"QUOTA_EXCEEDED","message":"node_count: requested 3, 1 admissible (project limit binding)",
		"metadata":{"dimension":"node_count","requested":3,"admissible":1,"bindingScope":"project",
		"scopes":[{"scope":"project","targetId":"demo","limit":4,"used":3,"headroom":1}]}}]}}`
)

// "Retry shortly and the same request is admitted." Without the retry an apply
// that grows a group straight after creating it fails.
func TestInstanceGroupScaleRetriesTheSettlingConflict(t *testing.T) {
	server, posts := groupServer(t, "active", 2, http.StatusConflict, settlingBody)
	if diags := scaleGroup(t, server, time.Minute); diags.HasError() || posts.Load() != 3 {
		t.Errorf("posts=%d diags=%s", posts.Load(), diagText(diags))
	}
}

// The contract calls this conflict reason-less, and the service writes it with
// details[].reason CONFLICT_IN_USE. Both have to be waited out.
func TestInstanceGroupScaleRetriesTheConflictAsTheServiceWritesIt(t *testing.T) {
	server, posts := groupServer(t, "active", 2, http.StatusConflict, inUseBody)
	if diags := scaleGroup(t, server, time.Minute); diags.HasError() || posts.Load() != 3 {
		t.Errorf("posts=%d diags=%s", posts.Load(), diagText(diags))
	}
}

// A group re-rendering for a security-group binding in the same apply answers
// the same conflict, and it clears when that finishes.
func TestInstanceGroupScaleWaitsOutAnotherOperation(t *testing.T) {
	server, posts := groupServer(t, "updating", 2, http.StatusConflict, settlingBody)
	if diags := scaleGroup(t, server, time.Minute); diags.HasError() || posts.Load() != 3 {
		t.Errorf("posts=%d diags=%s", posts.Load(), diagText(diags))
	}
}

// "Retry: unlike the 409s above, this condition does clear on its own."
func TestInstanceGroupScaleRetriesAnUnconfirmedPoolSize(t *testing.T) {
	server, posts := groupServer(t, "active", 2, http.StatusServiceUnavailable, unconfirmed)
	if diags := scaleGroup(t, server, time.Minute); diags.HasError() || posts.Load() != 3 {
		t.Errorf("posts=%d diags=%s", posts.Load(), diagText(diags))
	}
}

// A failed group never becomes scalable by waiting, so the conflict is the
// answer, and the diagnostic has to say why.
func TestInstanceGroupScaleReportsAFailedGroupAtOnce(t *testing.T) {
	server, posts := groupServer(t, "failed", 100, http.StatusConflict, settlingBody)
	diags := scaleGroup(t, server, time.Minute)
	text := diagText(diags)
	if !diags.HasError() || posts.Load() != 1 || !strings.Contains(text, "is failed") ||
		!strings.Contains(text, "Request id: req-1") {
		t.Errorf("posts=%d diags=%s", posts.Load(), text)
	}
}

func TestInstanceGroupScaleStopsOnAnUnknownState(t *testing.T) {
	server, posts := groupServer(t, "quiescing", 100, http.StatusConflict, settlingBody)
	diags := scaleGroup(t, server, time.Hour)
	if !diags.HasError() || posts.Load() != 1 || !strings.Contains(diagText(diags), "quiescing") {
		t.Errorf("posts=%d diags=%s", posts.Load(), diagText(diags))
	}
}

// Waiting out the window has a limit, and when it is reached the diagnostic is
// the platform's refusal, not only a timeout.
func TestInstanceGroupScaleTimeoutReportsTheRefusal(t *testing.T) {
	server, _ := groupServer(t, "active", 1<<30, http.StatusConflict, settlingBody)
	// A long poll interval puts the deadline between requests, not inside one.
	meta := testClusterMeta(t, server.URL, 60*time.Millisecond)
	meta.Wait.Interval = 40 * time.Millisecond
	r := &instanceGroupResource{meta: meta}
	var body k0rdent.InstanceGroupScaleRequest
	text := diagText(r.scaleOnce(context.Background(), clusterTarget{region: "test", project: "test"}, "g1", body))
	for _, want := range []string{"409", "still settling", "Request id: req-1", "update` timeout"} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostic lacks %q:\n%s", want, text)
		}
	}
}

// The service also answers 503 for causes that never clear, such as quota
// enforcement it was never configured for. Retrying those for the whole update
// timeout would hold the apply for an hour, so a 503 is retried only briefly,
// and the diagnostic quotes the platform rather than guessing at the cause.
func TestInstanceGroupScaleGivesUpOnAPersistent503(t *testing.T) {
	const body = `{"error":{"code":503,"requestId":"req-1",` +
		`"message":"instance group \"g1\" is quota-accounted but quota enforcement is not configured on this compute instance",` +
		`"details":[{"reason":"UNAVAILABLE","message":"not configured"}]}}`
	server, posts := groupServer(t, "active", 1<<30, http.StatusServiceUnavailable, body)
	done := make(chan diag.Diagnostics, 1)
	go func() { done <- scaleGroupWithin(t, server, time.Hour, 30*time.Millisecond) }()
	select {
	case diags := <-done:
		text := diagText(diags)
		if !diags.HasError() || !strings.Contains(text, "quota enforcement is not configured") ||
			strings.Contains(text, "infrastructure service answers") {
			t.Errorf("posts=%d diags=%s", posts.Load(), text)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("still retrying a persistent 503 after %d posts", posts.Load())
	}
}

// When the timeout runs out while a scale is on its way, the platform may have
// admitted it, so the diagnostic must not say nothing was scaled.
func TestInstanceGroupScaleCutOffMidRequestMayHaveLanded(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && posts.Add(1) > 1 {
			time.Sleep(300 * time.Millisecond)
		}
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			if _, err := w.Write([]byte(settlingBody)); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id": "g1", "clusterType": "/v1/regions/test/compute/cluster-types/t", "state": "active",
		}); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()
	text := diagText(scaleGroup(t, server, 100*time.Millisecond))
	if strings.Contains(text, "Nothing was scaled") || !strings.Contains(text, "-refresh-only") ||
		!strings.Contains(text, "may have") {
		t.Errorf("diagnostic:\n%s", text)
	}
}

// The service builds some quota refusals with no binding scope, and the advice
// must still read as a sentence.
func TestInstanceGroupQuotaRefusalWithoutAScope(t *testing.T) {
	const body = `{"error":{"code":409,"reason":"QUOTA_EXCEEDED","requestId":"req-1","message":"over quota",
		"details":[{"reason":"QUOTA_EXCEEDED","metadata":{"dimension":"node_count","requested":2,"admissible":0}}]}}`
	server, _ := groupServer(t, "active", 100, http.StatusConflict, body)
	text := diagText(scaleGroup(t, server, time.Minute))
	for _, unwanted := range []string{"binding scope .", "the  limit", "at most 0", "under the limit"} {
		if strings.Contains(text, unwanted) {
			t.Errorf("diagnostic has %q:\n%s", unwanted, text)
		}
	}
	for _, want := range []string{"requested 2, admissible 0", "No increase fits"} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostic lacks %q:\n%s", want, text)
		}
	}
}

// Retrying a quota refusal unchanged gets the same answer, so it is reported
// at once, with the numbers needed to fit.
func TestInstanceGroupQuotaRefusalNamesTheNumbers(t *testing.T) {
	server, posts := groupServer(t, "active", 100, http.StatusConflict, overQuotaBody)
	diags := scaleGroup(t, server, time.Minute)
	text := diagText(diags)
	if !diags.HasError() || posts.Load() != 1 {
		t.Fatalf("posts=%d diags=%s", posts.Load(), text)
	}
	for _, want := range []string{
		"node_count", "requested 3", "admissible 1", "binding scope project",
		"project demo: limit 4, used 3, headroom 1", "at most 1 host", "Request id: req-1",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostic lacks %q:\n%s", want, text)
		}
	}
}

// The platform names the organization's scope `tenant` before its quota API
// renamed it and `organization` after. The refusal reads the same from either.
func TestInstanceGroupQuotaRefusalNamesTheOrganizationOneWay(t *testing.T) {
	for _, sent := range []string{tenantScope, organizationScope} {
		t.Run(sent, func(t *testing.T) {
			body := `{"error":{"code":409,"reason":"QUOTA_EXCEEDED","requestId":"req-1","message":"over quota",
				"details":[{"reason":"QUOTA_EXCEEDED","metadata":{"dimension":"node_count","requested":3,
				"admissible":1,"bindingScope":"` + sent + `","scopes":[
				{"scope":"` + sent + `","targetId":"acme","limit":5,"used":4,"headroom":1},
				{"scope":"project","targetId":"demo","limit":9,"used":0,"headroom":9}]}}]}}`
			server, _ := groupServer(t, "active", 100, http.StatusConflict, body)
			text := diagText(scaleGroup(t, server, time.Minute))
			for _, want := range []string{
				"binding scope organization", "organization acme: limit 5, used 4, headroom 1",
				"project demo: limit 9", "under the organization limit",
			} {
				if !strings.Contains(text, want) {
					t.Errorf("diagnostic lacks %q:\n%s", want, text)
				}
			}
			if strings.Contains(text, "tenant") {
				t.Errorf("diagnostic names the old scope:\n%s", text)
			}
		})
	}
}

// Each pool refusal needs a different action, and none of them clears by
// retrying the same request.
func TestInstanceGroupPoolRefusalsSayWhatToDo(t *testing.T) {
	for reason, advice := range map[string]string{
		"POOL_COUNT_MISMATCH":        "-refresh-only",
		"POOL_EMPTY":                 "cannot grow an empty pool",
		"POOL_HOSTS_NOT_PROVISIONED": "has not finished provisioning",
		"RESERVATION_RELEASED":       "-replace",
		"RESERVATION_DRIFTED":        "administrator",
	} {
		t.Run(reason, func(t *testing.T) {
			body := `{"error":{"code":409,"message":"refused","requestId":"req-1","details":[{"reason":"` +
				reason + `","message":"refused"}]}}`
			server, posts := groupServer(t, "active", 100, http.StatusConflict, body)
			diags := scaleGroup(t, server, time.Minute)
			text := diagText(diags)
			if !diags.HasError() || posts.Load() != 1 || !strings.Contains(text, reason) ||
				!strings.Contains(text, advice) {
				t.Errorf("posts=%d diags=%s", posts.Load(), text)
			}
		})
	}
}

// poolValue is a node_pool map for the instance group schema.
func poolValue(t *testing.T, r resource.Resource, sizes map[string]int64) tftypes.Value {
	t.Helper()
	var s resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &s)
	object, ok := s.Schema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema is not an object")
	}
	m, ok := object.AttributeTypes["node_pool"].(tftypes.Map)
	if !ok {
		t.Fatal("node_pool is not a map")
	}
	pool, ok := m.ElementType.(tftypes.Object)
	if !ok {
		t.Fatal("node_pool element is not an object")
	}
	elems := map[string]tftypes.Value{}
	for id, size := range sizes {
		elems[id] = tftypes.NewValue(pool, map[string]tftypes.Value{"size": tftypes.NewValue(tftypes.Number, size)})
	}
	return tftypes.NewValue(m, elems)
}

// planGrowth runs ModifyPlan for a group that grows workers from 6 to 8, against
// a platform reporting the given counts for it.
func planGrowth(t *testing.T, ready, notReady any) diag.Diagnostics {
	t.Helper()
	return planGrowthOf(t, "active", ready, notReady, nil)
}

// planGrowthOf is planGrowth for a group in state, with planned values that
// differ from state other than the pool size.
func planGrowthOf(t *testing.T, state string, ready, notReady any, planned map[string]tftypes.Value) diag.Diagnostics {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		pool := map[string]any{"nodePool": "workers", "desiredCount": 6}
		if ready != nil {
			pool["readyCount"], pool["notReadyCount"] = ready, notReady
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id": "g1", "clusterType": "/v1/regions/test/compute/cluster-types/t", "state": state,
			"nodePools": []any{pool},
		}); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()

	r := guardedModifier(t, &instanceGroupResource{meta: testClusterMeta(t, server.URL, time.Minute)},
		instanceGroupTenantState)
	common := map[string]tftypes.Value{
		"id": str("g1"), "region": str("test"), "project": str("test"),
		"cluster_type": str("/v1/regions/test/compute/cluster-types/t"),
	}
	with := func(size int64) map[string]tftypes.Value {
		out := map[string]tftypes.Value{"node_pool": poolValue(t, r, map[string]int64{"workers": size})}
		for k, v := range common {
			out[k] = v
		}
		return out
	}
	stateRaw, s := testValue(t, r, with(6))
	plan := with(8)
	for k, v := range planned {
		plan[k] = v
	}
	planRaw, _ := testValue(t, r, plan)
	resp := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: planRaw, Schema: s.Schema},
		Plan:   tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
		State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
	}, &resp)
	return resp.Diagnostics
}

// A pool whose desired size and held hosts disagree is refused any scale, so a
// plan that grows it is told before the apply fails.
func TestInstanceGroupPlanWarnsWhenAGrownPoolHasDiverged(t *testing.T) {
	diags := planGrowth(t, 3, 1)
	text := diagText(diags)
	if diags.HasError() || diags.WarningsCount() != 1 ||
		!strings.Contains(text, "asks for 6") || !strings.Contains(text, "holds 4") {
		t.Errorf("diags=%s", text)
	}
}

// Agreeing counts, or counts the platform could not read, are not a warning.
func TestInstanceGroupPlanDoesNotWarnWithoutADivergence(t *testing.T) {
	for name, counts := range map[string][2]any{"agree": {5, 1}, "absent": {nil, nil}} {
		t.Run(name, func(t *testing.T) {
			if diags := planGrowth(t, counts[0], counts[1]); len(diags) != 0 {
				t.Errorf("diags=%s", diagText(diags))
			}
		})
	}
}

// While a scale is in flight desiredCount is already the new target and the
// hosts are still arriving, which is not a divergence.
func TestInstanceGroupPlanDoesNotWarnWhileTheGroupIsBusy(t *testing.T) {
	for _, state := range []string{"updating", "creating"} {
		if diags := planGrowthOf(t, state, 3, 1, nil); len(diags) != 0 {
			t.Errorf("%s: diags=%s", state, diagText(diags))
		}
	}
}

// A plan that replaces the group never sends the scale the warning is about.
// Each one permits the replacement: refused, it is an error and not a warning.
func TestInstanceGroupPlanDoesNotWarnOnAReplacement(t *testing.T) {
	allow := tftypes.NewValue(tftypes.Bool, true)
	for name, planned := range map[string]map[string]tftypes.Value{
		"cluster_type": {"cluster_type": str("/v1/regions/test/compute/cluster-types/other"), "allow_replace": allow},
		"id":           {"id": str("g2"), "allow_replace": allow},
		"project":      {"project": str("elsewhere"), "allow_replace": allow},
	} {
		if diags := planGrowthOf(t, "active", 3, 1, planned); len(diags) != 0 {
			t.Errorf("%s: diags=%s", name, diagText(diags))
		}
	}
}

// deleteServer accepts a delete and then answers reads in the order given,
// where "" is a 404.
func deleteServer(t *testing.T, states ...string) *httptest.Server {
	t.Helper()
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		i := int(reads.Add(1)) - 1
		if i >= len(states) {
			i = len(states) - 1
		}
		if states[i] == "" {
			w.WriteHeader(http.StatusNotFound)
			if _, err := w.Write([]byte(`{"error":{"code":404,"message":"not found"}}`)); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"id": "g1", "clusterType": "/v1/regions/test/compute/cluster-types/t", "state": states[i],
		}); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func waitDeleted(t *testing.T, server *httptest.Server, window time.Duration) error {
	t.Helper()
	meta := testClusterMeta(t, server.URL, time.Minute)
	meta.Wait.ProvisionalFailureWindow = window
	r := &instanceGroupResource{meta: meta}
	return instanceGroupStates.awaitDeleted(context.Background(), meta.Wait, "the instance group to finish deleting",
		r.reader(clusterTarget{region: "test", project: "test"}, "g1"))
}

// "From creating, the group stays in creating while its create is cancelled;
// a follow-up GET shows creating, then briefly failed, then 404." Destroying a
// group whose create timed out goes exactly this way.
func TestInstanceGroupDeleteWaitsThroughACancelledCreate(t *testing.T) {
	server := deleteServer(t, "creating", "creating", "failed", "failed", "")
	if err := waitDeleted(t, server, time.Minute); err != nil {
		t.Errorf("wait: %v", err)
	}
}

// "If the workflow fails, the group settles to failed and delete may be
// retried." A failure that stays is reported once the window passes.
func TestInstanceGroupDeleteReportsATeardownThatStaysFailed(t *testing.T) {
	server := deleteServer(t, "deleting", "failed")
	err := waitDeleted(t, server, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "as failed") || !strings.Contains(err.Error(), "stayed that way") {
		t.Errorf("wait: %v", err)
	}
}

func TestInstanceGroupDeleteStopsOnAnUnknownState(t *testing.T) {
	server := deleteServer(t, "quiescing")
	err := waitDeleted(t, server, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "quiescing") {
		t.Errorf("wait: %v", err)
	}
}
