package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// cancelAPI is one cluster that follows the contract's delete and cancel
// admission: delete only from `active` or `failed`, cancel only from
// `creating` or `updating`.
type cancelAPI struct {
	mu    sync.Mutex
	state string // "" once the cluster is gone
	// cancelStatus, when set, is how cancel answers instead of settling.
	cancelStatus int
	// settleOnCancel is the state the cluster has reached on its own by the
	// time a refused cancel arrives.
	settleOnCancel string
	calls          []string

	// execution is the id of the operation in flight, as a read of the
	// cluster names it in `operation.executionId`. Empty reads as "e1".
	execution string
	// noExecution leaves `operation` out of every read.
	noExecution bool
	// moves is how many cancels find that the cluster has begun another
	// operation since it was read: each one moves execution on first.
	moves int
	// sent is the executionId each cancel named.
	sent []string

	// onBusy is the cluster's `on_destroy_busy` in state, or "" for unset.
	onBusy string
	// timeout bounds the destroy. Zero is a minute.
	timeout time.Duration
	// settlesAfter is how many reads a `creating` or `updating` cluster takes
	// to settle `active` on its own. Zero never does, short of a cancel.
	settlesAfter int
	busyReads    int
	// restarts is how many deletes find that another operation began on the
	// settled cluster just before them, as another run's apply would.
	restarts int
	// opStatus, when set, is the `operation.status` a read reports. Left
	// empty, it follows the state: `succeeded` once active, `failed` once
	// failed, and `running` while busy. A cancel sets it to `cancelled`.
	opStatus string
	// afterCancel is the state an accepted cancel leaves, `failed` when
	// empty, as the contract settles it. "gone" leaves no cluster at all.
	afterCancel string
	// cancelDelay holds every cancel this long before it's answered.
	cancelDelay time.Duration
	// onRefused, when set, runs each time a request is refused as busy.
	onRefused func()

	// uid is the cluster's uid as a read names it, none when empty.
	uid string
	// held is the uid the row holds, none when empty.
	held string
	// replacedOnRead is the read on which the cluster is found to be another,
	// counting from 1: the row's cluster was deleted and another tool made
	// a new one under the id, with uid newcomer, `creating` and settling
	// `active` two reads later. Zero never.
	replacedOnRead int
	reads          int
	// readsFail answers every read 500.
	readsFail bool
}

// newcomer is the uid of the cluster another tool makes under the id.
const newcomer = "00000000-0000-4000-8000-0000000000bb"

func (a *cancelAPI) status() string {
	if a.opStatus != "" {
		return a.opStatus
	}
	switch a.state {
	case "active":
		return "succeeded"
	case "failed":
		return "failed"
	}
	return "running"
}

func (a *cancelAPI) running() string {
	if a.execution == "" {
		a.execution = "e1"
	}
	return a.execution
}

