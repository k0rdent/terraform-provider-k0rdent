package provider

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// widget is a made-up body, so these tests are about the module and not about
// any one kind's policy. The kinds' own tables are pinned in
// settle_states_internal_test.go.
type widget struct {
	state  *string
	reason string
	ready  bool
	broken error
}

func widgetIn(state string) widget { return widget{state: &state} }

var widgets = stateKind[widget]{
	name: "widget", subject: "this widget",
	state: func(w widget) (string, bool) {
		if w.state == nil {
			return "", false
		}
		return *w.state, true
	},
	detail: func(w widget) string { return stateDetail(nil, w.reason) },
	ready:  func(w widget) (bool, error) { return w.ready, w.broken },
	settle: stateTable{
		missing: stateRule{verdict: settled},
		states: map[string]stateRule{
			"active":   {verdict: settledOnceReady},
			"creating": {verdict: keepWaiting},
			"wobbly":   {verdict: failsProvisionally, detail: true},
			"failed":   {verdict: failsTerminally, detail: true},
			"deleted":  {verdict: failsTerminally},
			"active!":  {verdict: failsTerminally, says: afterDelete, detail: true},
		},
	},
}

func TestDecideReadsTheTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body widget
		want verdict
		says string
	}{
		{"no state takes the missing rule", widget{}, settled, ""},
		{"an empty state is a state, not a missing one", widgetIn(""), unrecognised,
			unknownStateError("widget", "").Error()},
		{"a state the table leaves out", widgetIn("quiescing"), unrecognised,
			unknownStateError("widget", "quiescing").Error()},
		{"waiting", widgetIn("creating"), keepWaiting, ""},
		{"a failure names the subject and adds the detail",
			widget{state: widgetIn("failed").state, reason: "INTERNAL"}, failsTerminally,
			"the platform reports this widget as failed: reason INTERNAL"},
		{"a failure without detail leaves it off",
			widget{state: widgetIn("deleted").state, reason: "INTERNAL"}, failsTerminally,
			"the platform reports this widget as deleted"},
		{"a provisional failure", widgetIn("wobbly"), failsProvisionally,
			"the platform reports this widget as wobbly"},
		{"a rule's own wording", widgetIn("active!"), failsTerminally,
			`the platform reports this widget as "active!" after the delete was accepted`},
		{"ready", widget{state: widgetIn("active").state, ready: true}, settled, ""},
		{"not ready yet", widgetIn("active"), keepWaiting, ""},
		{"a ready check that fails", widget{state: widgetIn("active").state, broken: errors.New("no")},
			failsTerminally, "no"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := widgets.decide(widgets.settle, tt.body)
			if got != tt.want {
				t.Errorf("verdict = %d, want %d", got, tt.want)
			}
			says := ""
			if err != nil {
				says = err.Error()
			}
			if says != tt.says {
				t.Errorf("error = %q, want %q", says, tt.says)
			}
		})
	}
}

// reads serves one body after another and then repeats the last. A nil body
// is the service's 404 for a missing widget.
func reads(bodies ...*widget) stateRead[widget] {
	n := 0
	return func(context.Context) (*widget, *http.Response, []byte, error) {
		b := bodies[min(n, len(bodies)-1)]
		n++
		if b == nil {
			return nil, &http.Response{StatusCode: http.StatusNotFound, Header: http.Header{}},
				[]byte(`{"error":{"code":404,"message":"not found"}}`), nil
		}
		return b, &http.Response{StatusCode: http.StatusOK, Header: http.Header{}}, nil, nil
	}
}

var quickWait = waitConfig{Timeout: time.Second, Interval: time.Microsecond}

func TestAwaitSettledPollsUntilTheTableSaysDone(t *testing.T) {
	t.Parallel()
	creating, active := widgetIn("creating"), widget{state: widgetIn("active").state, ready: true}
	if err := widgets.awaitSettled(context.Background(), quickWait, "the widget",
		reads(&creating, &creating, &active)); err != nil {
		t.Fatalf("awaitSettled: %v", err)
	}
}

// A settle has nothing to wait for once the resource is gone.
func TestAwaitSettledReportsAMissingResource(t *testing.T) {
	t.Parallel()
	err := widgets.awaitSettled(context.Background(), quickWait, "the widget", reads(nil))
	if err == nil || !strings.HasPrefix(err.Error(), "reading the widget while waiting: ") {
		t.Fatalf("err = %v, want the failed read", err)
	}
}

func TestAwaitDeletedEndsOnA404(t *testing.T) {
	t.Parallel()
	creating := widgetIn("creating")
	k := widgets
	k.delete = widgets.settle
	if err := k.awaitDeleted(context.Background(), quickWait, "the widget", reads(&creating, nil)); err != nil {
		t.Fatalf("awaitDeleted: %v", err)
	}
}

