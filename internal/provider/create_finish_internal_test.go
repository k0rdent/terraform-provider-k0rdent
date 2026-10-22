package provider

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// finishModel stands in for a nested kind's model: an id, a parent, the
// scope, one configured value and one only the read back fills in.
type finishModel struct {
	ID      types.String `tfsdk:"id"`
	Network types.String `tfsdk:"network"`
	Region  types.String `tfsdk:"region"`
	Project types.String `tfsdk:"project"`
	Name    types.String `tfsdk:"name"`
	State   types.String `tfsdk:"state"`
}

var finishSchema = schema.Schema{Attributes: map[string]schema.Attribute{
	"id":      schema.StringAttribute{Optional: true, Computed: true},
	"network": schema.StringAttribute{Required: true},
	"region":  schema.StringAttribute{Optional: true, Computed: true},
	"project": schema.StringAttribute{Optional: true, Computed: true},
	"name":    schema.StringAttribute{Required: true},
	"state":   schema.StringAttribute{Computed: true},
}}

// finishPlan is a plan with the id, the scope and the read only value
// unknown, as Terraform hands Create one.
func finishPlan(t *testing.T) tfsdk.Plan {
	t.Helper()
	object, ok := finishSchema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema is not an object")
	}
	unknown := tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
	return tfsdk.Plan{Schema: finishSchema, Raw: tftypes.NewValue(object, map[string]tftypes.Value{
		"id": unknown, "region": unknown, "project": unknown, "state": unknown,
		"network": tftypes.NewValue(tftypes.String, "n1"),
		"name":    tftypes.NewValue(tftypes.String, "web"),
	})}
}

// finishCase is one create, from its first send to its finished state.
type finishCase struct {
	// answers are what each send gets, in order. The last repeats.
	answers []createAnswer
	// lookup is what reading the id back finds.
	lookup createLookup
	// waitErr is what the wait returns.
	waitErr error
	// readFound is false for a read back that answers 404.
	readFound bool
	// readError is the summary of an error the read back reports, empty for
	// none.
	readError string

	wantOK    bool
	wantSends int
	wantWaits int
	wantReads int
	// wantError is a substring of the first error's summary, empty for none.
	wantError string
	// wantWarning is a substring of a warning's summary, empty for none.
	wantWarning string
	// wantState is the state attribute values to check, "" for null.
	wantState map[string]string
	// wantNullState says nothing at all may be written.
	wantNullState bool
}

func answerCreated() createAnswer {
	return createAnswer{resp: &http.Response{StatusCode: http.StatusCreated, Status: "201 Created"}}
}