func (a *cancelAPI) refuse(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"error":{"code":%d,"message":%q,"requestId":"req-cancel-1"}}`, status, msg)
}

func (a *cancelAPI) serve(t *testing.T) *httptest.Server {
	t.Helper()
	const clusterPath = "/v1/regions/{r}/projects/{p}/compute/clusters/{id}"
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+clusterPath, func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		if a.reads++; a.reads == a.replacedOnRead {
			a.state, a.uid, a.execution, a.opStatus, a.busyReads, a.settlesAfter = "creating", newcomer, "theirs", "", 0, 2
		}
		if a.readsFail {
			writeTestError(t, w, http.StatusInternalServerError, "unavailable")
			return
		}
		switch a.state {
		case "":
			writeTestError(t, w, http.StatusNotFound, "not found")
			return
		case "deleting":
			a.state = "" // gone on the next read
			writeJSON(w, map[string]any{"id": "c1", "state": "deleting"})
			return
		}
		if (a.state == "creating" || a.state == "updating") && a.settlesAfter > 0 {
			a.busyReads++
			if a.busyReads >= a.settlesAfter {
				a.state, a.busyReads = "active", 0
			}
		}
		body := map[string]any{"id": "c1", "state": a.state}
		if a.uid != "" {
			body["uid"] = a.uid
		}
		if !a.noExecution {
			body["operation"] = map[string]any{"executionId": a.running(), "status": a.status()}
		}
		writeJSON(w, body)
	})
	mux.HandleFunc("DELETE "+clusterPath, func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.calls = append(a.calls, "delete")
		if a.state == "active" && a.restarts > 0 {
			a.restarts--
			a.state = "updating"
			a.execution = fmt.Sprintf("e%d", len(a.calls)+1)
		}
		switch a.state {
		case "":
			writeTestError(t, w, http.StatusNotFound, "not found")
		case "active", "failed":
			a.state = "deleting"
			w.WriteHeader(http.StatusNoContent)
		default:
			a.refuse(w, http.StatusConflict, "cluster is "+a.state+"; cancel it first (POST .../cancel)")
		}
	})
	mux.HandleFunc("POST "+clusterPath+"/cancel", func(w http.ResponseWriter, r *http.Request) {
		if a.cancelDelay > 0 {
			select {
			case <-time.After(a.cancelDelay):
			case <-r.Context().Done():
			}
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.calls = append(a.calls, "cancel")
		var req struct {
			ExecutionID string `json:"executionId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ExecutionID == "" {
			a.refuse(w, http.StatusBadRequest, "executionId is required")
			return
		}
		a.sent = append(a.sent, req.ExecutionID)
		if a.moves > 0 {
			a.moves--
			a.execution = fmt.Sprintf("e%d", len(a.sent)+1)
		}
		if a.settleOnCancel == "" && req.ExecutionID != a.running() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = fmt.Fprintf(w, `{"error":{"code":409,"message":"executionId %s is not the operation in flight","requestId":"req-cancel-1",`+
				`"details":[{"reason":"CONFLICT_VERSION"}]}}`, req.ExecutionID)
			return
		}
		if a.settleOnCancel != "" {
			a.state = a.settleOnCancel
		}
		if a.cancelStatus != 0 {
			a.refuse(w, a.cancelStatus, "cancel refused while "+a.state)
			return
		}
		if a.state != "creating" && a.state != "updating" {
			a.refuse(w, http.StatusConflict, "cluster is "+a.state+"; nothing is in flight")
			return
		}
		a.state, a.opStatus = "failed", "cancelled"
		switch a.afterCancel {
		case "":
		case "gone":
			a.state = ""
		default:
			a.state = a.afterCancel
		}
		writeJSON(w, map[string]any{
			"id": "c1", "state": "failed", "stateReason": "CANCELLED",
			"operation": map[string]any{"executionId": req.ExecutionID, "status": "cancelled"},
		})
	})
	// The waits follow the event log. This fake has none, which stops them
	// following it.
	mux.HandleFunc("GET /v1/regions/test/projects/test/compute/clusters/c1/runtime/events", http.NotFound)
	// A refresh asks for the SSH users. A landscape without the route keeps
	// what state holds.
	mux.HandleFunc("GET /v1/regions/test/projects/test/compute/clusters/c1/ssh-access", http.NotFound)
	// Anything else, including an update, is answered as busy.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.calls = append(a.calls, r.Method+" "+r.URL.Path)
		onRefused := a.onRefused
		a.mu.Unlock()
		if onRefused != nil {
			onRefused()
		}
		a.refuse(w, http.StatusConflict, "the cluster is busy")
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func (a *cancelAPI) destroy(t *testing.T) resource.DeleteResponse {
	t.Helper()
	server := a.serve(t)
	timeout := a.timeout
	if timeout == 0 {
		timeout = time.Minute
	}
	r := &clusterResource{meta: testClusterMeta(t, server.URL, timeout)}
	values := clusterValues(verRef(1), verRef(1), 2, "creating")
	if a.onBusy != "" {
		values[onDestroyBusyAttribute] = str(a.onBusy)
	}
	raw, s := testValue(t, r, values)
	st := tfsdk.State{Raw: raw, Schema: s.Schema}
	req := resource.DeleteRequest{State: st}
	freshPrivate(t, &req.Private, nil)
	if d := recordUID(context.Background(), req.Private, a.held); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.DeleteResponse{State: st}
	r.Delete(context.Background(), req, &resp)
	return resp
}

func (a *cancelAPI) callList() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return strings.Join(a.calls, " ")
}

// A cluster still creating, typically tainted after its create wait timed out,
// is destroyed by cancelling the create first, and the user is told.
func TestDestroyCancelsAnOperationInFlight(t *testing.T) {
	for _, from := range []string{"creating", "updating"} {
		t.Run(from, func(t *testing.T) {
			api := &cancelAPI{state: from}
			resp := api.destroy(t)
			if resp.Diagnostics.HasError() {
				t.Fatalf("destroy: %v", resp.Diagnostics.Errors())
			}
			if got := api.callList(); got != "delete cancel delete" {
				t.Errorf("calls = %q", got)
			}
			if api.state != "" {
				t.Errorf("the cluster is still %s", api.state)
			}
			if !hasWarning(resp.Diagnostics, "cancelled") || !hasWarning(resp.Diagnostics, from) {
				t.Errorf("no warning naming the cancelled %s operation: %v", from, resp.Diagnostics)
			}
		})
	}
}

// The contract pins a cancel to one operation: the body names the execution
// id a read of the cluster gave just before, so a cancel acting on a stale
// view can't stop an operation it never saw.
func TestDestroyCancelsTheOperationItRead(t *testing.T) {
	api := &cancelAPI{state: "updating", execution: "op-7"}
	resp := api.destroy(t)
	if resp.Diagnostics.HasError() {
		t.Fatalf("destroy: %v", resp.Diagnostics.Errors())
	}
	if len(api.sent) != 1 || api.sent[0] != "op-7" {
		t.Errorf("cancels named %q, want one naming op-7", api.sent)
	}
}

