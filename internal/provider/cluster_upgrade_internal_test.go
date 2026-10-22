package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

const upgradeTypeRef = "/v1/regions/test/compute/cluster-types/gpu"

func ver(n int) string    { return fmt.Sprintf("aaaaaaaa-0000-4000-8000-%012d", n) }
func verRef(n int) string { return upgradeTypeRef + "/versions/" + ver(n) }

// upgradeAPI is a cluster with a version history: v1, v2 available, v3 draft,
// v4 and v5 available. The workers pool allows 1 to 8 nodes unless bounds says
// otherwise for a version. Like the platform, it checks a scale against the
// bound version's bounds and an upgrade against the target's.
type upgradeAPI struct {
	mu       sync.Mutex
	bound    int
	target   int
	state    string
	reads    int // reads while updating; the cluster settles on the second
	workers  int
	bounds   map[int][2]int  // min and max of the workers pool at a version
	bmaas    map[int]bool    // versions with no Kubernetes configuration
	failInto int             // an upgrade into this version settles failed
	refuse   map[int]int     // status to answer an upgrade into this version with
	bare404  *notFoundAnswer // when set, a refused 404 is answered with this instead
	calls    []string        // "upgrade N" and "scale N", in order

	goneAtRead int    // the cluster read with this number, from 1, and every later one answer 404
	stuck      bool   // an accepted upgrade settles `active` without moving the version
	origin     string // prefixed to the clusterType the cluster read reports
	clusterGET int    // cluster reads so far
	lists      int    // version list reads so far

	events eventLog // the runtime event log, served a page at a time

	versionStatus map[int]int // status to answer a read of this version with
	versionGETs   int         // version reads so far
	// versionFailsOnceUpgrading answers every version read with a 500 from
	// the first accepted upgrade on.
	versionFailsOnceUpgrading bool
	upgrading                 bool

	// ceiling is the storage ceiling in GiB the cluster declares, 0 for
	// none. busyRaises is how many storage raises are refused with the
	// reason-less 409 a cluster that isn't `active` answers, each one
	// leaving the cluster `updating` for the next read to settle.
	ceiling    int
	busyRaises int

	// busyUpgrades is how many upgrades find that another run started an
	// operation on the cluster just before them: each is refused with the
	// 409 a cluster that isn't `active` answers, and leaves the cluster
	// `updating` for the reads to settle. With othersUpgrade, that operation
	// is the same upgrade, to the version asked for. With busyForever it
	// never settles.
	busyUpgrades  int
	othersUpgrade bool
	busyForever   bool
}

// k8sAt is the Kubernetes version that version n carries.
func k8sAt(n int) string { return fmt.Sprintf("v1.3%d.0+k0s.0", n) }

var upgradeStatuses = map[int]string{1: "available", 2: "available", 3: "draft", 4: "available", 5: "available"}

func (a *upgradeAPI) boundsAt(n int) (int, int) {
	if b, ok := a.bounds[n]; ok {
		return b[0], b[1]
	}
	return 1, 8
}

func (a *upgradeAPI) fits(n, count int) bool {
	lo, hi := a.boundsAt(n)
	return count >= lo && count <= hi
}

func refuseWith(w http.ResponseWriter, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":"refused","requestId":"req-upgrade-1"}}`, status)
}

func versionIndex(id string) int {
	for i := 1; i <= 5; i++ {
		if id == ver(i) {
			return i
		}
	}
	return 0
}

