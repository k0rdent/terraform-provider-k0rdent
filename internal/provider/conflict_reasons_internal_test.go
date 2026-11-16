package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// The platform names a compute 409's cause differently by API version. v1.3
// answers some refusals with no reason and puts the rest on details[] only.
// From v1.4 every compute 409 names a reason, at the top level and on its one
// details[] entry, and a refusal that clears by itself is CONFLICT_STATE or
// CONFLICT_VERSION. The bodies here are each version's shape, from its
// contract's examples.

// conflictV13 is a v1.3 409: reason, when there is one, on details[] only.
func conflictV13(reason string) string {
	if reason == "" {
		return `{"error":{"code":409,"message":"conflict: refused","requestId":"req-1"}}`
	}
	return fmt.Sprintf(`{"error":{"code":409,"message":"conflict: refused","requestId":"req-1",`+
		`"details":[{"reason":%q,"message":"conflict: refused"}]}}`, reason)
}

// conflictV14 is a v1.4 409: reason at the top level and on details[].
func conflictV14(reason string) string {
	return fmt.Sprintf(`{"error":{"code":409,"message":"conflict: refused","requestId":"req-1","reason":%q,`+
		`"details":[{"reason":%q,"message":"conflict: refused"}]}}`, reason, reason)
}

// topLevelOnly is a 409 with its reason at the top level alone, as
// PROJECT_REGION_MISMATCH and QUOTA_UNASSIGNED are in both versions.
func topLevelOnly(reason string) string {
	return fmt.Sprintf(`{"error":{"code":409,"message":"conflict: refused","requestId":"req-1","reason":%q}}`, reason)
}

func conflictError(body string) *k0rdent.APIError {
	return k0rdent.NewAPIError(answer(http.StatusConflict, "", body), []byte(body))
}

// A cluster scale refused while its quota accounting settles, or by another
// operation that started first, is resent while the cluster reads active.
// v1.3 answers both with no reason, v1.4 with CONFLICT_STATE and
// CONFLICT_VERSION. Every other reason is the answer.
func TestScaleMayRetryOnEitherVersion(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		state      k0rdent.ClusterState
		want       bool
	}{
		{"v1.3 no reason", conflictV13(""), "active", true},
		{"v1.4 CONFLICT_STATE", conflictV14("CONFLICT_STATE"), "active", true},
		{"v1.4 CONFLICT_VERSION", conflictV14("CONFLICT_VERSION"), "active", true},
		{"v1.4 CONFLICT_STATE on a busy cluster", conflictV14("CONFLICT_STATE"), "updating", false},
		{"v1.4 CONFLICT_STATE on a failed cluster", conflictV14("CONFLICT_STATE"), "failed", false},
		{"v1.4 QUOTA_EXCEEDED", conflictV14("QUOTA_EXCEEDED"), "active", false},
		{"QUOTA_UNASSIGNED", topLevelOnly("QUOTA_UNASSIGNED"), "active", false},
		{"PROJECT_REGION_MISMATCH", topLevelOnly("PROJECT_REGION_MISMATCH"), "active", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scaleMayRetry(conflictError(tc.body), func() (k0rdent.ClusterState, bool) { return tc.state, true })
			if got != tc.want {
				t.Errorf("retry = %v, want %v", got, tc.want)
			}
		})
	}
}

// The scale itself, refused once while the cluster reads active, goes through
// on the resend in either version's words.
func TestAScaleRefusedWhileTheClusterSettlesIsSentAgain(t *testing.T) {
	for name, body := range map[string]string{
		"v1.3 no reason":        conflictV13(""),
		"v1.4 CONFLICT_STATE":   conflictV14("CONFLICT_STATE"),
		"v1.4 CONFLICT_VERSION": conflictV14("CONFLICT_VERSION"),
	} {
		t.Run(name, func(t *testing.T) {
			api := &upgradeAPI{bound: 1, state: "active", workers: 2, settlingScales: 1, settlingScaleBody: body}
			if resp := runUpgrade(t, api, 1, 3); resp.Diagnostics.HasError() {
				t.Fatalf("update: %v", resp.Diagnostics.Errors())
			}
			if got := fmt.Sprint(api.calls); got != "[scale 3 scale 3]" {
				t.Errorf("calls = %s, want the scale sent twice", got)
			}
		})
	}
}

// An instance group scale refused while another operation runs, or while its
// quota accounting settles, is the wait the group's state then decides. v1.3
// answers both with CONFLICT_IN_USE on details[], v1.4 with CONFLICT_IN_USE
// and CONFLICT_STATE at the top level too. A pool's or the quota's refusal is
// the answer.
func TestIsLifecycleConflictOnEitherVersion(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"v1.3 no reason", conflictV13(""), true},
		{"v1.3 CONFLICT_IN_USE", conflictV13("CONFLICT_IN_USE"), true},
		{"v1.4 CONFLICT_IN_USE", conflictV14("CONFLICT_IN_USE"), true},
		{"v1.4 CONFLICT_STATE", conflictV14("CONFLICT_STATE"), true},
		{"v1.4 QUOTA_EXCEEDED", conflictV14("QUOTA_EXCEEDED"), false},
		{"QUOTA_UNASSIGNED", topLevelOnly("QUOTA_UNASSIGNED"), false},
		{"POOL_EMPTY", conflictV13("POOL_EMPTY"), false},
		{"RESERVATION_RELEASED", conflictV13("RESERVATION_RELEASED"), false},
		{"PROJECT_REGION_MISMATCH", topLevelOnly("PROJECT_REGION_MISMATCH"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLifecycleConflict(conflictError(tc.body)); got != tc.want {
				t.Errorf("lifecycle conflict = %v, want %v", got, tc.want)
			}
		})
	}
}