// A provisional failure reaches waitFor as one, so it is held for the window
// and a recovery inside it is not reported.
func TestAProvisionalVerdictIsHeldForTheWindow(t *testing.T) {
	t.Parallel()
	wobbly, active := widgetIn("wobbly"), widget{state: widgetIn("active").state, ready: true}
	cfg := quickWait
	cfg.ProvisionalFailureWindow = time.Minute
	if err := widgets.awaitSettled(context.Background(), cfg, "the widget",
		reads(&wobbly, &wobbly, &active)); err != nil {
		t.Fatalf("a recovery inside the window was reported: %v", err)
	}

	cfg.ProvisionalFailureWindow = 0
	err := widgets.awaitSettled(context.Background(), cfg, "the widget", reads(&wobbly))
	if err == nil || !strings.Contains(err.Error(), "and stayed that way for") {
		t.Fatalf("err = %v, want the failure once the window has passed", err)
	}
}

// deleting is widgets with a delete table and the state a running delete
// reads, as a kind a resource deletes has.
func deletingWidgets() stateKind[widget] {
	k := widgets
	k.resource, k.deleting = "widget", "deleting"
	k.delete = stateTable{states: map[string]stateRule{
		"deleting": {verdict: keepWaiting},
		"failed":   {verdict: failsTerminally, says: afterDelete},
	}}
	return k
}

// answers is a send that answers status with a contract envelope.
func answers(status int, sends *int) func(context.Context) (*http.Response, []byte, error) {
	return func(context.Context) (*http.Response, []byte, error) {
		*sends++
		var body []byte
		if status >= 300 {
			body = []byte(`{"error":{"code":` + strconv.Itoa(status) + `,"message":"no"}}`)
		}
		return &http.Response{StatusCode: status, Header: http.Header{}}, body, nil
	}
}

// A destroy sends the delete, waits for the 404, and reports nothing.
func TestDestroyWaitsForTheDeleteItStarted(t *testing.T) {
	t.Parallel()
	active, deleting := widgetIn("active"), widgetIn("deleting")
	sends := 0
	var diags diag.Diagnostics
	got := deletingWidgets().destroy(context.Background(), quickWait, answers(http.StatusAccepted, &sends),
		reads(&active, &deleting, nil), &diags)
	if got != deleteAccepted || sends != 1 || diags.HasError() {
		t.Fatalf("destroy = %d, sends %d, diags %v", got, sends, diags)
	}
}

// A refusal is reported once, and nothing is waited for.
func TestDestroyReportsARefusalWithoutWaiting(t *testing.T) {
	t.Parallel()
	active := widgetIn("active")
	sends := 0
	var diags diag.Diagnostics
	got := deletingWidgets().destroy(context.Background(), quickWait, answers(http.StatusConflict, &sends),
		reads(&active), &diags)
	if got != deleteRefused || sends != 1 || len(diags.Errors()) != 1 {
		t.Fatalf("destroy = %d, sends %d, diags %v", got, sends, diags)
	}
	if summary := diags.Errors()[0].Summary(); strings.Contains(summary, "did not finish deleting") {
		t.Errorf("a refused delete reads as a wait: %q", summary)
	}
}

// A wait the platform ends is reported in the kind's own words, and its
// advice names the kind's resource in the removed block.
func TestDestroyReportsAFailedDeleteInTheKindsWords(t *testing.T) {
	t.Parallel()
	active, failed := widgetIn("active"), widgetIn("failed")
	sends := 0
	var diags diag.Diagnostics
	deletingWidgets().destroy(context.Background(), quickWait, answers(http.StatusAccepted, &sends),
		reads(&active, &failed), &diags)
	text := diagText(diags)
	for _, want := range []string{"The widget did not finish deleting", deleteRefusedAdvice, ResourceName("widget")} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostic lacks %q:\n%s", want, text)
		}
	}
}

// A failed delete's `removed` advice names the resource that owns the kind,
// as that resource names itself to Terraform.
func TestEveryKindADestroyReportsNamesItsResource(t *testing.T) {
	for _, k := range []struct {
		kind, suffix string
		owner        resource.Resource
	}{
		{clusterStates.name, clusterStates.resource, &clusterResource{}},
		{instanceGroupStates.name, instanceGroupStates.resource, &instanceGroupResource{}},
		{fileStoreStates.name, fileStoreStates.resource, &fileStoreResource{}},
		{networkStates.name, networkStates.resource, &networkResource{}},
		{projectStates.name, projectStates.resource, &projectResource{}},
		{securityGroupStates.name, securityGroupStates.resource, &securityGroupResource{}},
		{subnetStates.name, subnetStates.resource, &subnetResource{}},
		{vpcPeeringStates.name, vpcPeeringStates.resource, &vpcPeeringResource{}},
	} {
		var md resource.MetadataResponse
		k.owner.Metadata(context.Background(), resource.MetadataRequest{}, &md)
		if got := ResourceName(k.suffix); got != md.TypeName {
			t.Errorf("the %s kind names %q, want %q", k.kind, got, md.TypeName)
		}
	}
}