// The cluster began another operation between the read and the cancel, so the
// platform refuses the stale id. The destroy reads the cluster again and
// cancels the operation now running, once.
func TestDestroyCancelsAgainWhenTheOperationMovedOn(t *testing.T) {
	api := &cancelAPI{state: "updating", moves: 1}
	resp := api.destroy(t)
	if resp.Diagnostics.HasError() {
		t.Fatalf("destroy: %v", resp.Diagnostics.Errors())
	}
	if got := strings.Join(api.sent, " "); got != "e1 e2" {
		t.Errorf("cancels named %q, want e1 then e2", got)
	}
	if got := api.callList(); got != "delete cancel cancel delete" {
		t.Errorf("calls = %q", got)
	}
	if api.state != "" {
		t.Errorf("the cluster is still %s", api.state)
	}
}

// An operation that moves on again is not chased: the destroy stops with the
// platform's answer and both ids, and the cluster stays in state.
func TestDestroyStopsWhenTheOperationKeepsMovingOn(t *testing.T) {
	api := &cancelAPI{state: "updating", moves: 2}
	resp := api.destroy(t)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a cancel refused twice reported success")
	}
	if got := strings.Join(api.sent, " "); got != "e1 e2" {
		t.Errorf("cancels named %q, want e1 then e2 and no third", got)
	}
	e := resp.Diagnostics.Errors()[0]
	text := e.Summary() + "\n" + e.Detail()
	for _, want := range []string{"409", "e2", "another operation", "Nothing was deleted"} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostic does not say %q:\n%s", want, text)
		}
	}
	if resp.State.Raw.IsNull() {
		t.Error("the cluster was dropped from state")
	}
}

// A cluster mid-operation that names no operation leaves nothing to cancel. No
// cancel is sent without an id, which the platform would refuse with a 400.
func TestDestroyNamesAMissingOperationRatherThanSendingNoID(t *testing.T) {
	api := &cancelAPI{state: "creating", noExecution: true}
	resp := api.destroy(t)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a cluster with no operation id reported success")
	}
	if got := api.callList(); got != "delete" {
		t.Errorf("calls = %q, want the delete and no cancel", got)
	}
	e := resp.Diagnostics.Errors()[0]
	text := e.Summary() + "\n" + e.Detail()
	for _, want := range []string{"operation.executionId", "creating", "run again"} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostic does not say %q:\n%s", want, text)
		}
	}
}

// A destroy of a cluster already being deleted, such as one an earlier
// destroy stopped waiting for, waits for that delete. It sends no second
// DELETE, which the platform would answer 409, and cancels nothing.
func TestDestroyWaitsOutADeleteAlreadyRunning(t *testing.T) {
	api := &cancelAPI{state: "deleting"}
	resp := api.destroy(t)
	if resp.Diagnostics.HasError() {
		t.Fatalf("destroy: %v", resp.Diagnostics.Errors())
	}
	if got := api.callList(); got != "" {
		t.Errorf("calls = %q, want no delete and no cancel", got)
	}
	if len(resp.Diagnostics.Warnings()) != 0 {
		t.Errorf("warned about a cancel that never happened: %v", resp.Diagnostics)
	}
}

// The operation finished on its own between the refused delete and the
// cancel. Nothing was cancelled, so the delete is simply asked again.
func TestDestroyRetriesWhenTheOperationSettlesFirst(t *testing.T) {
	api := &cancelAPI{state: "creating", settleOnCancel: "active"}
	resp := api.destroy(t)
	if resp.Diagnostics.HasError() {
		t.Fatalf("destroy: %v", resp.Diagnostics.Errors())
	}
	if got := api.callList(); got != "delete cancel delete" {
		t.Errorf("calls = %q", got)
	}
	if len(resp.Diagnostics.Warnings()) != 0 {
		t.Errorf("warned about a cancel the platform refused: %v", resp.Diagnostics)
	}
}

// Another delete started between the refused delete and the cancel. The cancel
// is refused because nothing is in flight, and the destroy waits for that
// delete instead of failing.
func TestDestroyWaitsWhenADeleteStartsBeforeTheCancel(t *testing.T) {
	api := &cancelAPI{state: "creating", settleOnCancel: "deleting"}
	resp := api.destroy(t)
	if resp.Diagnostics.HasError() {
		t.Fatalf("destroy: %v", resp.Diagnostics.Errors())
	}
	if api.state != "" {
		t.Errorf("the cluster is still %s", api.state)
	}
	if len(resp.Diagnostics.Warnings()) != 0 {
		t.Errorf("warned about a cancel the platform refused: %v", resp.Diagnostics)
	}
}

