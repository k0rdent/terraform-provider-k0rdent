package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// These pin what each caller does with every state a cluster can report, end
// to end through the caller, so that moving the decisions somewhere else
// cannot change an outcome unnoticed.

// scriptedCluster answers a destroy from a script: each DELETE takes the next
// status, a 204 once they run out, and each GET the next state, a 404 once
// they run out. A cancel answers cancelStatus, or 200 when it is zero.
type scriptedCluster struct {
	mu           sync.Mutex
	deletes      []int
	reads        []string
	cancelStatus int
	calls        []string
}

func (s *scriptedCluster) serve(t *testing.T) *httptest.Server {
	t.Helper()
	const clusterPath = "/v1/regions/{r}/projects/{p}/compute/clusters/{id}"
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+clusterPath, func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if len(s.reads) == 0 {
			writeTestError(t, w, http.StatusNotFound, "not found")
			return
		}
		state := s.reads[0]
		s.reads = s.reads[1:]
		// Every read names the same operation, so a cancel always names it.
		writeJSON(w, map[string]any{"id": "c1", "state": state,
			"operation": map[string]any{"executionId": "e1", "status": "running"}})
	})
	mux.HandleFunc("DELETE "+clusterPath, func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, "delete")
		if len(s.deletes) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		status := s.deletes[0]
		s.deletes = s.deletes[1:]
		(&cancelAPI{}).refuse(w, status, "the cluster is busy")
	})
	mux.HandleFunc("POST "+clusterPath+"/cancel", func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.calls = append(s.calls, "cancel")
		if s.cancelStatus != 0 {
			(&cancelAPI{}).refuse(w, s.cancelStatus, "cancel refused")
			return
		}
		writeJSON(w, map[string]any{"id": "c1", "state": "failed", "stateReason": "CANCELLED"})
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

// What a destroy does after the platform refuses the delete, for every state
// the cluster can then be read in.
func TestEveryStateAfterARefusedDelete(t *testing.T) {
	for _, tc := range []struct {
		name         string
		deletes      []int
		reads        []string
		cancelStatus int
		calls        string
		err          string // "" for a destroy that succeeds
		warns        bool
	}{
		{name: "deleting waits for it", deletes: []int{409}, reads: []string{"deleting"}, calls: "delete"},
		{name: "deleted waits for it", deletes: []int{409}, reads: []string{"deleted"}, calls: "delete"},
		{name: "active asks again", deletes: []int{409}, reads: []string{"active"}, calls: "delete delete"},
		{name: "failed asks again", deletes: []int{409}, reads: []string{"failed"}, calls: "delete delete"},
		{name: "creating cancels first", deletes: []int{409}, reads: []string{"creating", "failed"},
			calls: "delete cancel delete", warns: true},
		{name: "updating cancels first", deletes: []int{409}, reads: []string{"updating", "active"},
			calls: "delete cancel delete", warns: true},
		{name: "gone is done", deletes: []int{409}, calls: "delete"},
		{name: "an unrecognised state stops", deletes: []int{409}, reads: []string{"quiescing"},
			calls: "delete", err: `"quiescing"`},
		{name: "busy again after a cancel gives up", deletes: []int{409, 409}, reads: []string{"creating", "active", "updating"},
			calls: "delete cancel delete", err: "409", warns: true},
		{name: "three refusals give up", deletes: []int{409, 409, 409}, reads: []string{"active", "active", "active"},
			calls: "delete delete delete", err: "409"},
		{name: "another refusal is the answer", deletes: []int{422}, calls: "delete", err: "422"},
		{name: "a cancel waits through updating and stops at deleting", deletes: []int{409},
			reads: []string{"creating", "updating", "deleting"}, calls: "delete cancel delete", warns: true},
		{name: "a cancel that meets an unrecognised state stops", deletes: []int{409},
			reads: []string{"creating", "quiescing"}, calls: "delete cancel", err: "did not settle after its operation was cancelled", warns: true},
		{name: "a refused cancel on a cluster that settled asks again", deletes: []int{409}, cancelStatus: 409,
			reads: []string{"creating", "active"}, calls: "delete cancel delete"},
		{name: "a refused cancel on a cluster that is gone asks again", deletes: []int{409}, cancelStatus: 409,
			reads: []string{"creating"}, calls: "delete cancel delete"},
		{name: "a refused cancel on a busy cluster stops", deletes: []int{409}, cancelStatus: 409,
			reads: []string{"updating", "updating"}, calls: "delete cancel", err: "Could not cancel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The destroy reads the cluster before its first DELETE, to find a
			// delete already running. Here it finds the cluster `active`, so
			// every row starts from the DELETE it scripts.
			reads := append([]string{"active"}, tc.reads...)
			s := &scriptedCluster{deletes: tc.deletes, reads: reads, cancelStatus: tc.cancelStatus}
			server := s.serve(t)
			r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
			raw, sch := testValue(t, r, clusterValues(verRef(1), verRef(1), 2, "creating"))
			st := tfsdk.State{Raw: raw, Schema: sch.Schema}
			resp := resource.DeleteResponse{State: st}
			r.Delete(context.Background(), resource.DeleteRequest{State: st}, &resp)

			if got := strings.Join(s.calls, " "); got != tc.calls {
				t.Errorf("calls = %q, want %q", got, tc.calls)
			}
			switch {
			case tc.err == "" && resp.Diagnostics.HasError():
				t.Errorf("destroy failed: %v", resp.Diagnostics.Errors())
			case tc.err != "" && !strings.Contains(detailOf(resp.Diagnostics), tc.err):
				t.Errorf("diagnostics do not say %q:\n%s", tc.err, detailOf(resp.Diagnostics))
			}
			if got := len(resp.Diagnostics.Warnings()) > 0; got != tc.warns {
				t.Errorf("warned = %v, want %v: %v", got, tc.warns, resp.Diagnostics.Warnings())
			}
		})
	}
}