func (a *upgradeAPI) serve(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/regions/{r}/compute/cluster-types/gpu/versions", func(w http.ResponseWriter, _ *http.Request) {
		var versions []any
		for n := 1; n <= 5; n++ {
			versions = append(versions, map[string]any{
				"versionId": ver(n), "version": n, "status": upgradeStatuses[n], "displayName": fmt.Sprint("v", n),
			})
		}
		a.mu.Lock()
		a.lists++
		a.mu.Unlock()
		writeJSON(w, map[string]any{"versions": versions})
	})
	mux.HandleFunc("GET /v1/regions/{r}/compute/cluster-types/gpu/versions/{v}", func(w http.ResponseWriter, r *http.Request) {
		n := versionIndex(r.PathValue("v"))
		a.mu.Lock()
		a.versionGETs++
		failing := a.versionFailsOnceUpgrading && a.upgrading
		a.mu.Unlock()
		if failing {
			writeTestError(t, w, http.StatusInternalServerError, "the cluster type store is unavailable")
			return
		}
		if status, ok := a.versionStatus[n]; ok {
			writeTestError(t, w, status, "refused")
			return
		}
		lo, hi := a.boundsAt(n)
		body := map[string]any{"id": "gpu", "version": n, "nodePools": []any{
			map[string]any{"id": "workers", "role": "worker", "nodeCountMin": lo, "nodeCountMax": hi},
		}}
		if !a.bmaas[n] {
			body["k8s"] = map[string]any{"clusterTemplateName": "t", "clusterTemplateVersion": "v", "k0sVersion": k8sAt(n)}
		}
		writeJSON(w, body)
	})
	mux.HandleFunc("GET /v1/regions/{r}/projects/{p}/compute/clusters/{id}", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.clusterGET++; a.goneAtRead != 0 && a.clusterGET >= a.goneAtRead {
			writeTestError(t, w, http.StatusNotFound, "not found")
			return
		}
		if a.state == "updating" && !a.busyForever {
			if a.reads++; a.reads >= 2 {
				a.state, a.reads = "active", 0
				if a.target != 0 {
					switch {
					case a.target == a.failInto:
						a.state = "failed"
					case a.stuck:
						// Settles where it was.
					default:
						a.bound = a.target
					}
					a.target = 0
				}
			}
		}
		body := map[string]any{
			"id": "c1", "clusterType": a.origin + verRef(a.bound), "state": a.state,
			"nodePools": []any{map[string]any{"id": "workers", "nodeCount": a.workers}},
		}
		if a.state == "failed" {
			body["stateMessage"] = "the upgrade did not reconcile"
		}
		if a.ceiling > 0 {
			body["storage"] = map[string]any{"ceilingGiB": a.ceiling}
		}
		writeJSON(w, body)
	})
	mux.HandleFunc("PATCH /v1/regions/{r}/projects/{p}/compute/clusters/{id}", func(w http.ResponseWriter, r *http.Request) {
		var req k0rdent.ClusterUpdateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("update body: %v", err)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if req.Storage == nil {
			a.calls = append(a.calls, "update")
			writeJSON(w, map[string]any{"id": "c1", "clusterType": verRef(a.bound), "state": a.state})
			return
		}
		n := int(req.Storage.CeilingGiB)
		a.calls = append(a.calls, fmt.Sprint("storage ", n))
		switch {
		case req.SecurityGroups != nil:
			refuseWith(w, http.StatusUnprocessableEntity)
		case n < a.ceiling:
			refuseWith(w, http.StatusUnprocessableEntity)
		case a.busyRaises > 0 || a.state != "active":
			// The contract's reason-less 409 for a raise on a cluster that
			// isn't active.
			if a.busyRaises > 0 {
				a.busyRaises--
				a.state = "updating"
			}
			refuseWith(w, http.StatusConflict)
		default:
			a.ceiling = n
			writeJSON(w, map[string]any{"id": "c1", "clusterType": verRef(a.bound), "state": a.state,
				"storage": map[string]any{"ceilingGiB": n}})
		}
	})
	mux.HandleFunc("POST /v1/regions/{r}/projects/{p}/compute/clusters/{id}/upgrade", func(w http.ResponseWriter, r *http.Request) {
		var req k0rdent.UpgradeClusterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ClusterTypeVersionId == nil {
			t.Errorf("an upgrade must name its version: %v", err)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		n := versionIndex(req.ClusterTypeVersionId.String())
		a.calls = append(a.calls, fmt.Sprint("upgrade ", n))
		status, refused := a.refuse[n]
		if a.busyUpgrades > 0 && a.state == "active" {
			a.busyUpgrades--
			a.state = "updating"
			if a.othersUpgrade {
				a.target = n
			}
		}
		switch {
		case refused:
		case a.state != "active":
			status = http.StatusConflict
		case !a.fits(n, a.workers) || a.bmaas[n]:
			status = http.StatusUnprocessableEntity
		default:
			a.state, a.target, a.upgrading = "updating", n, true
			writeJSON(w, map[string]any{"id": "c1", "clusterType": verRef(a.bound), "state": a.state})
			return
		}
		if status == http.StatusNotFound && a.bare404 != nil {
			a.bare404.write(t, w)
			return
		}
		refuseWith(w, status)
	})
	mux.HandleFunc("POST /v1/regions/{r}/projects/{p}/compute/clusters/{id}/scale", func(w http.ResponseWriter, r *http.Request) {
		var req k0rdent.ClusterScaleRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.NodePools) != 1 {
			t.Errorf("scale body: %v", err)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		n := req.NodePools[0].NodeCount
		a.calls = append(a.calls, fmt.Sprint("scale ", n))
		switch {
		case a.state != "active":
			refuseWith(w, http.StatusConflict)
		case !a.fits(a.bound, n):
			refuseWith(w, http.StatusUnprocessableEntity)
		default:
			a.workers, a.state = n, "updating"
			writeJSON(w, map[string]any{"id": "c1", "clusterType": verRef(a.bound), "state": a.state})
		}
	})
	mux.HandleFunc("GET /v1/regions/{r}/projects/{p}/compute/clusters/{id}/runtime/events", a.events.serve(t))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func workersValue(n int) tftypes.Value {
	obj := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"node_count": tftypes.Number}}
	return tftypes.NewValue(tftypes.Map{ElementType: obj}, map[string]tftypes.Value{
		"workers": tftypes.NewValue(obj, map[string]tftypes.Value{"node_count": tftypes.NewValue(tftypes.Number, n)}),
	})
}

