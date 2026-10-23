package provider

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// The cancel action is the one way to stop a cluster's operation and keep the
// cluster. These run it against cancelAPI, the same contract-shaped cluster
// the destroy's cancel tests use.

// actionConfig builds the action's configuration with every attribute null
// but those in set.
func actionConfig(t *testing.T, a action.Action, set map[string]tftypes.Value) tfsdk.Config {
	t.Helper()
	var s action.SchemaResponse
	a.Schema(context.Background(), action.SchemaRequest{}, &s)
	object, ok := s.Schema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("the action's schema is not an object")
	}
	values := make(map[string]tftypes.Value, len(object.AttributeTypes))
	for name, attrType := range object.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	for name, v := range set {
		values[name] = v
	}
	return tfsdk.Config{Raw: tftypes.NewValue(object, values), Schema: s.Schema}
}

// invoked is what one invocation reported.
type invoked struct {
	diags    diag.Diagnostics
	progress []string
}

func (i invoked) said() string {
	var b strings.Builder
	for _, p := range i.progress {
		b.WriteString(p + "\n")
	}
	for _, d := range i.diags {
		b.WriteString(d.Summary() + "\n" + d.Detail() + "\n")
	}
	return b.String()
}

// invoke runs the action against the cluster with set as its configuration,
// cluster_id "c1" unless set says otherwise.
func (a *cancelAPI) invoke(t *testing.T, set map[string]tftypes.Value, timeout time.Duration) invoked {
	t.Helper()
	server := a.serve(t)
	act := &clusterCancelOperationAction{meta: testClusterMeta(t, server.URL, timeout)}
	if set == nil {
		set = map[string]tftypes.Value{}
	}
	if _, ok := set["cluster_id"]; !ok {
		set["cluster_id"] = str("c1")
	}
	var mu sync.Mutex
	var out invoked
	resp := action.InvokeResponse{SendProgress: func(e action.InvokeProgressEvent) {
		mu.Lock()
		defer mu.Unlock()
		out.progress = append(out.progress, e.Message)
	}}
	act.Invoke(context.Background(), action.InvokeRequest{Config: actionConfig(t, act, set)}, &resp)
	out.diags = resp.Diagnostics
	return out
}

func cancelsSent(a *cancelAPI) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.sent)
}

// A cluster mid-operation has that operation cancelled, by the execution id
// the read gave, and the action waits for the cluster to settle and says
// where it got to as it goes.
func TestCancelActionCancelsTheRunningOperation(t *testing.T) {
	for _, from := range []string{"creating", "updating"} {
		t.Run(from, func(t *testing.T) {
			api := &cancelAPI{state: from, execution: "op-7"}
			got := api.invoke(t, nil, time.Minute)
			if got.diags.HasError() {
				t.Fatalf("invoke: %v", got.diags.Errors())
			}
			if len(api.sent) != 1 || api.sent[0] != "op-7" {
				t.Errorf("cancels named %q, want one naming op-7", api.sent)
			}
			if strings.Contains(api.callList(), "delete") {
				t.Errorf("the action deleted something: %q", api.callList())
			}
			if api.state != "failed" {
				t.Errorf("the cluster is %s, want failed", api.state)
			}
			said := got.said()
			for _, want := range []string{from, "op-7", "failed"} {
				if !strings.Contains(said, want) {
					t.Errorf("the action never said %q:\n%s", want, said)
				}
			}
			if len(got.progress) < 2 {
				t.Errorf("want progress before and after the cancel, got %q", got.progress)
			}
		})
	}
}

// Naming the operation pins the cancel to it: the action cancels it when it's
// the one running.
func TestCancelActionCancelsTheOperationItNames(t *testing.T) {
	api := &cancelAPI{state: "updating", execution: "op-7"}
	got := api.invoke(t, map[string]tftypes.Value{"execution_id": str("op-7")}, time.Minute)
	if got.diags.HasError() {
		t.Fatalf("invoke: %v", got.diags.Errors())
	}
	if len(api.sent) != 1 || api.sent[0] != "op-7" {
		t.Errorf("cancels named %q, want one naming op-7", api.sent)
	}
}