// A refused scale is retried only when it is the reason-less 409 an active
// cluster answers while its quota accounting catches up.
func TestEveryStateAfterARefusedScale(t *testing.T) {
	const bare = `{"error":{"code":409,"message":"settling"}}`
	for _, tc := range []struct {
		state, conflict string
		retried         bool
	}{
		{"active", bare, true},
		{"active", `{"error":{"code":409,"message":"over quota","reason":"QUOTA_EXCEEDED"}}`, false},
		{"active", `{"error":{"code":409,"message":"no","details":[{"reason":"POOL_BUSY"}]}}`, false},
		{"creating", bare, false},
		{"updating", bare, false},
		{"failed", bare, false},
		{"deleting", bare, false},
		{"deleted", bare, false},
		{"quiescing", bare, false},
	} {
		t.Run(tc.state+" "+tc.conflict, func(t *testing.T) {
			server, posts := scaleServerIn(t, tc.state, tc.conflict, 1)
			r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
			r.scaleOnce(context.Background(), clusterTarget{region: "test", project: "test"}, "c1",
				k0rdent.ClusterScaleRequest{})
			if got := posts.Load() > 1; got != tc.retried {
				t.Errorf("retried = %v (posts=%d), want %v", got, posts.Load(), tc.retried)
			}
		})
	}
}

// What a `cluster_type` edit plans as, through the replacement guard and
// ModifyPlan, for each kind of difference and the cluster's state.
func TestEveryClusterTypeChangeAtPlan(t *testing.T) {
	const origin = "https://api.k0rdent.test"
	other := "/v1/regions/test/compute/cluster-types/cpu"
	for _, tc := range []struct {
		name, stored, configured, state string
		replace, resolvedUnknown        bool
		refusal                         string
	}{
		{name: "the same reference", stored: verRef(1), configured: verRef(1), state: "active"},
		{name: "the same reference with an origin", stored: verRef(1), configured: origin + verRef(1), state: "active"},
		{name: "the head of the bound version", stored: verRef(1), configured: upgradeTypeRef, state: "active"},
		{name: "another version", stored: verRef(1), configured: verRef(4), state: "active", resolvedUnknown: true},
		{name: "another version while creating", stored: verRef(1), configured: verRef(4), state: "creating", resolvedUnknown: true},
		{name: "another version while failed", stored: verRef(1), configured: verRef(4), state: "failed",
			resolvedUnknown: true, refusal: "does not retry an upgrade from `failed`"},
		{name: "another ClusterType", stored: verRef(1), configured: other, state: "active", replace: true},
		{name: "another ClusterType while failed", stored: verRef(1), configured: other, state: "failed", replace: true},
		{name: "another host", stored: "https://api.k0rdent.test" + verRef(1),
			configured: "https://elsewhere.test" + verRef(1), state: "active", replace: true},
		{name: "a version pinned on a head reference", stored: upgradeTypeRef, configured: verRef(4), state: "active",
			resolvedUnknown: true},
		{name: "a head reference over an imported version", stored: verRef(1), configured: upgradeTypeRef, state: "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &clusterResource{}
			stateRaw, s := testValue(t, r, clusterValues(tc.stored, tc.stored, 2, tc.state))
			planRaw, _ := testValue(t, r, clusterValues(tc.configured, tc.stored, 2, tc.state))
			req := planmodifier.StringRequest{
				Path:        path.Root("cluster_type"),
				ConfigValue: types.StringValue(tc.configured),
				PlanValue:   types.StringValue(tc.configured),
				StateValue:  types.StringValue(tc.stored),
				State:       tfsdk.State{Raw: stateRaw, Schema: s.Schema},
				Plan:        tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
			}
			guard := planmodifier.StringResponse{PlanValue: req.PlanValue}
			clusterTypeVersionGuard().PlanModifyString(context.Background(), req, &guard)
			if guard.RequiresReplace != tc.replace {
				t.Errorf("replace = %v, want %v", guard.RequiresReplace, tc.replace)
			}

			// A replacement is permitted here: what is under test is the
			// classification, and the refusal of an unpermitted one is
			// TestTenantStateReplacementIsRefused.
			planned := clusterValues(tc.configured, tc.stored, 2, tc.state)
			if tc.replace {
				planned["allow_replace"] = tftypes.NewValue(tftypes.Bool, true)
			}
			plan := planWith(t, nil, clusterValues(tc.stored, tc.stored, 2, tc.state), planned)
			if got := resolvedIsUnknown(t, plan); got != tc.resolvedUnknown {
				t.Errorf("cluster_type_resolved unknown = %v, want %v", got, tc.resolvedUnknown)
			}
			got := detailOf(plan.Diagnostics)
			switch {
			case tc.refusal == "" && plan.Diagnostics.HasError():
				t.Errorf("plan refused: %s", got)
			case tc.refusal != "" && !strings.Contains(got, tc.refusal):
				t.Errorf("plan does not refuse with %q:\n%s", tc.refusal, got)
			}
		})
	}
}