func clusterValues(clusterType, resolved string, workers int, state string) map[string]tftypes.Value {
	v := map[string]tftypes.Value{
		"id": str("c1"), "region": str("test"), "project": str("test"),
		"cluster_type": str(clusterType), "node_pool": workersValue(workers),
	}
	if resolved != "" {
		v["cluster_type_resolved"] = str(resolved)
	}
	if state != "" {
		v["state"] = str(state)
	}
	return v
}

// runUpgrade applies a plan that moves the cluster from its bound version to
// target, and optionally to another worker count.
func runUpgrade(t *testing.T, api *upgradeAPI, target, workers int) resource.UpdateResponse {
	t.Helper()
	server := api.serve(t)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	stateRaw, s := testValue(t, r, clusterValues(verRef(api.bound), verRef(api.bound), api.workers, api.state))
	planRaw, _ := testValue(t, r, clusterValues(verRef(target), "", workers, ""))
	resp := resource.UpdateResponse{State: tfsdk.State{Raw: planRaw, Schema: s.Schema}}
	r.Update(context.Background(), resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}, &resp)
	return resp
}

func stateAttr(t *testing.T, s tfsdk.State, name string) string {
	t.Helper()
	var v types.String
	if d := s.GetAttribute(context.Background(), path.Root(name), &v); d.HasError() {
		t.Fatalf("read %s: %v", name, d)
	}
	return v.ValueString()
}

func TestUpgradePath(t *testing.T) {
	var versions []k0rdent.ClusterTypeVersion
	for n := 1; n <= 5; n++ {
		id := mustUUID(t, ver(n))
		seq := int32(n)
		versions = append(versions, k0rdent.ClusterTypeVersion{
			VersionId: &id, Version: &seq, Status: k0rdent.ClusterTypeVersionStatus(upgradeStatuses[n]),
		})
	}
	for _, tc := range []struct {
		name     string
		from, to int
		want     []int
		refusal  string
	}{
		{"one step", 1, 2, []int{2}, ""},
		{"over a draft", 1, 4, []int{2, 4}, ""},
		{"already there", 2, 2, nil, ""},
		{"backwards", 4, 2, nil, "forward-only"},
		{"to a draft", 2, 3, nil, "draft"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			steps, refusal := upgradePath(versions, ver(tc.from), ver(tc.to))
			var got []int
			for _, s := range steps {
				got = append(got, int(*s.Version))
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) || !strings.Contains(refusal, tc.refusal) || (tc.refusal == "") != (refusal == "") {
				t.Errorf("steps %v refusal %q, want %v %q", got, refusal, tc.want, tc.refusal)
			}
		})
	}
	if _, refusal := upgradePath(versions, ver(1), "99999999-0000-4000-8000-000000000000"); !strings.Contains(refusal, "not a version") {
		t.Errorf("an unknown version: %q", refusal)
	}
}