// An operation other than the one named is left running: the action refuses
// rather than cancel something nobody asked it to.
func TestCancelActionLeavesAnOperationItWasNotAskedToCancel(t *testing.T) {
	api := &cancelAPI{state: "updating", execution: "op-8"}
	got := api.invoke(t, map[string]tftypes.Value{"execution_id": str("op-7")}, time.Minute)
	if !got.diags.HasError() {
		t.Fatal("cancelling an operation that isn't running reported success")
	}
	if n := cancelsSent(api); n != 0 {
		t.Errorf("sent %d cancels, want none", n)
	}
	if api.state != "updating" {
		t.Errorf("the cluster is %s, want it still updating", api.state)
	}
	said := got.said()
	for _, want := range []string{"op-7", "op-8", "updating", "Nothing was cancelled"} {
		if !strings.Contains(said, want) {
			t.Errorf("the error never said %q:\n%s", want, said)
		}
	}
}

// Nothing in flight is the outcome the action exists for, so it succeeds, and
// warns, because nothing was cancelled.
func TestCancelActionOnASettledClusterHasNothingToCancel(t *testing.T) {
	for _, tc := range []struct {
		state, opStatus, named, want string
	}{
		{"active", "", "", "active"},
		{"failed", "", "", "failed"},
		{"active", "", "e1", "active"},
		{"failed", "cancelled", "", "already cancelled"},
		{"failed", "cancelled", "e1", "already cancelled"},
	} {
		t.Run(tc.state+"/"+tc.opStatus+"/"+tc.named, func(t *testing.T) {
			api := &cancelAPI{state: tc.state, opStatus: tc.opStatus}
			set := map[string]tftypes.Value{}
			if tc.named != "" {
				set["execution_id"] = str(tc.named)
			}
			got := api.invoke(t, set, time.Minute)
			if got.diags.HasError() {
				t.Fatalf("invoke: %v", got.diags.Errors())
			}
			if n := cancelsSent(api); n != 0 {
				t.Errorf("sent %d cancels, want none", n)
			}
			if !hasWarning(got.diags, "Nothing to cancel") || !hasWarning(got.diags, tc.want) {
				t.Errorf("no warning saying there was nothing to cancel and %q: %v", tc.want, got.diags)
			}
		})
	}
}

// The operation finished on its own between the read and the cancel. The
// platform refuses the cancel, and the cluster has nothing in flight, which is
// what was asked for, so the action succeeds and says what happened.
func TestCancelActionWhenTheOperationFinishesFirst(t *testing.T) {
	api := &cancelAPI{state: "creating", settleOnCancel: "active"}
	got := api.invoke(t, nil, time.Minute)
	if got.diags.HasError() {
		t.Fatalf("invoke: %v", got.diags.Errors())
	}
	if !hasWarning(got.diags, "finished before") {
		t.Errorf("no warning that the operation finished first: %v", got.diags)
	}
}

// The cluster began another operation between the read and the cancel. That
// one was never asked about, so it isn't chased the way a destroy chases it:
// the action stops and names it.
func TestCancelActionDoesNotChaseANewOperation(t *testing.T) {
	api := &cancelAPI{state: "updating", moves: 1}
	got := api.invoke(t, nil, time.Minute)
	if !got.diags.HasError() {
		t.Fatal("a cancel refused for a stale operation reported success")
	}
	if got := strings.Join(api.sent, " "); got != "e1" {
		t.Errorf("cancels named %q, want only e1", got)
	}
	said := got.said()
	for _, want := range []string{"e1", "e2", "409", "again"} {
		if !strings.Contains(said, want) {
			t.Errorf("the error never said %q:\n%s", want, said)
		}
	}
}

// A refusal the action can't settle by reading again is the platform's answer,
// with its status and request id.
func TestCancelActionReportsARefusedCancel(t *testing.T) {
	api := &cancelAPI{state: "creating", cancelStatus: http.StatusGatewayTimeout}
	got := api.invoke(t, nil, time.Minute)
	if !got.diags.HasError() {
		t.Fatal("a refused cancel reported success")
	}
	said := got.said()
	for _, want := range []string{"504", "req-cancel-1", "creating"} {
		if !strings.Contains(said, want) {
			t.Errorf("the error never said %q:\n%s", want, said)
		}
	}
}