// A refused cancel stops the destroy with the platform's own answer, and the
// cluster stays in state so a later destroy can try again.
func TestDestroyStopsWhenTheCancelIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{"not cancellable", http.StatusConflict, "409"},
		{"engine unreachable", http.StatusGatewayTimeout, "504"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &cancelAPI{state: "creating", cancelStatus: tc.status}
			resp := api.destroy(t)
			if !resp.Diagnostics.HasError() {
				t.Fatal("a refused cancel reported success")
			}
			e := resp.Diagnostics.Errors()[0]
			text := e.Summary() + "\n" + e.Detail()
			for _, want := range []string{"cancel", tc.want, "req-cancel-1", "creating", "Nothing was deleted"} {
				if !strings.Contains(text, want) {
					t.Errorf("diagnostic does not say %q:\n%s", want, text)
				}
			}
			if got := api.callList(); got != "delete cancel" {
				t.Errorf("calls = %q", got)
			}
			if resp.State.Raw.IsNull() {
				t.Error("the cluster was dropped from state")
			}
		})
	}
}

// Cancelling is a destroy's decision and nothing else's. A refresh, a plan and
// an update all meet a cluster mid-operation and must leave it running.
func TestOnlyADestroyCancels(t *testing.T) {
	// The update's deadline passes once the platform has refused it as busy,
	// not after 50ms, which a loaded machine could spend before the update
	// was even sent. Then the test would pass without reaching the busy path.
	ctx := newTestDeadline(t)
	api := &cancelAPI{state: "creating", onRefused: ctx.expire}
	server := api.serve(t)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Hour)}
	stateRaw, s := testValue(t, r, clusterValues(verRef(1), verRef(1), 2, "creating"))
	st := tfsdk.State{Raw: stateRaw, Schema: s.Schema}

	var read resource.ReadResponse
	read.State = st
	r.Read(context.Background(), resource.ReadRequest{State: st}, &read)

	changed := clusterValues(verRef(1), verRef(1), 3, "")
	changed["display_name"] = tftypes.NewValue(tftypes.String, "renamed")
	planRaw, _ := testValue(t, r, changed)
	plan := tfsdk.Plan{Raw: planRaw, Schema: s.Schema}
	planResp := resource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: planRaw, Schema: s.Schema}, Plan: plan, State: st,
	}, &planResp)

	update := resource.UpdateResponse{State: st}
	r.Update(ctx, resource.UpdateRequest{State: st, Plan: plan}, &update)

	got := api.callList()
	if strings.Contains(got, "cancel") {
		t.Errorf("a non-destroy path cancelled the operation: %q", got)
	}
	if !strings.Contains(got, "PATCH") {
		t.Errorf("calls = %q, want the update sent and refused as busy", got)
	}
	if api.state != "creating" {
		t.Errorf("the cluster is %s, want it still creating", api.state)
	}
}

// A create whose wait times out leaves the operation running and says so:
// where it had got to, that Terraform taints it, and the untaint and apply
// that wait for it instead of replacing it.
func TestATimedOutWaitSaysTheOperationKeepsRunning(t *testing.T) {
	// The deadline passes on the second read of a still-creating cluster,
	// not on the clock. A 20ms operation timeout also bounded the POST, and
	// a loaded machine spent it there, so the create failed for want of an
	// answer and never reached the wait this test is about.
	ctx := newTestDeadline(t)
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			t.Error("a timed-out create was cancelled")
		}
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":"c1","state":"creating"}`)
			return
		}
		if reads.Add(1) == 2 {
			ctx.expire()
		}
		writeJSON(w, map[string]any{"id": "c1", "state": "creating"})
	}))
	t.Cleanup(server.Close)

	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Hour)}
	planRaw, s := testValue(t, r, clusterValues(verRef(1), "", 2, ""))
	resp := resource.CreateResponse{State: tfsdk.State{Raw: planRaw, Schema: s.Schema}}
	r.Create(ctx, resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a create that never settled reported success")
	}
	e := resp.Diagnostics.Errors()[0]
	text := e.Summary() + "\n" + e.Detail()
	for _, want := range []string{"still being created", "still `creating`", "still be running",
		"tainted", "terraform untaint <address>\n    terraform apply", "`timeouts`"} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostic does not say %q:\n%s", want, text)
		}
	}
	if got := stateAttr(t, resp.State, "id"); got != "c1" {
		t.Errorf("id in state = %q", got)
	}
}

// Only a timeout means the operation is still running. A wait that ended on a
// `failed` cluster must not tell the user a destroy has anything to cancel.
func TestOnlyATimeoutSaysTheOperationKeepsRunning(t *testing.T) {
	timedOut := fmt.Errorf("%w: the cluster to finish scaling did not settle", errWaitTimeout)
	failed := fmt.Errorf("the platform reports the cluster as failed")
	for _, tc := range []struct {
		err  error
		want bool
	}{{timedOut, true}, {failed, false}} {
		var diags diag.Diagnostics
		addClusterWaitError(&diags, "The cluster did not settle after scaling", changeKeepsRunning, tc.err)
		detail := diags.Errors()[0].Detail()
		if got := strings.Contains(detail, "terraform destroy` cancels"); got != tc.want {
			t.Errorf("%v: advice present = %v, want %v:\n%s", tc.err, got, tc.want, detail)
		}
	}
}