// Each call names its version, so a version published mid-chain cannot
// redirect it, and the draft in between is skipped rather than stopped at.
func TestUpdateUpgradesOneVersionPerCall(t *testing.T) {
	api := &upgradeAPI{bound: 1, state: "active", workers: 2}
	resp := runUpgrade(t, api, 4, 2)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics.Errors())
	}
	if got := fmt.Sprint(api.calls); got != "[upgrade 2 upgrade 4]" {
		t.Errorf("calls = %s", got)
	}
	if got := stateAttr(t, resp.State, "cluster_type_resolved"); got != verRef(4) {
		t.Errorf("cluster_type_resolved = %s", got)
	}
}

// A step that fails stops the chain and leaves state at the version reached,
// so the next plan proposes the rest rather than pretending it landed.
func TestAFailedStepRecordsTheVersionReached(t *testing.T) {
	api := &upgradeAPI{bound: 2, state: "active", workers: 2, failInto: 5}
	resp := runUpgrade(t, api, 5, 2)
	if got := fmt.Sprint(api.calls); got != "[upgrade 4 upgrade 5]" {
		t.Errorf("calls = %s", got)
	}
	if !resp.Diagnostics.HasError() {
		t.Fatal("a failed upgrade reported success")
	}
	e := resp.Diagnostics.Errors()[0]
	for _, want := range []string{"failed at version 4", "does not retry", "Nothing was deleted", "did not reconcile"} {
		if !strings.Contains(e.Summary()+e.Detail(), want) {
			t.Errorf("diagnostic does not say %q:\n%s\n%s", want, e.Summary(), e.Detail())
		}
	}
	for _, attr := range []string{"cluster_type", "cluster_type_resolved"} {
		if got := stateAttr(t, resp.State, attr); got != verRef(4) {
			t.Errorf("%s = %s, want the version reached", attr, got)
		}
	}
	if got := stateAttr(t, resp.State, "state"); got != "failed" {
		t.Errorf("state = %s", got)
	}
}

func TestUpgradeRefusalsSayWhatToDo(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   *regexp.Regexp
	}{
		{"conflict", http.StatusConflict, regexp.MustCompile("only from `active`")},
		{"refused", http.StatusUnprocessableEntity, regexp.MustCompile("next available version")},
		{"not started", http.StatusGatewayTimeout, regexp.MustCompile("(?s)did not start.*Nothing was deleted")},
		{"withdrawn", http.StatusNotFound, regexp.MustCompile("does not find version 2")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &upgradeAPI{bound: 1, state: "active", workers: 2, refuse: map[int]int{2: tc.status}}
			resp := runUpgrade(t, api, 2, 2)
			if !resp.Diagnostics.HasError() {
				t.Fatal("a refused upgrade reported success")
			}
			e := resp.Diagnostics.Errors()[0]
			text := e.Summary() + "\n" + e.Detail()
			if !tc.want.MatchString(text) || !strings.Contains(text, "req-upgrade-1") {
				t.Errorf("diagnostic:\n%s", text)
			}
			if got := stateAttr(t, resp.State, "cluster_type"); got != verRef(1) {
				t.Errorf("cluster_type = %s, want the version still bound", got)
			}
		})
	}
}

// Only the service can say a version is missing. A 404 that never reached it
// says nothing about the version, so the diagnostic names what answered.
func TestAnUpgrade404FromOutsideTheServiceNamesWhatAnswered(t *testing.T) {
	for _, a := range notFoundAnswers {
		if a.gone {
			continue
		}
		t.Run(a.name, func(t *testing.T) {
			api := &upgradeAPI{bound: 1, state: "active", workers: 2,
				refuse: map[int]int{2: http.StatusNotFound}, bare404: &a}
			resp := runUpgrade(t, api, 2, 2)
			if !resp.Diagnostics.HasError() {
				t.Fatal("a refused upgrade reported success")
			}
			e := resp.Diagnostics.Errors()[0]
			text := e.Summary() + "\n" + e.Detail()
			if strings.Contains(text, "does not find version") {
				t.Errorf("a 404 from outside the service was read as the version missing:\n%s", text)
			}
			if !strings.Contains(text, a.answeredBy) {
				t.Errorf("the diagnostic does not say what answered (want %q):\n%s", a.answeredBy, text)
			}
			if got := stateAttr(t, resp.State, "cluster_type"); got != verRef(1) {
				t.Errorf("cluster_type = %s, want the version still bound", got)
			}
		})
	}
}