// A cluster that isn't there is most likely the wrong id or scope, so the
// action fails rather than report nothing to cancel.
func TestCancelActionForAMissingClusterFails(t *testing.T) {
	api := &cancelAPI{state: ""}
	got := api.invoke(t, nil, time.Minute)
	if !got.diags.HasError() {
		t.Fatal("cancelling a cluster that doesn't exist reported success")
	}
	if n := cancelsSent(api); n != 0 {
		t.Errorf("sent %d cancels, want none", n)
	}
	if said := got.said(); !strings.Contains(said, "c1") || !strings.Contains(said, "404") {
		t.Errorf("the error doesn't name the cluster and the 404:\n%s", said)
	}
}

// A delete has no cancel, so a cluster being deleted is refused without a
// request the platform would refuse too.
func TestCancelActionOnADeletingClusterFails(t *testing.T) {
	api := &cancelAPI{state: "deleting"}
	got := api.invoke(t, nil, time.Minute)
	if !got.diags.HasError() {
		t.Fatal("cancelling a delete reported success")
	}
	if n := cancelsSent(api); n != 0 {
		t.Errorf("sent %d cancels, want none", n)
	}
	if said := got.said(); !strings.Contains(said, "deleting") {
		t.Errorf("the error doesn't say the cluster is being deleted:\n%s", said)
	}
}

// No cancel is sent without an id, which the platform would refuse with 400.
func TestCancelActionNamesAMissingOperation(t *testing.T) {
	api := &cancelAPI{state: "creating", noExecution: true}
	got := api.invoke(t, nil, time.Minute)
	if !got.diags.HasError() {
		t.Fatal("a busy cluster with no operation id reported success")
	}
	if n := cancelsSent(api); n != 0 {
		t.Errorf("sent %d cancels, want none", n)
	}
	if said := got.said(); !strings.Contains(said, "operation.executionId") {
		t.Errorf("the error doesn't name the missing field:\n%s", said)
	}
}

// A cluster that never settles after an accepted cancel times the wait out.
// The cancel isn't undone, and the error says it was accepted.
func TestCancelActionTimesOutWaitingForTheClusterToSettle(t *testing.T) {
	api := &cancelAPI{state: "updating", afterCancel: "updating"}
	got := api.invoke(t, nil, 50*time.Millisecond)
	if !got.diags.HasError() {
		t.Fatal("a cluster that never settled reported success")
	}
	said := got.said()
	for _, want := range []string{"accepted", "did not settle", "timeouts"} {
		if !strings.Contains(said, want) {
			t.Errorf("the error never said %q:\n%s", want, said)
		}
	}
}

// The invoke timeout bounds the whole invocation, and a value that isn't a
// duration is refused before anything is sent.
func TestCancelActionTakesItsTimeoutFromTheBlock(t *testing.T) {
	api := &cancelAPI{state: "updating", afterCancel: "updating"}
	timeouts := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"invoke": tftypes.String}}
	started := time.Now()
	got := api.invoke(t, map[string]tftypes.Value{
		"timeouts": tftypes.NewValue(timeouts, map[string]tftypes.Value{"invoke": str("50ms")}),
	}, time.Hour)
	if !got.diags.HasError() {
		t.Fatal("a cluster that never settled reported success")
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("took %s, so the block's 50ms didn't bound it", took)
	}
}

// Planning the action reads the cluster and says what the invoke will stop,
// before the approval, the way a plan that destroys a busy cluster does.
func TestPlanningTheCancelSaysWhatItWillStop(t *testing.T) {
	for _, tc := range []struct {
		state string
		warns bool
	}{
		{"updating", true},
		{"creating", true},
		{"active", false},
		{"failed", false},
	} {
		t.Run(tc.state, func(t *testing.T) {
			api := &cancelAPI{state: tc.state, execution: "op-7"}
			server := api.serve(t)
			act := &clusterCancelOperationAction{meta: testClusterMeta(t, server.URL, time.Minute)}
			cfg := actionConfig(t, act, map[string]tftypes.Value{"cluster_id": str("c1")})
			var resp action.ModifyPlanResponse
			act.ModifyPlan(context.Background(), action.ModifyPlanRequest{Config: cfg}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("plan: %v", resp.Diagnostics.Errors())
			}
			if got := hasWarning(resp.Diagnostics, "op-7"); got != tc.warns {
				t.Errorf("warned about op-7 = %v, want %v: %v", got, tc.warns, resp.Diagnostics)
			}
			if n := cancelsSent(api); n != 0 {
				t.Errorf("the plan sent %d cancels", n)
			}
		})
	}
}