// planDestroyOf plans the destroy of a cluster that read prior at refresh, or
// its replacement when replace is set.
func planDestroyOf(t *testing.T, prior string, replace bool) resource.ModifyPlanResponse {
	t.Helper()
	r := guardedModifier(t, &clusterResource{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)},
		clusterTenantState)
	stateRaw, s := testValue(t, r, clusterValues(verRef(1), verRef(1), 2, prior))
	st := tfsdk.State{Raw: stateRaw, Schema: s.Schema}
	planRaw := tftypes.NewValue(stateRaw.Type(), nil)
	config := tfsdk.Config{Raw: planRaw, Schema: s.Schema}
	if replace {
		values := clusterValues(verRef(1), verRef(1), 2, prior)
		values["project"] = str("elsewhere")
		values["allow_replace"] = tftypes.NewValue(tftypes.Bool, true)
		planRaw, _ = testValue(t, r, values)
		config = tfsdk.Config{Raw: planRaw, Schema: s.Schema}
	}
	plan := tfsdk.Plan{Raw: planRaw, Schema: s.Schema}
	resp := resource.ModifyPlanResponse{Plan: plan}
	if replace {
		resp.RequiresReplace = path.Paths{path.Root("project")}
	}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{Config: config, Plan: plan, State: st}, &resp)
	return resp
}

// A destroy cancels whatever the cluster is running, and an upgrade cut off
// that way can't be resumed. The apply says so once it happens, but the
// approval comes first, so the plan says it too, when state shows an
// operation in flight.
func TestPlanningTheDestroyOfABusyClusterSaysItCancels(t *testing.T) {
	for _, tc := range []struct {
		prior   string
		replace bool
		warns   bool
	}{
		{"updating", false, true},
		{"creating", false, true},
		{"updating", true, true},
		{"active", false, false},
		{"failed", false, false},
		{"active", true, false},
	} {
		resp := planDestroyOf(t, tc.prior, tc.replace)
		if resp.Diagnostics.HasError() {
			t.Fatalf("%s: plan: %v", tc.prior, resp.Diagnostics.Errors())
		}
		if got := hasWarning(resp.Diagnostics, "cancel"); got != tc.warns {
			t.Errorf("%s, replace=%v: warned = %v, want %v: %v", tc.prior, tc.replace, got, tc.warns, resp.Diagnostics)
		}
	}
}

// With `on_destroy_busy = "wait"`, a destroy that meets a cluster mid-operation
// waits for the operation to finish and then deletes the cluster. Nothing is
// cancelled, so nothing is warned about.
func TestDestroyWaitsForTheOperationWhenAsked(t *testing.T) {
	for _, from := range []string{"creating", "updating"} {
		t.Run(from, func(t *testing.T) {
			api := &cancelAPI{state: from, onBusy: destroyWaits, settlesAfter: 3}
			resp := api.destroy(t)
			if resp.Diagnostics.HasError() {
				t.Fatalf("destroy: %v", resp.Diagnostics.Errors())
			}
			if got := api.callList(); got != "delete delete" {
				t.Errorf("calls = %q, want a refused delete, then the delete once the operation finished", got)
			}
			if api.state != "" {
				t.Errorf("the cluster is still %s", api.state)
			}
			if len(resp.Diagnostics.Warnings()) != 0 {
				t.Errorf("warned about a cancel that never happened: %v", resp.Diagnostics)
			}
		})
	}
}

// `cancel`, written out, is what an unset `on_destroy_busy` already does.
func TestDestroyCancelsWhenToldToCancel(t *testing.T) {
	api := &cancelAPI{state: "updating", onBusy: destroyCancels, settlesAfter: 3}
	resp := api.destroy(t)
	if resp.Diagnostics.HasError() {
		t.Fatalf("destroy: %v", resp.Diagnostics.Errors())
	}
	if got := api.callList(); got != "delete cancel delete" {
		t.Errorf("calls = %q", got)
	}
}

// Another run's apply can start an operation on the cluster between its
// settling and the delete. A destroy that waits waits again, for as long as the
// delete timeout allows, rather than giving up after the three refusals a
// cancelling destroy allows itself.
func TestDestroyThatWaitsWaitsOutEachNewOperation(t *testing.T) {
	api := &cancelAPI{state: "updating", onBusy: destroyWaits, settlesAfter: 2, restarts: 3}
	resp := api.destroy(t)
	if resp.Diagnostics.HasError() {
		t.Fatalf("destroy: %v", resp.Diagnostics.Errors())
	}
	if got := api.callList(); got != "delete delete delete delete delete" {
		t.Errorf("calls = %q, want four refused deletes and the one that landed", got)
	}
	if api.state != "" {
		t.Errorf("the cluster is still %s", api.state)
	}
}