// A refusal part way through a chain must not claim nothing changed: the
// earlier step did. It names the version the cluster is at.
func TestARefusalMidChainNamesTheVersionReached(t *testing.T) {
	api := &upgradeAPI{bound: 1, state: "active", workers: 2, refuse: map[int]int{4: http.StatusConflict}}
	resp := runUpgrade(t, api, 4, 2)
	// A 409 from a cluster that reads `active` is sent once more, since it may
	// have settled between the refusal and the read. See afterRefusedUpgrade.
	if got := fmt.Sprint(api.calls); got != "[upgrade 2 upgrade 4 upgrade 4]" {
		t.Errorf("calls = %s", got)
	}
	if !resp.Diagnostics.HasError() {
		t.Fatal("a refused upgrade reported success")
	}
	e := resp.Diagnostics.Errors()[0]
	text := e.Summary() + "\n" + e.Detail()
	if strings.Contains(text, "Nothing was changed") || !strings.Contains(text, "at version 2") {
		t.Errorf("diagnostic:\n%s", text)
	}
	if got := stateAttr(t, resp.State, "cluster_type_resolved"); got != verRef(2) {
		t.Errorf("cluster_type_resolved = %s", got)
	}
}

// A scale is checked against the bound version and an upgrade against the
// target, on the counts already settled. So each pool change goes before the
// upgrade when its new count fits the bound version, and after it otherwise.
// All four ways a version can move a bound.
func TestEachPoolChangeGoesWhereTheBoundsAdmitIt(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		from, to, have, want int
		bounds               map[int][2]int
		calls                string
	}{
		{"max lowered", 1, 2, 6, 3, map[int][2]int{2: {1, 4}}, "[scale 3 upgrade 2]"},
		{"max raised", 2, 4, 4, 8, map[int][2]int{2: {1, 4}}, "[upgrade 4 scale 8]"},
		{"min raised", 1, 2, 2, 3, map[int][2]int{2: {3, 8}}, "[scale 3 upgrade 2]"},
		{"min lowered", 1, 2, 2, 1, map[int][2]int{1: {2, 8}}, "[upgrade 2 scale 1]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &upgradeAPI{bound: tc.from, state: "active", workers: tc.have, bounds: tc.bounds}
			if resp := runUpgrade(t, api, tc.to, tc.want); resp.Diagnostics.HasError() {
				t.Fatalf("update: %v", resp.Diagnostics.Errors())
			}
			if got := fmt.Sprint(api.calls); got != tc.calls {
				t.Errorf("calls = %s, want %s", got, tc.calls)
			}
		})
	}
}

// A count no order admits is refused before anything is sent, naming the
// pool and each version's bounds.
func TestACountNoOrderAdmitsIsRefusedBeforeAnyCall(t *testing.T) {
	api := &upgradeAPI{bound: 1, state: "active", workers: 6, bounds: map[int][2]int{2: {1, 4}}}
	resp := runUpgrade(t, api, 2, 6)
	if len(api.calls) != 0 {
		t.Errorf("calls = %v, want none", api.calls)
	}
	if !resp.Diagnostics.HasError() {
		t.Fatal("no refusal")
	}
	e := resp.Diagnostics.Errors()[0]
	if !regexp.MustCompile(`(?s)"workers".*version 2 allows 1 to 4`).MatchString(e.Detail()) {
		t.Errorf("diagnostic:\n%s\n%s", e.Summary(), e.Detail())
	}
}

// An apply that starts while the cluster is still settling waits for it
// rather than sending an upgrade the platform answers 409.
func TestAnUpgradeWaitsForABusyClusterFirst(t *testing.T) {
	api := &upgradeAPI{bound: 1, state: "updating", workers: 2}
	if resp := runUpgrade(t, api, 2, 2); resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics.Errors())
	}
	if got := fmt.Sprint(api.calls); got != "[upgrade 2]" {
		t.Errorf("calls = %s", got)
	}
}