// A cluster id that isn't known until apply, such as one a resource in the
// same run creates, leaves the plan nothing to read, so it sends nothing.
func TestPlanningTheCancelOfAnUnknownClusterReadsNothing(t *testing.T) {
	act := &clusterCancelOperationAction{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)}
	cfg := actionConfig(t, act, map[string]tftypes.Value{"cluster_id": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)})
	var resp action.ModifyPlanResponse
	act.ModifyPlan(context.Background(), action.ModifyPlanRequest{Config: cfg}, &resp)
	if len(resp.Diagnostics) != 0 {
		t.Errorf("planned an unknown cluster with diagnostics: %v", resp.Diagnostics)
	}
}

// Left unpinned, the invoke cancels whatever is running when it runs, which
// may not be the operation the plan read. The warning says so and shows how
// to pin it. Pinned, it names the one operation it can cancel.
func TestPlanningAnUnpinnedCancelSaysItCancelsWhateverRuns(t *testing.T) {
	for _, tc := range []struct {
		pinned bool
		want   []string
		not    string
	}{
		{false, []string{"whatever", `execution_id = "op-7"`}, ""},
		{true, []string{"op-7"}, "whatever"},
	} {
		api := &cancelAPI{state: "updating", execution: "op-7"}
		server := api.serve(t)
		act := &clusterCancelOperationAction{meta: testClusterMeta(t, server.URL, time.Minute)}
		set := map[string]tftypes.Value{"cluster_id": str("c1")}
		if tc.pinned {
			set["execution_id"] = str("op-7")
		}
		var resp action.ModifyPlanResponse
		act.ModifyPlan(context.Background(), action.ModifyPlanRequest{Config: actionConfig(t, act, set)}, &resp)
		said := invoked{diags: resp.Diagnostics}.said()
		for _, w := range tc.want {
			if !strings.Contains(said, w) {
				t.Errorf("pinned=%v: the plan never said %q:\n%s", tc.pinned, w, said)
			}
		}
		if tc.not != "" && strings.Contains(said, tc.not) {
			t.Errorf("pinned=%v: the plan said %q:\n%s", tc.pinned, tc.not, said)
		}
	}
}

// What a refused cancel advises depends on what refused it. Only a 5xx can
// have landed, so only it says to read the cluster before trying again.
func TestCancelActionRefusalAdviceFitsTheStatus(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   []string
		not    []string
	}{
		{http.StatusBadRequest, []string{"Nothing was cancelled", "malformed"}, []string{"may have landed"}},
		{http.StatusForbidden, []string{"Nothing was cancelled", "role"}, []string{"may have landed"}},
		{http.StatusConflict, []string{"Nothing was cancelled", "Read the cluster"}, []string{"may have landed"}},
		{http.StatusInternalServerError, []string{"may have landed", "Read the cluster", "already cancelled"}, []string{"as it was"}},
		{http.StatusGatewayTimeout, []string{"may have landed", "Read the cluster", "already cancelled"}, []string{"as it was"}},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			api := &cancelAPI{state: "creating", cancelStatus: tc.status}
			got := api.invoke(t, nil, time.Minute)
			if !got.diags.HasError() {
				t.Fatal("a refused cancel reported success")
			}
			said := got.said()
			for _, w := range tc.want {
				if !strings.Contains(said, w) {
					t.Errorf("the error never said %q:\n%s", w, said)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(said, w) {
					t.Errorf("the error said %q:\n%s", w, said)
				}
			}
		})
	}
}

// A cancel cut off mid-request, by Ctrl-C or the invoke timeout, may have
// reached the platform, so the error says to read the cluster first.
func TestCancelActionCutOffMidCancelSaysItMayHaveLanded(t *testing.T) {
	api := &cancelAPI{state: "updating", cancelDelay: time.Second}
	timeouts := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"invoke": tftypes.String}}
	got := api.invoke(t, map[string]tftypes.Value{
		"timeouts": tftypes.NewValue(timeouts, map[string]tftypes.Value{"invoke": str("100ms")}),
	}, time.Minute)
	if !got.diags.HasError() {
		t.Fatal("a cancel cut off mid-request reported success")
	}
	said := got.said()
	for _, w := range []string{"may have landed", "Read the cluster"} {
		if !strings.Contains(said, w) {
			t.Errorf("the error never said %q:\n%s", w, said)
		}
	}
}