// An operation that outlasts the delete timeout is reported, with what the
// destroy didn't do and the ways on. The cluster stays in state, untouched.
func TestDestroyThatWaitsReportsAnOperationThatOutlastsIt(t *testing.T) {
	api := &cancelAPI{state: "updating", onBusy: destroyWaits, timeout: 50 * time.Millisecond}
	resp := api.destroy(t)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a destroy whose wait ran out reported success")
	}
	if got := api.callList(); got != "delete" {
		t.Errorf("calls = %q, want the refused delete and no cancel", got)
	}
	e := resp.Diagnostics.Errors()[0]
	text := e.Summary() + "\n" + e.Detail()
	for _, want := range []string{"on_destroy_busy", "updating", "Nothing was deleted", "timeouts.delete"} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostic does not say %q:\n%s", want, text)
		}
	}
	if api.state != "updating" {
		t.Errorf("the cluster is %s, want it still updating", api.state)
	}
	if resp.State.Raw.IsNull() {
		t.Error("the cluster was dropped from state")
	}
}

// The plan warns before an approval that the destroy will cancel, and only
// when it will. One that waits says so instead, so the approval knows the
// destroy can take as long as the operation does.
func TestPlanWarnsAboutTheDestroyItWillDo(t *testing.T) {
	for _, tc := range []struct {
		onBusy, want, not string
	}{
		{"", "will cancel", "will wait"},
		{destroyCancels, "will cancel", "will wait"},
		{destroyWaits, "will wait", "will cancel"},
	} {
		t.Run("on_destroy_busy="+tc.onBusy, func(t *testing.T) {
			r := &clusterResource{}
			values := clusterValues(verRef(1), verRef(1), 2, "updating")
			if tc.onBusy != "" {
				values[onDestroyBusyAttribute] = str(tc.onBusy)
			}
			raw, s := testValue(t, r, values)
			var diags diag.Diagnostics
			warnOnBusyDestroy(context.Background(), tfsdk.State{Raw: raw, Schema: s.Schema}, &diags)
			if !hasWarning(diags, tc.want) || hasWarning(diags, tc.not) {
				t.Errorf("warnings = %v, want one that says %q", diags.Warnings(), tc.want)
			}
		})
	}
}

// A create whose wait times out records what a destroy that follows reads:
// `on_destroy_busy` and the delete timeout. Without them a cluster destroyed
// mid-create, the commonest one a destroy meets mid-operation, would always be
// cancelled.
func TestATimedOutCreateKeepsTheDestroySettings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = fmt.Fprint(w, `{"id":"c1","state":"creating"}`)
			return
		}
		writeJSON(w, map[string]any{"id": "c1", "state": "creating"})
	}))
	t.Cleanup(server.Close)

	r := &clusterResource{meta: testClusterMeta(t, server.URL, 20*time.Millisecond)}
	values := clusterValues(verRef(1), "", 2, "")
	values[onDestroyBusyAttribute] = str(destroyWaits)
	planRaw, s := testValue(t, r, values)
	nullRaw := tftypes.NewValue(planRaw.Type(), nil)
	resp := resource.CreateResponse{State: tfsdk.State{Raw: nullRaw, Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a create that never settled reported success")
	}
	if got := stateAttr(t, resp.State, onDestroyBusyAttribute); got != destroyWaits {
		t.Errorf("on_destroy_busy in state = %q, want %q", got, destroyWaits)
	}
}

// The way out of a destroy that waits, or into one, is to change
// `on_destroy_busy` and apply. On a cluster that is still `updating` that
// apply sends nothing and waits for nothing: the platform holds none of what
// changed, so it records the plan's value and returns. Otherwise it would sit
// out the very operation the change was made to stop waiting for.
func TestChangingOnlyProviderSettingsOnABusyClusterSendsNothing(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		writeJSON(w, map[string]any{"id": "c1", "state": "updating"})
	}))
	t.Cleanup(server.Close)

	r := &clusterResource{meta: testClusterMeta(t, server.URL, 200*time.Millisecond)}
	stateValues := clusterValues(verRef(1), verRef(1), 2, "updating")
	stateValues[onDestroyBusyAttribute] = str(destroyWaits)
	stateRaw, s := testValue(t, r, stateValues)
	planValues := clusterValues(verRef(1), verRef(1), 2, "")
	planValues[onDestroyBusyAttribute] = str(destroyCancels)
	planValues["state"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	planValues["node_count"] = tftypes.NewValue(tftypes.Number, tftypes.UnknownValue)
	planRaw, _ := testValue(t, r, planValues)

	resp := resource.UpdateResponse{State: tfsdk.State{Raw: planRaw, Schema: s.Schema}}
	started := time.Now()
	r.Update(context.Background(), resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics.Errors())
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Errorf("the update took %s, so it waited on the cluster", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("the update sent %v, want nothing", calls)
	}
	if got := stateAttr(t, resp.State, onDestroyBusyAttribute); got != destroyCancels {
		t.Errorf("on_destroy_busy in state = %q, want %q", got, destroyCancels)
	}
	if got := stateAttr(t, resp.State, "state"); got != "updating" {
		t.Errorf("state = %q, want the prior %q", got, "updating")
	}
}