// A pool missing from prior state is compared with the count the platform
// reports, not assumed to be growing: a request that mixes an increase with a
// decrease is refused.
func TestScaleComparesWithThePlatformsCount(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			var req k0rdent.ClusterScaleRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("scale body: %v", err)
			}
			mu.Lock()
			sent = append(sent, fmt.Sprint(req.NodePools))
			mu.Unlock()
		}
		writeJSON(w, map[string]any{"id": "c1", "clusterType": upgradeTypeRef, "state": "active", "nodePools": []any{
			map[string]any{"id": "workers", "nodeCount": 2}, map[string]any{"id": "gpus", "nodeCount": 1},
		}})
	}))
	t.Cleanup(server.Close)

	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	obj := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"node_count": tftypes.Number}}
	pools := func(counts map[string]int) tftypes.Value {
		m := map[string]tftypes.Value{}
		for id, n := range counts {
			m[id] = tftypes.NewValue(obj, map[string]tftypes.Value{"node_count": tftypes.NewValue(tftypes.Number, n)})
		}
		return tftypes.NewValue(tftypes.Map{ElementType: obj}, m)
	}
	base := func(p tftypes.Value) map[string]tftypes.Value {
		return map[string]tftypes.Value{
			"id": str("c1"), "region": str("test"), "project": str("test"),
			"cluster_type": str(upgradeTypeRef), "cluster_type_resolved": str(upgradeTypeRef), "node_pool": p,
		}
	}
	stateRaw, s := testValue(t, r, base(pools(map[string]int{"gpus": 1})))
	planRaw, _ := testValue(t, r, base(pools(map[string]int{"gpus": 3, "workers": 1})))
	resp := resource.UpdateResponse{State: tfsdk.State{Raw: planRaw, Schema: s.Schema}}
	r.Update(context.Background(), resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics.Errors())
	}
	if got := fmt.Sprint(sent); got != "[[{gpus 3}] [{workers 1}]]" {
		t.Errorf("scale requests = %s, want the increase and the decrease apart", got)
	}
}

// planUpgrade runs ModifyPlan for a cluster bound to bound with two workers,
// configured at target, and returns the response.
func planUpgrade(t *testing.T, meta *Meta, bound, target, state string) resource.ModifyPlanResponse {
	t.Helper()
	return planWith(t, meta, clusterValues(bound, bound, 2, state), clusterValues(target, bound, 2, state))
}

func planWith(t *testing.T, meta *Meta, prior, planned map[string]tftypes.Value) resource.ModifyPlanResponse {
	t.Helper()
	r := guardedModifier(t, &clusterResource{meta: meta}, clusterTenantState)
	stateRaw, s := testValue(t, r, prior)
	planRaw, _ := testValue(t, r, planned)
	resp := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: planRaw, Schema: s.Schema},
		Plan:   tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
		State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
	}, &resp)
	return resp
}

func resolvedIsUnknown(t *testing.T, resp resource.ModifyPlanResponse) bool {
	t.Helper()
	var v types.String
	if d := resp.Plan.GetAttribute(context.Background(), path.Root("cluster_type_resolved"), &v); d.HasError() {
		t.Fatalf("read plan: %v", d)
	}
	return v.IsUnknown()
}

func TestPlanSaysHowManyStepsAnUpgradeTakes(t *testing.T) {
	server := (&upgradeAPI{bound: 1, state: "active"}).serve(t)
	resp := planUpgrade(t, testClusterMeta(t, server.URL, time.Minute), verRef(1), verRef(4), "active")
	if resp.Diagnostics.HasError() || resp.RequiresReplace.Contains(path.Root("cluster_type")) {
		t.Fatalf("plan: %v replace=%v", resp.Diagnostics, resp.RequiresReplace)
	}
	if !hasWarning(resp.Diagnostics, "2 steps") {
		t.Errorf("no step count in %v", resp.Diagnostics)
	}
	if !resolvedIsUnknown(t, resp) {
		t.Error("cluster_type_resolved is planned known, so the apply would be inconsistent")
	}
}