// After an accepted cancel the cluster may go before it settles. A delete
// started elsewhere is warned about, since the action doesn't stop it, and a
// cluster that's gone is said to be gone, not to have nothing in flight.
func TestCancelActionSaysWhereTheClusterWentAfterTheCancel(t *testing.T) {
	for _, tc := range []struct {
		after, last string
		warns       bool
	}{
		{"deleting", "being deleted", true},
		{"gone", "is gone", false},
	} {
		t.Run(tc.after, func(t *testing.T) {
			api := &cancelAPI{state: "updating", afterCancel: tc.after}
			got := api.invoke(t, nil, time.Minute)
			if got.diags.HasError() {
				t.Fatalf("invoke: %v", got.diags.Errors())
			}
			last := got.progress[len(got.progress)-1]
			if !strings.Contains(last, tc.last) || strings.Contains(last, "nothing in flight") {
				t.Errorf("the last progress line is %q, want it to say %q", last, tc.last)
			}
			if warned := hasWarning(got.diags, "being deleted"); warned != tc.warns {
				t.Errorf("warned about the delete = %v, want %v: %v", warned, tc.warns, got.diags)
			}
		})
	}
}

// The action says how an accepted cancel ended, from the operation the
// settled cluster reports. Only `cancelled` says it cancelled anything, and
// every other end succeeds with a warning that says what happened instead.
func TestTheCancelActionSaysHowTheCancelEnded(t *testing.T) {
	was := clusterNow{found: true, state: k0rdent.ClusterStateCreating, execution: "e1",
		status: k0rdent.OperationStatusRunning}
	settled := func(state k0rdent.ClusterState, execution string, status k0rdent.OperationStatus) clusterNow {
		return clusterNow{found: true, state: state, execution: execution, status: status}
	}
	for _, tc := range []struct {
		name     string
		after    clusterNow
		progress string
		warning  string // "" for none
	}{
		{"cancelled", settled(k0rdent.ClusterStateFailed, "e1", k0rdent.OperationStatusCancelled),
			"Cancelled operation e1. Cluster c1 is failed, with nothing in flight.", ""},
		{"finished anyway", settled(k0rdent.ClusterStateActive, "e1", k0rdent.OperationStatusSucceeded),
			"Operation e1 finished before the cancel took effect. Cluster c1 is active.", "reads `succeeded`"},
		{"failed first", settled(k0rdent.ClusterStateFailed, "e1", k0rdent.OperationStatusFailed),
			"Operation e1 failed before the cancel took effect. Cluster c1 is failed.", "reads `failed`"},
		{"another operation", settled(k0rdent.ClusterStateActive, "e2", k0rdent.OperationStatusSucceeded),
			"Cluster c1 is active.", "operation `e2` reading `succeeded`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var diags diag.Diagnostics
			var said []string
			run := cancelRun{id: "c1", diags: &diags,
				send: func(e action.InvokeProgressEvent) { said = append(said, e.Message) }}
			run.ended(context.Background(), was, tc.after)
			if len(said) != 1 || said[0] != tc.progress {
				t.Errorf("progress = %q, want %q", said, tc.progress)
			}
			if diags.HasError() {
				t.Fatalf("an accepted cancel failed the action: %v", diags)
			}
			switch {
			case tc.warning == "" && len(diags.Warnings()) > 0:
				t.Errorf("warned on a cancel that landed: %v", diags.Warnings())
			case tc.warning != "" && (len(diags.Warnings()) != 1 || !strings.Contains(diags.Warnings()[0].Detail(), tc.warning)):
				t.Errorf("the warning doesn't say %q: %v", tc.warning, diags.Warnings())
			case tc.warning != "" && tc.name != "another operation" &&
				!strings.Contains(diags.Warnings()[0].Detail(), "Nothing was cancelled"):
				t.Errorf("the warning doesn't say nothing was cancelled: %v", diags.Warnings())
			}
		})
	}
}