// Which way an apply takes a `cluster_type` edit: upgrade calls for another
// version, nothing for another spelling of the bound one.
func TestEveryClusterTypeChangeAtApply(t *testing.T) {
	for _, tc := range []struct {
		name, configured string
		calls            string
	}{
		{"the bound version", verRef(1), "[]"},
		{"the bound version with an origin", "https://api.k0rdent.test" + verRef(1), "[]"},
		{"the head of the bound version", upgradeTypeRef, "[]"},
		{"another version", verRef(2), "[upgrade 2]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &upgradeAPI{bound: 1, state: "active", workers: 2}
			server := api.serve(t)
			r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
			stateRaw, s := testValue(t, r, clusterValues(verRef(1), verRef(1), 2, "active"))
			planRaw, _ := testValue(t, r, clusterValues(tc.configured, "", 2, ""))
			resp := resource.UpdateResponse{State: tfsdk.State{Raw: planRaw, Schema: s.Schema}}
			r.Update(context.Background(), resource.UpdateRequest{
				State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
				Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
			}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("update: %v", resp.Diagnostics.Errors())
			}
			if got := fmt.Sprint(api.calls); got != tc.calls {
				t.Errorf("calls = %s, want %s", got, tc.calls)
			}
		})
	}
}

// What a missing kubeconfig means for every state the cluster can be read in,
// for the data source, which does not wait for provisioning, and the
// ephemeral resource, which does.
func TestEveryStateWhileAKubeconfigIsMissing(t *testing.T) {
	const (
		waits       = "waits"
		provisional = "provisional"
		notYet      = "The cluster has no kubeconfig yet"
		beingGone   = "The cluster is being deleted"
	)
	for _, tc := range []struct {
		state           string
		plan, ephemeral string // an outcome, or text the error carries
	}{
		{"active", provisional, provisional},
		{"creating", notYet, waits},
		{"updating", notYet, waits},
		{"failed", "as failed", provisional},
		{"deleting", beingGone, beingGone},
		{"deleted", beingGone, beingGone},
		{"quiescing", `"quiescing"`, `"quiescing"`},
	} {
		for _, wait := range []bool{false, true} {
			want := tc.plan
			if wait {
				want = tc.ephemeral
			}
			t.Run(fmt.Sprintf("%s wait=%v", tc.state, wait), func(t *testing.T) {
				p := &kubeconfigPlatform{withheld: 1000, clusterState: tc.state}
				err := kubeconfigIsComing(context.Background(), p.meta(t, time.Minute),
					clusterTarget{region: "test", project: "test"}, "c1", wait)
				var pf provisionalFailure
				var kf kubeconfigFailure
				got := ""
				switch {
				case err == nil:
					got = waits
				case errors.As(err, &pf):
					got = provisional
				case errors.As(err, &kf):
					got = kf.summary
				default:
					got = err.Error()
				}
				if !strings.Contains(got, want) {
					t.Errorf("got %q, want %q", got, want)
				}
			})
		}
	}
}