func TestPlanRefusals(t *testing.T) {
	server := (&upgradeAPI{bound: 4, state: "active"}).serve(t)
	meta := testClusterMeta(t, server.URL, time.Minute)
	for _, tc := range []struct {
		name, bound, target, state, want string
	}{
		{"an earlier version", verRef(4), verRef(2), "active", "forward-only"},
		{"a draft", verRef(2), verRef(3), "active", "draft"},
		{"a version the ClusterType does not have", verRef(2), upgradeTypeRef + "/versions/99999999-0000-4000-8000-000000000000", "active", "not a version"},
		{"a failed cluster", verRef(2), verRef(4), "failed", "does not retry"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := planUpgrade(t, meta, tc.bound, tc.target, tc.state)
			if !resp.Diagnostics.HasError() {
				t.Fatal("the plan was not refused")
			}
			e := resp.Diagnostics.Errors()[0]
			if !regexp.MustCompile(`different version of its\s+ClusterType`).MatchString(e.Summary()) ||
				!strings.Contains(e.Detail(), tc.want) {
				t.Errorf("diagnostic:\n%s\n%s", e.Summary(), e.Detail())
			}
			// `terraform destroy` refreshes with a normal plan first, so this
			// refusal also stops a destroy. It has to name one that works.
			if !strings.Contains(e.Detail(), "terraform destroy -refresh=false") ||
				!strings.Contains(e.Detail(), tc.bound) || strings.Contains(e.Detail(), "-target") {
				t.Errorf("no working way to destroy:\n%s", e.Detail())
			}
		})
	}
}

// With no client to ask, the plan cannot know whether the version is later,
// so it plans the update and leaves the check to the apply.
func TestPlanDefersWithoutAClient(t *testing.T) {
	resp := planUpgrade(t, nil, verRef(1), verRef(4), "active")
	if resp.Diagnostics.HasError() {
		t.Fatalf("plan: %v", resp.Diagnostics)
	}
	if !resolvedIsUnknown(t, resp) {
		t.Error("cluster_type_resolved is planned known")
	}
}

// A cluster_type that is unknown at plan may name another version, so what
// the platform will report has to be unknown too. Planned known, it becomes
// unknown at apply and Terraform rejects the final plan as inconsistent.
func TestAnUnknownClusterTypePlansTheBindingUnknown(t *testing.T) {
	planned := clusterValues(verRef(1), verRef(1), 2, "active")
	planned["cluster_type"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	resp := planWith(t, nil, clusterValues(verRef(1), verRef(1), 2, "active"), planned)
	if resp.Diagnostics.HasError() {
		t.Fatalf("plan: %v", resp.Diagnostics)
	}
	if !resolvedIsUnknown(t, resp) {
		t.Error("cluster_type_resolved is planned known")
	}
}

// Every step on the route is checked at plan, so a chain that would stop part
// way is refused before any of it runs.
func TestPlanRefusesARouteSomeStepCannotTake(t *testing.T) {
	for _, tc := range []struct {
		name string
		api  *upgradeAPI
		want string
	}{
		{"a later step's bounds", &upgradeAPI{bound: 1, state: "active", workers: 2, bounds: map[int][2]int{4: {1, 1}}},
			"version 4 allows 1 to 1"},
		{"a BMaaS-only step", &upgradeAPI{bound: 1, state: "active", workers: 2, bmaas: map[int]bool{4: true}},
			"no Kubernetes configuration"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := tc.api.serve(t)
			resp := planWith(t, testClusterMeta(t, server.URL, time.Minute),
				clusterValues(verRef(1), verRef(1), 2, "active"), clusterValues(verRef(5), verRef(1), 2, "active"))
			if !resp.Diagnostics.HasError() {
				t.Fatal("the plan was not refused")
			}
			if e := resp.Diagnostics.Errors()[0]; !strings.Contains(e.Detail(), tc.want) {
				t.Errorf("diagnostic:\n%s\n%s", e.Summary(), e.Detail())
			}
		})
	}

	// One worker fits every step, so the same route plans.
	server := (&upgradeAPI{bound: 1, state: "active", workers: 2, bounds: map[int][2]int{4: {1, 1}}}).serve(t)
	resp := planWith(t, testClusterMeta(t, server.URL, time.Minute),
		clusterValues(verRef(1), verRef(1), 2, "active"), clusterValues(verRef(4), verRef(1), 1, "active"))
	if resp.Diagnostics.HasError() {
		t.Errorf("plan: %v", resp.Diagnostics)
	}
}