// A storage raise refused while the cluster isn't active, while an earlier
// size change settles, or by a concurrent writer, is sent again once the
// cluster settles. v1.3 answers these with no reason, v1.4 with
// CONFLICT_STATE and CONFLICT_VERSION.
func TestUnsettledRaiseOnEitherVersion(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       bool
	}{
		{"v1.3 no reason", conflictV13(""), true},
		{"v1.4 CONFLICT_STATE", conflictV14("CONFLICT_STATE"), true},
		{"v1.4 CONFLICT_VERSION", conflictV14("CONFLICT_VERSION"), true},
		{"v1.4 QUOTA_EXCEEDED", conflictV14("QUOTA_EXCEEDED"), false},
		{"v1.4 CONFLICT_IMMUTABLE", conflictV14("CONFLICT_IMMUTABLE"), false},
		{"QUOTA_UNASSIGNED", topLevelOnly("QUOTA_UNASSIGNED"), false},
		{"PROJECT_REGION_MISMATCH", topLevelOnly("PROJECT_REGION_MISMATCH"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			if got := unsettledRaise(answer(http.StatusConflict, "", tc.body), body); got != tc.want {
				t.Errorf("unsettled = %v, want %v", got, tc.want)
			}
		})
	}
}

// The raise itself, refused once in v1.4's words, goes through on the resend.
func TestARaiseRefusedWithV14sReasonIsSentAgain(t *testing.T) {
	for _, reason := range []string{"CONFLICT_STATE", "CONFLICT_VERSION"} {
		t.Run(reason, func(t *testing.T) {
			api := &upgradeAPI{bound: 1, workers: 2, state: "active", ceiling: 100, busyRaises: 1,
				busyRaiseBody: conflictV14(reason)}
			resp := runRaise(t, api, 250)
			if resp.Diagnostics.HasError() {
				t.Fatal(resp.Diagnostics)
			}
			if got := strings.Join(api.calls, " "); got != "storage 250 storage 250" {
				t.Errorf("calls = %q, want the raise sent twice", got)
			}
		})
	}
}

// A file store update that meets a store still settling its capacity or its
// quota accounting is sent once more, whichever version's reason says so:
// v1.3's CONFLICT_IN_USE or v1.4's CONFLICT_STATE.
func TestFileStoreUpdateSendsOnceMoreWhileAnActiveStoreSettles(t *testing.T) {
	for name, body := range map[string]string{
		"v1.3 CONFLICT_IN_USE": conflictV13("CONFLICT_IN_USE"),
		"v1.4 CONFLICT_STATE":  conflictV14("CONFLICT_STATE"),
	} {
		t.Run(name, func(t *testing.T) {
			b := newBusyStore(t, 0)
			b.inUseWhileActive = 1
			b.activeRefusal = body
			if _, errs := updateBusyStore(t, b, time.Minute); len(errs) > 0 {
				t.Fatalf("Update: %v", errs)
			}
			if got := len(b.api.etags()); got != 2 {
				t.Errorf("%d PATCHes, want the refused one and the resend", got)
			}
		})
	}
}

// refuseCreate makes the first n cluster creates answer body and make
// nothing, as a create that lost a race or met an unsettled dependency does.
func refuseCreate(api *clusterAPI, n int, body string) {
	api.answer = func(sent int, id string, w http.ResponseWriter) bool {
		if sent > n {
			return false
		}
		delete(api.tags, id)
		w.WriteHeader(http.StatusConflict)
		if _, err := w.Write([]byte(body)); err != nil {
			api.t.Errorf("write: %v", err)
		}
		return true
	}
}

// A cluster create that lost a race made nothing, and the contract says to
// send it again: v1.3 answers that CONFLICT_VERSION on details[] only, v1.4
// at the top level too.
func TestAClusterCreateThatLostARaceIsSentAgain(t *testing.T) {
	for name, body := range map[string]string{
		"v1.3": conflictV13("CONFLICT_VERSION"),
		"v1.4": conflictV14("CONFLICT_VERSION"),
	} {
		t.Run(name, func(t *testing.T) {
			api := newClusterAPI(t)
			refuseCreate(api, 1, body)
			resp := createCluster(t, api.serve(), "c1")
			if resp.Diagnostics.HasError() {
				t.Fatalf("Create: %v", resp.Diagnostics.Errors())
			}
			if len(api.sent) != 2 || stateID(t, resp.State) != "c1" {
				t.Errorf("creates sent = %v, state id %q, want two sends and c1", api.sent, stateID(t, resp.State))
			}
		})
	}
}