// A plan can change a value the platform reads as the same, such as pinning
// `cluster_type` to the version the cluster is already bound to. That sends
// nothing either, but the state it writes is the plan's, or Terraform reports
// the result as inconsistent with the plan.
func TestAnUpdateThatSendsNothingRecordsThePlan(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the update sent %s %s", r.Method, r.URL.Path)
		writeJSON(w, map[string]any{"id": "c1", "state": "active"})
	}))
	t.Cleanup(server.Close)

	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	stateRaw, s := testValue(t, r, clusterValues(upgradeTypeRef, verRef(1), 2, "active"))
	planValues := clusterValues(verRef(1), verRef(1), 2, "")
	planValues["state"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	planValues["node_count"] = tftypes.NewValue(tftypes.Number, tftypes.UnknownValue)
	planRaw, _ := testValue(t, r, planValues)

	resp := resource.UpdateResponse{State: tfsdk.State{Raw: planRaw, Schema: s.Schema}}
	r.Update(context.Background(), resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics.Errors())
	}
	if got := stateAttr(t, resp.State, "cluster_type"); got != verRef(1) {
		t.Errorf("cluster_type = %q, want the plan's %q", got, verRef(1))
	}
	if got := stateAttr(t, resp.State, "state"); got != "active" {
		t.Errorf("state = %q, want the prior %q for what the plan left unknown", got, "active")
	}
}

// An import adopts every pool, so a configuration that names only the workers
// plans dropping the control plane's pool from node_pool. That sends nothing,
// and on a cluster still `creating` it must not wait for the create either.
// It did, for the whole update timeout, on the first import on hardware.
func TestNarrowingNodePoolOnABusyClusterSendsNothing(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		writeJSON(w, map[string]any{"id": "c1", "state": "creating"})
	}))
	t.Cleanup(server.Close)

	obj := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"node_count": tftypes.Number}}
	pool := func(n int) tftypes.Value {
		return tftypes.NewValue(obj, map[string]tftypes.Value{"node_count": tftypes.NewValue(tftypes.Number, n)})
	}

	r := &clusterResource{meta: testClusterMeta(t, server.URL, 200*time.Millisecond)}
	stateValues := clusterValues(verRef(1), verRef(1), 1, "creating")
	stateValues["node_pool"] = tftypes.NewValue(tftypes.Map{ElementType: obj},
		map[string]tftypes.Value{"control-plane": pool(1), "workers": pool(1)})
	stateRaw, s := testValue(t, r, stateValues)
	planValues := clusterValues(verRef(1), verRef(1), 1, "")
	planValues["state"] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	planValues["node_count"] = tftypes.NewValue(tftypes.Number, tftypes.UnknownValue)
	planRaw, _ := testValue(t, r, planValues)

	resp := resource.UpdateResponse{State: tfsdk.State{Raw: planRaw, Schema: s.Schema}}
	started := time.Now()
	r.Update(context.Background(), resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics.Errors())
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Errorf("the update took %s, so it waited on the cluster", elapsed)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("the update sent %v, want nothing", calls)
	}
	if got := stateAttr(t, resp.State, "state"); got != "creating" {
		t.Errorf("state = %q, want the prior %q", got, "creating")
	}
}

// A destroy's warning about its cancel says how the cancel ended, from the
// operation the settled cluster reports, never "did not finish" for one that
// did.
func TestADestroySaysHowItsCancelEnded(t *testing.T) {
	was := clusterNow{found: true, state: k0rdent.ClusterStateCreating, execution: "e1",
		status: k0rdent.OperationStatusRunning}
	for _, tc := range []struct {
		name  string
		after clusterNow
		says  []string
		never string
	}{
		{"cancelled", clusterNow{found: true, state: k0rdent.ClusterStateFailed, execution: "e1",
			status: k0rdent.OperationStatusCancelled},
			[]string{"Cancelled the cluster's in-flight operation", "reads `cancelled`", "did not finish"}, "landed"},
		{"finished anyway", clusterNow{found: true, state: k0rdent.ClusterStateActive, execution: "e1",
			status: k0rdent.OperationStatusSucceeded},
			[]string{"finished before the cancel took effect", "reads `succeeded`", "`active`", "Nothing was cancelled"},
			"did not finish"},
		{"failed first", clusterNow{found: true, state: k0rdent.ClusterStateFailed, execution: "e1",
			status: k0rdent.OperationStatusFailed},
			[]string{"failed before the cancel took effect", "reads `failed`", "Nothing was cancelled"}, "did not finish"},
		{"another operation", clusterNow{found: true, state: k0rdent.ClusterStateActive, execution: "e2",
			status: k0rdent.OperationStatusSucceeded},
			[]string{"Asked the platform to cancel", "operation `e2` reading `succeeded`", "can't be read"}, "did not finish"},
		{"gone", clusterNow{},
			[]string{"Asked the platform to cancel", "gone by the next read"}, "did not finish"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			warnCancelEnded(&diags, k0rdent.ClusterStateCreating, was, tc.after)
			if diags.HasError() || len(diags.Warnings()) != 1 {
				t.Fatalf("want one warning, got %v", diags)
			}
			said := diags.Warnings()[0].Summary() + " " + diags.Warnings()[0].Detail()
			for _, want := range tc.says {
				if !strings.Contains(said, want) {
					t.Errorf("the warning doesn't say %q: %s", want, said)
				}
			}
			if strings.Contains(said, tc.never) {
				t.Errorf("the warning says %q: %s", tc.never, said)
			}
		})
	}
}