func hasWarning(diags diag.Diagnostics, text string) bool {
	for _, d := range diags.Warnings() {
		if strings.Contains(d.Summary()+d.Detail(), text) {
			return true
		}
	}
	return false
}

func mustUUID(t *testing.T, s string) openapi_types.UUID {
	t.Helper()
	id, err := uuid.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return id
}

// runUpgradeWithin is runUpgrade bounded by timeout.
func runUpgradeWithin(t *testing.T, api *upgradeAPI, target, workers int, timeout time.Duration) resource.UpdateResponse {
	t.Helper()
	server := api.serve(t)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, timeout)}
	stateRaw, s := testValue(t, r, clusterValues(verRef(api.bound), verRef(api.bound), api.workers, api.state))
	planRaw, _ := testValue(t, r, clusterValues(verRef(target), "", workers, ""))
	resp := resource.UpdateResponse{State: tfsdk.State{Raw: planRaw, Schema: s.Schema}}
	r.Update(context.Background(), resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}, &resp)
	return resp
}

// Another run starts an operation on the cluster between this upgrade's
// settle and its step, and the platform refuses the step with 409. The
// upgrade waits for that operation to finish and takes the step, however
// many times it happens, within the update timeout.
func TestAnUpgradeWaitsOutAnotherRunsOperation(t *testing.T) {
	for _, tc := range []struct {
		busy  int
		calls string
	}{
		{1, "[upgrade 2 upgrade 2]"},
		{3, "[upgrade 2 upgrade 2 upgrade 2 upgrade 2]"},
	} {
		api := &upgradeAPI{bound: 1, state: "active", workers: 2, busyUpgrades: tc.busy}
		resp := runUpgrade(t, api, 2, 2)
		if resp.Diagnostics.HasError() {
			t.Fatalf("%d busy: upgrade: %v", tc.busy, resp.Diagnostics.Errors())
		}
		if got := fmt.Sprint(api.calls); got != tc.calls {
			t.Errorf("%d busy: calls = %s, want %s", tc.busy, got, tc.calls)
		}
		if got := stateAttr(t, resp.State, "cluster_type_resolved"); got != verRef(2) {
			t.Errorf("%d busy: cluster_type_resolved = %s, want version 2", tc.busy, got)
		}
	}
}

// The operation another run started was the same upgrade. Once it lands the
// cluster is already where the plan asked, so the step isn't sent again.
func TestAnUpgradeAnotherRunMadeIsNotMadeTwice(t *testing.T) {
	api := &upgradeAPI{bound: 1, state: "active", workers: 2, busyUpgrades: 1, othersUpgrade: true}
	resp := runUpgrade(t, api, 2, 2)
	if resp.Diagnostics.HasError() {
		t.Fatalf("upgrade: %v", resp.Diagnostics.Errors())
	}
	if got := fmt.Sprint(api.calls); got != "[upgrade 2]" {
		t.Errorf("calls = %s, want only the refused step", got)
	}
	if got := stateAttr(t, resp.State, "cluster_type_resolved"); got != verRef(2) {
		t.Errorf("cluster_type_resolved = %s, want version 2", got)
	}
}

// An operation that outlasts the update timeout is reported with the
// platform's refusal, what the upgrade waited for, and that this step changed
// nothing.
func TestAnUpgradeReportsAnOperationThatOutlastsIt(t *testing.T) {
	api := &upgradeAPI{bound: 1, state: "active", workers: 2, busyUpgrades: 1, busyForever: true}
	resp := runUpgradeWithin(t, api, 2, 2, 50*time.Millisecond)
	if !resp.Diagnostics.HasError() {
		t.Fatal("an upgrade that never got through reported success")
	}
	if got := fmt.Sprint(api.calls); got != "[upgrade 2]" {
		t.Errorf("calls = %s, want the one refused step", got)
	}
	e := resp.Diagnostics.Errors()[0]
	text := e.Summary() + "\n" + e.Detail()
	for _, want := range []string{"409", "req-upgrade-1", "updating", "version 1", "timeouts.update"} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostic does not say %q:\n%s", want, text)
		}
	}
}