// One that keeps losing stops after createAttempts sends with the platform's
// answer, and records nothing.
func TestAClusterCreateThatKeepsLosingARaceStops(t *testing.T) {
	api := newClusterAPI(t)
	refuseCreate(api, 1<<30, conflictV14("CONFLICT_VERSION"))
	resp := createCluster(t, api.serve(), "c1")
	if !resp.Diagnostics.HasError() {
		t.Fatal("a create refused every time reported success")
	}
	if len(api.sent) != createAttempts {
		t.Errorf("creates sent = %d, want %d", len(api.sent), createAttempts)
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state was written: %v", resp.State.Raw)
	}
}

// A cluster create refused for a state no resend clears, a project being
// deleted, an address pool that isn't active or a security group being
// deleted, is reported at once. v1.4 names it CONFLICT_STATE, and the error
// says what to do about each. v1.3 answers the project case with no reason,
// and gets the platform's message alone.
func TestAClusterCreateRefusedForAStateIsReportedWithWhatToDo(t *testing.T) {
	for name, tc := range map[string]struct {
		body   string
		advice bool
	}{
		"v1.3 no reason":      {conflictV13(""), false},
		"v1.4 CONFLICT_STATE": {conflictV14("CONFLICT_STATE"), true},
	} {
		t.Run(name, func(t *testing.T) {
			api := newClusterAPI(t)
			refuseCreate(api, 1<<30, tc.body)
			resp := createCluster(t, api.serve(), "c1")
			errs := resp.Diagnostics.Errors()
			if len(errs) == 0 {
				t.Fatal("a refused create reported success")
			}
			if len(api.sent) != 1 {
				t.Errorf("creates sent = %d, want 1", len(api.sent))
			}
			detail := errs[0].Detail()
			for _, want := range []string{"another project", "`" + ResourceName(addressPoolsDataSourceSuffix) + "`",
				"`security_groups`"} {
				if strings.Contains(detail, want) != tc.advice {
					t.Errorf("says %q = %v, want %v:\n%s", want, !tc.advice, tc.advice, detail)
				}
			}
		})
	}
}

// groupCreatePlatform answers the first `refuse` creates of instance group
// "pig" with refusal, which makes nothing, then creates it active.
type groupCreatePlatform struct {
	mu      sync.Mutex
	refuse  int
	refusal string
	posts   int
	created bool
}

func (p *groupCreatePlatform) serve(t *testing.T) *httptest.Server {
	group, err := json.Marshal(deletingGroupBody("active"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		status, body := http.StatusOK, string(group)
		switch {
		case r.Method == http.MethodPost:
			p.posts++
			if p.posts <= p.refuse {
				status, body = http.StatusConflict, p.refusal
			} else {
				status, p.created = http.StatusCreated, true
			}
		case strings.HasSuffix(r.URL.Path, "/instance-groups/pig") && !p.created:
			status, body = http.StatusNotFound, `{"error":{"code":404,"message":"not found","requestId":"req-2"}}`
		}
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func createGroup(t *testing.T, p *groupCreatePlatform) (resource.CreateResponse, int) {
	t.Helper()
	r := &instanceGroupResource{meta: testClusterMeta(t, p.serve(t).URL, time.Minute)}
	planRaw, s := testValue(t, r, with(opsGroup("active"), map[string]tftypes.Value{
		"state": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	}))
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(planRaw.Type(), nil), Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}, &resp)
	p.mu.Lock()
	defer p.mu.Unlock()
	return resp, p.posts
}

// An instance group create that lost a race to an address pool's change made
// nothing, and v1.4's contract says to retry it.
func TestAnInstanceGroupCreateThatLostARaceIsSentAgain(t *testing.T) {
	resp, posts := createGroup(t, &groupCreatePlatform{refuse: 1, refusal: conflictV14("CONFLICT_VERSION")})
	if resp.Diagnostics.HasError() {
		t.Fatalf("Create: %v", resp.Diagnostics.Errors())
	}
	if posts != 2 {
		t.Errorf("creates sent = %d, want 2", posts)
	}
}

// An instance group create refused because an address pool isn't active is
// reported at once, with what to do.
func TestAnInstanceGroupCreateRefusedForAStateIsReportedWithWhatToDo(t *testing.T) {
	resp, posts := createGroup(t, &groupCreatePlatform{refuse: 1 << 30, refusal: conflictV14("CONFLICT_STATE")})
	errs := resp.Diagnostics.Errors()
	if len(errs) == 0 {
		t.Fatal("a refused create reported success")
	}
	if posts != 1 {
		t.Errorf("creates sent = %d, want 1", posts)
	}
	if want := "`" + ResourceName(addressPoolsDataSourceSuffix) + "`"; !strings.Contains(errs[0].Detail(), want) {
		t.Errorf("doesn't name %s:\n%s", want, errs[0].Detail())
	}
}