// The values the guard sees that a plan with known references never sends:
// an unknown configuration and a null stored value. Either one replacing
// would destroy a running cluster.
func TestTheClusterTypeGuardOnUnknownAndNullValues(t *testing.T) {
	for _, tc := range []struct {
		name               string
		configured, stored types.String
		replace            bool
	}{
		{"an unknown configuration", types.StringUnknown(), types.StringValue(verRef(1)), false},
		{"a null stored value", types.StringValue("/v1/regions/test/compute/cluster-types/cpu"), types.StringNull(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &clusterResource{}
			raw, s := testValue(t, r, clusterValues(verRef(1), verRef(1), 2, "active"))
			req := planmodifier.StringRequest{
				Path:        path.Root("cluster_type"),
				ConfigValue: tc.configured,
				PlanValue:   tc.configured,
				StateValue:  tc.stored,
				State:       tfsdk.State{Raw: raw, Schema: s.Schema},
				Plan:        tfsdk.Plan{Raw: raw, Schema: s.Schema},
			}
			resp := planmodifier.StringResponse{PlanValue: req.PlanValue}
			clusterTypeVersionGuard().PlanModifyString(context.Background(), req, &resp)
			if resp.RequiresReplace != tc.replace || resp.Diagnostics.HasError() {
				t.Errorf("replace = %v %v, want %v", resp.RequiresReplace, resp.Diagnostics, tc.replace)
			}
		})
	}
}

// upgradeTo runs the upgrade loop straight, for a plan whose cluster_type is
// want, against api.
func upgradeTo(t *testing.T, api *upgradeAPI, want string) resource.UpdateResponse {
	t.Helper()
	server := api.serve(t)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	planRaw, s := testValue(t, r, clusterValues(want, "", api.workers, ""))
	var plan clusterModel
	if d := (tfsdk.Plan{Raw: planRaw, Schema: s.Schema}).Get(context.Background(), &plan); d.HasError() {
		t.Fatalf("plan: %v", d)
	}
	// Bounded, so that a loop missing an exit fails rather than hangs.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp := resource.UpdateResponse{State: tfsdk.State{Raw: planRaw, Schema: s.Schema}}
	resp.Diagnostics = r.upgrade(ctx, clusterTarget{region: "test", project: "test"}, "c1", plan, &resp.State)
	return resp
}

// The three ways out of the upgrade loop that end it on what a pass read.
func TestTheUpgradeLoopStopsOnWhatAPassReads(t *testing.T) {
	t.Run("the cluster disappeared", func(t *testing.T) {
		// The first read is the settle wait before the loop, the second the
		// loop's own.
		api := &upgradeAPI{bound: 1, state: "active", workers: 2, goneAtRead: 2}
		resp := upgradeTo(t, api, verRef(2))
		if got := detailOf(resp.Diagnostics); !strings.Contains(got, "disappeared during the upgrade") {
			t.Errorf("diagnostics:\n%s", got)
		}
		if len(api.calls) != 0 || api.lists != 0 {
			t.Errorf("calls = %v, version lists = %d, want none", api.calls, api.lists)
		}
	})
	t.Run("a step settled without moving", func(t *testing.T) {
		api := &upgradeAPI{bound: 1, state: "active", workers: 2, stuck: true}
		resp := upgradeTo(t, api, verRef(2))
		if got := detailOf(resp.Diagnostics); !strings.Contains(got, "settled without changing the version") {
			t.Errorf("diagnostics:\n%s", got)
		}
		if got := fmt.Sprint(api.calls); got != "[upgrade 2]" {
			t.Errorf("calls = %s, want one upgrade and no second ask", got)
		}
		// Checked before the replan, so the second pass reads no versions.
		if api.lists != 1 {
			t.Errorf("version lists = %d, want 1", api.lists)
		}
	})
	t.Run("the bound version under another origin", func(t *testing.T) {
		api := &upgradeAPI{bound: 1, state: "active", workers: 2, origin: "https://api.k0rdent.test"}
		resp := upgradeTo(t, api, "https://elsewhere.test"+verRef(1))
		if got := detailOf(resp.Diagnostics); !strings.Contains(got, "different origin than the platform's") {
			t.Errorf("diagnostics:\n%s", got)
		}
		if len(api.calls) != 0 || api.lists != 1 {
			t.Errorf("calls = %v, version lists = %d, want no calls after one list", api.calls, api.lists)
		}
	})
}