func TestCreateFinish(t *testing.T) {
	lost := createAnswer{err: errors.New("connection reset by peer")}
	readFailed := createLookup{resp: &http.Response{StatusCode: http.StatusInternalServerError, Status: "500 Internal Server Error"}}
	finished := map[string]string{"network": "n1", "region": "test", "project": "elsewhere", "name": "web", "state": "active"}
	// The plan, as for a lost answer, so a refresh after `terraform untaint`
	// reads the resource against what was configured.
	waitFailedRow := map[string]string{"network": "n1", "region": "test", "project": "elsewhere", "name": "web", "state": ""}

	for name, tc := range map[string]finishCase{
		"created, settled and read back": {
			answers: []createAnswer{answerCreated()}, readFound: true,
			wantOK: true, wantSends: 1, wantWaits: 1, wantReads: 1,
			wantState: finished,
		},
		// The row a failed wait leaves is what keeps the resource from being
		// orphaned: it carries the id, the scope and the parent, so the next
		// refresh reads it where it is.
		"wait fails": {
			answers: []createAnswer{answerCreated()}, waitErr: errors.New("the platform reports state failed"),
			wantSends: 1, wantWaits: 1,
			wantError: "The thing did not become active",
			wantState: waitFailedRow,
		},
		"read back answers 404": {
			answers: []createAnswer{answerCreated()}, readFound: false,
			wantSends: 1, wantWaits: 1, wantReads: 1,
			wantError: "The new thing could not be read back",
			wantState: waitFailedRow,
		},
		// The read's own error is the whole story: it is not also reported as
		// a 404.
		"read back fails": {
			answers: []createAnswer{answerCreated()}, readError: "Could not read the thing",
			wantSends: 1, wantWaits: 1, wantReads: 1,
			wantError: "Could not read the thing",
			wantState: waitFailedRow,
		},
		"lost answer, then the lookup finds it": {
			answers: []createAnswer{lost}, lookup: createLookup{found: true}, readFound: true,
			wantOK: true, wantSends: 1, wantWaits: 1, wantReads: 1,
			wantWarning: "was created, though the create did not say so",
			wantState:   finished,
		},
		"lost answer, the lookup finds nothing, and the resend lands": {
			answers: []createAnswer{lost, answerCreated()}, readFound: true,
			wantOK: true, wantSends: 2, wantWaits: 1, wantReads: 1,
			wantState: finished,
		},
		// The plan goes to state under the generated id, and nothing is waited
		// for: there may be nothing to wait for.
		"lost answer that a read cannot settle": {
			answers: []createAnswer{lost}, lookup: readFailed,
			wantSends: 1,
			wantError: "Could not confirm the thing was created",
			wantState: map[string]string{"network": "n1", "region": "test", "project": "elsewhere", "name": "web", "state": ""},
		},
		"refused": {
			answers:   []createAnswer{{resp: &http.Response{StatusCode: http.StatusBadRequest, Status: "400 Bad Request"}}},
			wantSends: 1, wantError: "Could not create the thing", wantNullState: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			key, err := newCreateKey(types.StringNull(), false)
			if err != nil {
				t.Fatal(err)
			}
			var sends, waits, reads int
			create := convergingCreate{
				key: key, kind: "thing", what: "create the thing", resourceName: ResourceName("thing"),
				importID: func(id string) string { return id },
				send: func(context.Context) createAnswer {
					a := tc.answers[min(sends, len(tc.answers)-1)]
					sends++
					return a
				},
				lookup: func(context.Context, string) createLookup { return tc.lookup },
			}

			plan := finishPlan(t)
			state := tfsdk.State{Schema: plan.Schema, Raw: tftypes.NewValue(plan.Raw.Type(), nil)}
			var diags diag.Diagnostics
			got, ok := createFinish[finishModel]{
				operation:  "createThing",
				target:     clusterTarget{region: "test", project: "elsewhere"},
				parentAttr: "network", parentID: "n1",
				wait: func(_ context.Context, waited string) error {
					waits++
					if waited != key.id {
						t.Errorf("waited for %q, want %q", waited, key.id)
					}
					return tc.waitErr
				},
				read: func(_ context.Context, read string) (finishModel, bool, diag.Diagnostics) {
					reads++
					var readDiags diag.Diagnostics
					if tc.readError != "" {
						readDiags.AddError(tc.readError, "The platform answered 500.")
					}
					return finishModel{
						ID: types.StringValue(read), Network: types.StringValue("n1"),
						Region: types.StringValue("test"), Project: types.StringValue("elsewhere"),
						Name: types.StringValue("web"), State: types.StringValue("active"),
					}, tc.readFound, readDiags
				},
			}.run(ctx, create, plan, &state, &diags)

			if ok != tc.wantOK {
				t.Errorf("ok = %t, want %t: %v", ok, tc.wantOK, diags)
			}
			if ok && got.ID.ValueString() != key.id {
				t.Errorf("returned id = %s, want %q", got.ID, key.id)
			}
			if sends != tc.wantSends || waits != tc.wantWaits || reads != tc.wantReads {
				t.Errorf("sends, waits, reads = %d, %d, %d, want %d, %d, %d",
					sends, waits, reads, tc.wantSends, tc.wantWaits, tc.wantReads)
			}
			assertFirstError(t, diags, tc.wantError)
			if notFound := "could not be read back"; !strings.Contains(tc.wantError, notFound) {
				for _, e := range diags.Errors() {
					if strings.Contains(e.Summary(), notFound) {
						t.Errorf("a read back that did not answer 404 was reported as one: %q", e.Summary())
					}
				}
			}
			if tc.wantWarning != "" && !hasWarning(diags, tc.wantWarning) {
				t.Errorf("no warning saying %q in %v", tc.wantWarning, diags.Warnings())
			}
			if tc.wantNullState {
				if !state.Raw.IsNull() {
					t.Errorf("state was written for a create that failed: %v", state.Raw)
				}
				return
			}
			// A created resource is never lost from state: every way out
			// after the create carries its id.
			if got := stateID(t, state); got != key.id {
				t.Errorf("id in state = %q, want the generated %q", got, key.id)
			}
			for attr, want := range tc.wantState {
				var v types.String
				if d := state.GetAttribute(ctx, path.Root(attr), &v); d.HasError() {
					t.Errorf("reading %s from state: %v", attr, d)
					continue
				}
				switch {
				case want == "" && !v.IsNull():
					t.Errorf("%s in state = %s, want null", attr, v)
				case want != "" && v.ValueString() != want:
					t.Errorf("%s in state = %s, want %q", attr, v, want)
				}
			}
		})
	}
}