// A cancel and a delete go by id. If the row's cluster is deleted outside
// Terraform and another tool makes one under the id while a destroy runs, the
// destroy must not cancel or delete that one. Every read before a cancel or a
// delete checks the uid: another one there means the row's cluster is gone,
// so nothing more is sent, the destroy succeeds so Terraform drops the row,
// and a warning names both uids.
func TestDestroySendsNothingToAnotherClusterUnderTheID(t *testing.T) {
	const ours = "00000000-0000-4000-8000-0000000000aa"
	for _, tc := range []struct {
		name string
		api  *cancelAPI
		// calls is every cancel and delete sent, all to the row's cluster.
		calls string
	}{
		{"at the first read", &cancelAPI{state: "active", replacedOnRead: 1}, ""},
		{"after a refused delete, before the cancel", &cancelAPI{state: "creating", replacedOnRead: 2}, "delete"},
		{"after a cancel refused for a stale operation", &cancelAPI{state: "creating", moves: 1, replacedOnRead: 3}, "delete cancel"},
		{"while waiting for the cancel to settle", &cancelAPI{state: "creating", replacedOnRead: 3}, "delete cancel"},
		{"while waiting for the operation to finish", &cancelAPI{state: "updating", onBusy: destroyWaits, replacedOnRead: 3}, "delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := tc.api
			api.uid, api.held = ours, ours
			resp := api.destroy(t)
			if resp.Diagnostics.HasError() {
				t.Fatalf("destroy: %v", resp.Diagnostics.Errors())
			}
			if got := api.callList(); got != tc.calls {
				t.Errorf("calls = %q, want %q", got, tc.calls)
			}
			if api.state != "creating" || api.uid != newcomer {
				t.Errorf("the other tool's cluster is %q with uid %q, want it untouched", api.state, api.uid)
			}
			var warned bool
			for _, w := range resp.Diagnostics.Warnings() {
				if strings.Contains(w.Summary(), `"c1" is another cluster now`) {
					warned = strings.Contains(w.Detail(), ours) && strings.Contains(w.Detail(), newcomer) &&
						strings.Contains(w.Detail(), "sent nothing to it")
				}
				if strings.Contains(w.Summary(), "cancel") {
					t.Errorf("warned about a cancel ending: %s", w.Summary())
				}
			}
			if !warned {
				t.Errorf("no warning naming both uids: %v", resp.Diagnostics)
			}
		})
	}
}

// A destroy whose first read fails sends no DELETE when the row holds a uid,
// since only the read tells its cluster from another made under the id. A
// row holding none can't tell them apart anyway, and sends the DELETE as
// before.
func TestDestroySendsNothingUncheckedWhenTheReadFails(t *testing.T) {
	const ours = "00000000-0000-4000-8000-0000000000aa"
	for _, tc := range []struct{ name, held, calls string }{
		{"uid recorded", ours, ""},
		{"none recorded", "", "delete"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &cancelAPI{state: "active", uid: ours, held: tc.held, readsFail: true}
			resp := api.destroy(t)
			if got := api.callList(); got != tc.calls {
				t.Errorf("calls = %q, want %q", got, tc.calls)
			}
			if tc.held != "" && !resp.Diagnostics.HasError() {
				t.Error("the destroy passed without reading the cluster")
			}
		})
	}
}

// A row holding the uid of the cluster it reads is destroyed as before, and
// so is a row holding none, which can't tell the clusters apart.
func TestDestroyDeletesTheClusterItHolds(t *testing.T) {
	const ours = "00000000-0000-4000-8000-0000000000aa"
	for _, tc := range []struct{ name, held string }{{"same uid", ours}, {"none recorded", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			api := &cancelAPI{state: "creating", uid: ours, held: tc.held}
			resp := api.destroy(t)
			if resp.Diagnostics.HasError() {
				t.Fatalf("destroy: %v", resp.Diagnostics.Errors())
			}
			if got := api.callList(); got != "delete cancel delete" {
				t.Errorf("calls = %q", got)
			}
			if api.state != "" {
				t.Errorf("the cluster is still %s", api.state)
			}
		})
	}
}