// A kind can name its own wait failure and say more about it, as the instance
// group does about a node that failed for want of SSH access.
func TestCreateFinishReportsAWaitFailureTheKindsWay(t *testing.T) {
	plan := finishPlan(t)
	state := tfsdk.State{Schema: plan.Schema, Raw: tftypes.NewValue(plan.Raw.Type(), nil)}
	var diags diag.Diagnostics
	_, ok := createFinish[finishModel]{
		operation: "createThing", target: clusterTarget{region: "test", project: "elsewhere"},
		parentAttr: "network", parentID: "n1",
		wait: func(context.Context, string) error {
			return errors.New("the platform reports the thing as failed")
		},
		waitSummary: "This side of the thing did not become active",
		reportWait: func(diags *diag.Diagnostics, summary string, err error) {
			addAdvisedError(diags, summary, "Check its SSH access.", err)
		},
		read: func(context.Context, string) (finishModel, bool, diag.Diagnostics) {
			t.Error("read after a failed wait")
			return finishModel{}, false, nil
		},
	}.run(context.Background(), convergingCreate{
		key: createKey{id: "t1"}, kind: "thing", what: "create the thing",
		send: func(context.Context) createAnswer {
			a := answerCreated()
			a.id = "t1"
			return a
		},
	}, plan, &state, &diags)
	if ok {
		t.Fatal("a failed wait finished")
	}
	assertFirstError(t, diags, "This side of the thing did not become active")
	if d := diags.Errors()[0].Detail(); !strings.Contains(d, "Check its SSH access.") {
		t.Errorf("the kind's advice is missing from %q", d)
	}
	if got := stateID(t, state); got != "t1" {
		t.Errorf("id in state = %q, want t1", got)
	}
}

// A wait that stops while the platform is still building, on its timeout or
// on an interrupt, fails the create so no pipeline reads the object as ready.
// The object stays in state, and the error says it's still running and gives
// the untaint and apply that wait for it. A kind's own failure wording isn't
// used for it.
func TestCreateFinishReportsACreateStillRunning(t *testing.T) {
	for name, stopped := range map[string]stoppedWaiting{
		"timed out":   {what: "the thing to finish creating", waited: time.Hour, last: "creating"},
		"interrupted": {what: "the thing to finish creating", waited: time.Minute, last: "creating", interrupted: true},
	} {
		t.Run(name, func(t *testing.T) {
			plan := finishPlan(t)
			state := tfsdk.State{Schema: plan.Schema, Raw: tftypes.NewValue(plan.Raw.Type(), nil)}
			var diags diag.Diagnostics
			_, ok := createFinish[finishModel]{
				operation: "createThing", target: clusterTarget{region: "test", project: "elsewhere"},
				parentAttr: "network", parentID: "n1",
				wait: func(context.Context, string) error { return stopped },
				reportWait: func(*diag.Diagnostics, string, error) {
					t.Error("a create still running was reported as a failed wait")
				},
				read: func(context.Context, string) (finishModel, bool, diag.Diagnostics) {
					t.Error("read after a wait that stopped")
					return finishModel{}, false, nil
				},
			}.run(context.Background(), convergingCreate{
				key: createKey{id: "t1"}, kind: "thing", what: "create the thing", resourceName: ResourceName("thing"),
				send: func(context.Context) createAnswer {
					a := answerCreated()
					a.id = "t1"
					return a
				},
			}, plan, &state, &diags)
			if ok {
				t.Fatal("a create still running finished")
			}
			if errs := diags.Errors(); len(errs) != 1 {
				t.Fatalf("errors = %v, want one, so the apply exits 1", errs)
			}
			e := diags.Errors()[0]
			text := e.Summary() + "\n" + e.Detail()
			for _, want := range []string{"The thing is still being created", "still `creating`",
				"still be running", "tainted", "terraform untaint <address>\n    terraform apply",
				ResourceName("thing") + ".example", "Don't run `terraform apply` first"} {
				if !strings.Contains(text, want) {
					t.Errorf("the error does not say %q:\n%s", want, text)
				}
			}
			if raise := strings.Contains(text, "`timeouts`"); raise == stopped.interrupted {
				t.Errorf("the error names the timeout = %v, want %v:\n%s", raise, !stopped.interrupted, text)
			}
			if got := stateID(t, state); got != "t1" {
				t.Errorf("id in state = %q, want t1", got)
			}
		})
	}
}

func assertFirstError(t *testing.T, diags diag.Diagnostics, want string) {
	t.Helper()
	switch errs := diags.Errors(); {
	case want == "" && len(errs) > 0:
		t.Errorf("unexpected errors: %v", errs)
	case want != "" && len(errs) == 0:
		t.Errorf("no error, want one saying %q", want)
	case want != "" && !strings.Contains(errs[0].Summary(), want):
		t.Errorf("first error = %q, want one saying %q", errs[0].Summary(), want)
	}
}
