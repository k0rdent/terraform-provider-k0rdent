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

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// A create or delete that outlives the run waiting for it is picked up by the
// next run, and never sent twice. These go through the plugin protocol,
// because the marker that carries a create from one run to the next lives in
// private state, which the framework carries, and because what Terraform does
// with an answer (taint it, hold it to the plan) is decided on what crosses
// the protocol.

// slowPlatform serves one object of a kind that waits. It accepts the create
// and reads `creating` until settle is called, then `active`. A delete puts
// it `deleting` until reap is called, then it's gone. A create's tags, the
// create token among them, are kept and read back, as the platform does.
// Every request is recorded, and interrupt, when set, is called on the next
// read.
type slowPlatform struct {
	t         *testing.T
	id        string
	body      map[string]any
	mu        sync.Mutex
	state     string // "" before the create and after the reap
	requests  []string
	interrupt func()
}

func (p *slowPlatform) settle() { p.set("active") }
func (p *slowPlatform) reap()   { p.set("") }

func (p *slowPlatform) set(state string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.state = state
}

func (p *slowPlatform) serve() *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/token") {
			writeJSON(w, map[string]any{"access_token": "t", "token_type": "Bearer", "expires_in": 900})
			return
		}
		p.mu.Lock()
		p.requests = append(p.requests, r.Method+" "+r.URL.Path)
		var interrupt func()
		if r.Method == http.MethodGet {
			interrupt, p.interrupt = p.interrupt, nil
		}
		ours := strings.HasSuffix(r.URL.Path, "/"+p.id)
		switch {
		case r.Method == http.MethodPost && !strings.HasSuffix(r.URL.Path, "/cancel"):
			p.state = "creating"
			var sent struct {
				Tags map[string]any `json:"tags"`
			}
			if json.NewDecoder(r.Body).Decode(&sent) == nil && sent.Tags != nil {
				p.body["tags"] = sent.Tags
			}
		case r.Method == http.MethodDelete && ours && p.state != "":
			p.state = "deleting"
		}
		state := p.state
		p.mu.Unlock()

		if interrupt != nil && r.Method == http.MethodGet {
			interrupt()
		}
		body := map[string]any{}
		for k, v := range p.body {
			body[k] = v
		}
		body["state"] = state
		switch {
		case r.Method == http.MethodPost:
			// The binding's POST answers 202, every create 201.
			status := http.StatusCreated
			if strings.Contains(r.URL.Path, "/networking/vpcs/") && !strings.Contains(r.URL.Path, "/peerings") {
				status = http.StatusAccepted
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if err := json.NewEncoder(w).Encode(body); err != nil {
				p.t.Errorf("write: %v", err)
			}
		case r.Method == http.MethodDelete && state != "":
			w.WriteHeader(http.StatusNoContent)
		case ours && state != "":
			writeJSON(w, body)
		case ours, r.Method != http.MethodGet:
			writeTestError(p.t, w, http.StatusNotFound, "not found")
		default:
			// A list a read or a wait consults on the side, such as the
			// cluster's VPCs or its runtime events: empty.
			writeJSON(w, map[string]any{"items": []any{}})
		}
	}))
	p.t.Cleanup(server.Close)
	return server
}

// sent counts the requests whose method and path match.
func (p *slowPlatform) sent(method, suffix string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for _, r := range p.requests {
		if strings.HasPrefix(r, method+" ") && strings.HasSuffix(r, suffix) {
			n++
		}
	}
	return n
}

// protocolRun is one configured provider and one resource type, driven the
// way Terraform drives it.
type protocolRun struct {
	t        *testing.T
	ps       tfprotov6.ProviderServer
	typeName string
	obj      tftypes.Object
	config   tftypes.Value
}

func newProtocolRun(t *testing.T, endpoint, suffix string, set map[string]tftypes.Value, timeouts map[string]string) *protocolRun {
	t.Helper()
	ctx := context.Background()
	p := &aiProvider{version: "test", lookupEnv: envMap(saEnv(nil))}
	ps, err := providerserver.NewProtocol6WithError(p)()
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := ps.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	providerConfig := nullObjectWith(t, schemas.Provider.ValueType(), map[string]string{
		"endpoint": endpoint, "region": "test", "project": "test", "poll_interval": "5ms",
	})
	configured, err := ps.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: providerConfig})
	if err != nil || len(diagnosticErrors(configured.Diagnostics)) > 0 {
		t.Fatalf("configure: %v %v", err, diagnosticErrors(configured.Diagnostics))
	}
	typeName := ResourceName(suffix)
	obj, ok := schemas.ResourceSchemas[typeName].ValueType().(tftypes.Object)
	if !ok {
		t.Fatalf("%s isn't an object", typeName)
	}
	values := map[string]tftypes.Value{}
	for name, at := range obj.AttributeTypes {
		values[name] = tftypes.NewValue(at, nil)
	}
	for name, v := range set {
		values[name] = v
	}
	timeoutsType, ok := obj.AttributeTypes["timeouts"].(tftypes.Object)
	if !ok {
		t.Fatalf("%s has no timeouts block", typeName)
	}
	limits := map[string]tftypes.Value{}
	for name, at := range timeoutsType.AttributeTypes {
		limits[name] = tftypes.NewValue(at, nil)
		if d, set := timeouts[name]; set {
			limits[name] = tftypes.NewValue(at, d)
		}
	}
	values["timeouts"] = tftypes.NewValue(timeoutsType, limits)
	return &protocolRun{t: t, ps: ps, typeName: typeName, obj: obj, config: tftypes.NewValue(obj, values)}
}

func (r *protocolRun) dynamic(v tftypes.Value) *tfprotov6.DynamicValue {
	r.t.Helper()
	dv, err := tfprotov6.NewDynamicValue(r.obj, v)
	if err != nil {
		r.t.Fatal(err)
	}
	return &dv
}

func (r *protocolRun) value(dv *tfprotov6.DynamicValue) tftypes.Value {
	r.t.Helper()
	if dv == nil {
		return tftypes.NewValue(r.obj, nil)
	}
	v, err := dv.Unmarshal(r.obj)
	if err != nil {
		r.t.Fatal(err)
	}
	return v
}

// proposed is the new state Terraform proposes to a plan: the configuration,
// with prior state for whatever the configuration leaves null.
func (r *protocolRun) proposed(prior tftypes.Value) tftypes.Value {
	if prior.IsNull() {
		return r.config
	}
	return proposedValue(r.t, r.config, prior)
}

// proposedValue merges one object: an attribute the configuration leaves null
// takes prior state's value, and a nested object both set is merged the same
// way, as Terraform does for the computed attributes inside it.
func proposedValue(t *testing.T, config, prior tftypes.Value) tftypes.Value {
	t.Helper()
	if prior.IsNull() || !prior.IsKnown() {
		return config
	}
	var set, state map[string]tftypes.Value
	if err := config.As(&set); err != nil {
		t.Fatal(err)
	}
	if err := prior.As(&state); err != nil {
		t.Fatal(err)
	}
	out := make(map[string]tftypes.Value, len(set))
	for name, v := range set {
		_, nested := v.Type().(tftypes.Object)
		switch {
		case v.IsNull():
			out[name] = state[name]
		case nested:
			out[name] = proposedValue(t, v, state[name])
		default:
			out[name] = v
		}
	}
	return tftypes.NewValue(config.Type(), out)
}

// protoPlan is one plan's answer.
type protoPlan struct {
	state   tftypes.Value
	private []byte
	replace []*tftypes.AttributePath
	// warnings are the plan's warnings, summary and detail.
	warnings []string
}

func (r *protocolRun) plan(prior tftypes.Value, private []byte) protoPlan {
	r.t.Helper()
	p, errs := r.tryPlan(prior, private)
	if len(errs) > 0 {
		r.t.Fatalf("plan: %v", errs)
	}
	return p
}

// tryPlan is plan, returning its errors rather than failing on them.
func (r *protocolRun) tryPlan(prior tftypes.Value, private []byte) (protoPlan, []string) {
	r.t.Helper()
	resp, err := r.ps.PlanResourceChange(context.Background(), &tfprotov6.PlanResourceChangeRequest{
		TypeName: r.typeName, PriorState: r.dynamic(prior), ProposedNewState: r.dynamic(r.proposed(prior)),
		Config: r.dynamic(r.config), PriorPrivate: private,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	if errs := diagnosticErrors(resp.Diagnostics); len(errs) > 0 {
		return protoPlan{}, errs
	}
	var warnings []string
	for _, d := range resp.Diagnostics {
		if d.Severity == tfprotov6.DiagnosticSeverityWarning {
			warnings = append(warnings, d.Summary+": "+d.Detail)
		}
	}
	return protoPlan{
		state: r.value(resp.PlannedState), private: resp.PlannedPrivate, replace: resp.RequiresReplace,
		warnings: warnings,
	}, nil
}

// replanTainted is the plan Terraform makes for a tainted object: a create,
// with no prior state, which it turns into a replacement afterwards.
func (r *protocolRun) replanTainted() (protoPlan, []string) {
	r.t.Helper()
	return r.tryPlan(tftypes.NewValue(r.obj, nil), nil)
}

// validate is ValidateResourceConfig on the configuration.
func (r *protocolRun) validate() []string {
	r.t.Helper()
	resp, err := r.ps.ValidateResourceConfig(context.Background(), &tfprotov6.ValidateResourceConfigRequest{
		TypeName: r.typeName, Config: r.dynamic(r.config),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return diagnosticErrors(resp.Diagnostics)
}

// applied is one apply's answer.
type applied struct {
	state   tftypes.Value
	private []byte
	diags   []*tfprotov6.Diagnostic
}

func (r *protocolRun) apply(ctx context.Context, prior tftypes.Value, p protoPlan) applied {
	r.t.Helper()
	resp, err := r.ps.ApplyResourceChange(ctx, &tfprotov6.ApplyResourceChangeRequest{
		TypeName: r.typeName, PriorState: r.dynamic(prior), PlannedState: r.dynamic(p.state),
		Config: r.dynamic(r.config), PlannedPrivate: p.private,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	return applied{state: r.value(resp.NewState), private: resp.Private, diags: resp.Diagnostics}
}

func (r *protocolRun) destroy(prior tftypes.Value, private []byte) applied {
	r.t.Helper()
	null := tftypes.NewValue(r.obj, nil)
	return r.apply(context.Background(), prior, protoPlan{state: null, private: private})
}

func (r *protocolRun) read(current tftypes.Value, private []byte) (tftypes.Value, []byte) {
	r.t.Helper()
	resp, err := r.ps.ReadResource(context.Background(), &tfprotov6.ReadResourceRequest{
		TypeName: r.typeName, CurrentState: r.dynamic(current), Private: private,
	})
	if err != nil {
		r.t.Fatal(err)
	}
	if errs := diagnosticErrors(resp.Diagnostics); len(errs) > 0 {
		r.t.Fatalf("read: %v", errs)
	}
	return r.value(resp.NewState), resp.Private
}

// conforms is Terraform's check on an apply that succeeds: every value the
// plan knew is the value the apply returned, or the apply fails with
// "Provider produced inconsistent result after apply".
func conforms(t *testing.T, plan, got tftypes.Value) {
	t.Helper()
	filled, err := tftypes.Transform(plan, func(p *tftypes.AttributePath, v tftypes.Value) (tftypes.Value, error) {
		if v.IsKnown() {
			return v, nil
		}
		at, _, err := tftypes.WalkAttributePath(got, p)
		if err != nil {
			return v, err
		}
		value, ok := at.(tftypes.Value)
		if !ok {
			return v, fmt.Errorf("%s isn't a value in the result", p)
		}
		return value, nil
	})
	if err != nil {
		t.Fatalf("comparing the plan with the result: %v", err)
	}
	if !filled.Equal(got) {
		diffs, err := filled.Diff(got)
		if err != nil {
			t.Fatalf("diffing the plan with the result: %v", err)
		}
		for _, d := range diffs {
			t.Errorf("the result moved a value the plan knew, at %s: planned %v, got %v", d.Path, d.Value1, d.Value2)
		}
	}
}

func stateOf(t *testing.T, v tftypes.Value) string {
	t.Helper()
	var attrs map[string]tftypes.Value
	if err := v.As(&attrs); err != nil {
		t.Fatal(err)
	}
	if !attrs["state"].IsKnown() {
		return "(known after apply)"
	}
	var s *string
	if err := attrs["state"].As(&s); err != nil {
		t.Fatal(err)
	}
	if s == nil {
		return "(null)"
	}
	return *s
}

// The kinds that build hardware, first among every kind that waits, with the
// body a read answers: pendingKinds' with the pools the plan asks for.
var hardwareKinds = map[string]struct {
	suffix, id string
	pools      []any
}{
	"cluster":        {clusterResourceSuffix, "c1", []any{}},
	"instance group": {instanceGroupResourceSuffix, "g1", []any{map[string]any{"nodePool": "workers", "desiredCount": 1}}},
}

func hardwarePlatform(t *testing.T, kind string) *slowPlatform {
	hw := hardwareKinds[kind]
	body := map[string]any{}
	for k, v := range pendingKinds()[kind].body {
		body[k] = v
	}
	body["nodePools"] = hw.pools
	return &slowPlatform{t: t, id: hw.id, body: body}
}

func hardwareRun(t *testing.T, kind string, platform *slowPlatform, extra map[string]tftypes.Value, timeouts map[string]string) *protocolRun {
	t.Helper()
	k := pendingKinds()[kind]
	set := k.plan(t, k.build(&Meta{}))
	for name, v := range extra {
		set[name] = v
	}
	return newProtocolRun(t, platform.serve().URL, hardwareKinds[kind].suffix, set, timeouts)
}

func containsAll(t *testing.T, what, text string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("%s does not say %q:\n%s", what, want, text)
		}
	}
}

// The whole sequence a user sees when a create outlives its wait:
//
//   - the plan to create reads the id, finds nothing (404), and plans a create
//   - the apply sends it and the wait stops, on its timeout or an interrupt:
//     an error, so the run exits 1, with the object in state for Terraform to
//     taint, naming the state it was in and the untaint and apply to run next
//   - `terraform apply` without the untaint plans the tainted object's
//     replacement as a create, which reads the id, finds it still `creating`,
//     and is refused with the same advice, so nothing is destroyed
//   - after `terraform untaint`, whether or not private state came with it,
//     the plan is an update that waits, its apply finishes the same object,
//     and one create was sent in all
func TestACreateThatStopsWaitingResumesAfterAnUntaint(t *testing.T) {
	for kind := range hardwareKinds {
		for _, interrupted := range []bool{false, true} {
			mode := "times out"
			if interrupted {
				mode = "is interrupted"
			}
			t.Run(kind+" whose wait "+mode, func(t *testing.T) {
				platform := hardwarePlatform(t, kind)
				run := hardwareRun(t, kind, platform, nil, map[string]string{"create": "200ms"})
				null := tftypes.NewValue(run.obj, nil)

				create := run.plan(null, nil)
				if n := platform.sent(http.MethodGet, "/"+platform.id); n == 0 {
					t.Error("the plan to create didn't read the id first")
				}
				// Terraform cancels the provider's context on Ctrl-C. Here it's
				// on the wait's first read, after the platform took the create.
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if interrupted {
					platform.mu.Lock()
					platform.interrupt = cancel
					platform.mu.Unlock()
				}
				first := run.apply(ctx, null, create)
				errs := strings.Join(diagnosticErrors(first.diags), "\n")
				if errs == "" {
					t.Fatal("a create still running succeeded, so a pipeline would read it as ready")
				}
				containsAll(t, "the create's error", errs, "is still being created",
					"still be running", "tainted", "terraform untaint <address>\n    terraform apply",
					"Don't run `terraform apply` first")
				if !interrupted {
					// The interrupt lands on the first read, so only a timeout
					// has a state to name.
					containsAll(t, "the create's error", errs, "still `creating`", "`timeouts`")
				}
				if first.state.IsNull() {
					t.Fatal("nothing went to state, so the next run can't find the object")
				}

				// The apply that forgot the untaint plans the replacement, which
				// is also the plan `terraform destroy` refreshes with, so it
				// passes, and warns before the approval that it cancels the
				// create, with the untaint that keeps it.
				replace, refused := run.replanTainted()
				if len(refused) > 0 {
					t.Fatalf("the plan to replace the tainted %s was refused, so a destroy would be too: %v", kind, refused)
				}
				containsAll(t, "the replacing plan's warning", strings.Join(replace.warnings, "\n"),
					"is still being created: applying this plan cancels it if this state holds it",
					"terraform untaint <address>\n    terraform apply", "`terraform destroy` shows this warning too")

				// After the untaint. Private state isn't relied on: nil here.
				refreshed, _ := run.read(first.state, nil)
				if got := stateOf(t, refreshed); got != "creating" {
					t.Errorf("the refresh reads %q, want creating", got)
				}
				resume := run.plan(refreshed, nil)
				if len(resume.replace) > 0 {
					t.Fatalf("the plan replaces the object over %v", resume.replace)
				}
				if got := stateOf(t, resume.state); got != "(known after apply)" {
					t.Errorf("planned state = %s, want it known after apply, so the plan is an update", got)
				}

				platform.settle()
				second := run.apply(context.Background(), refreshed, resume)
				if errs := diagnosticErrors(second.diags); len(errs) > 0 {
					t.Fatalf("the resumed apply failed: %v", errs)
				}
				if got := stateOf(t, second.state); got != "active" {
					t.Errorf("state after the resumed apply = %q, want active", got)
				}
				conforms(t, resume.state, second.state)

				settled, private := run.read(second.state, second.private)
				if after := run.plan(settled, private); !after.state.Equal(settled) {
					diffs, err := after.state.Diff(settled)
					t.Errorf("the plan after the resume still has changes: %v %v", diffs, err)
				}
				if n := platform.sent(http.MethodPost, ""); n != 1 {
					t.Errorf("%d creates sent, want one", n)
				}
				if n := platform.sent(http.MethodDelete, ""); n != 0 {
					t.Errorf("%d deletes sent, want none", n)
				}
			})
		}
	}
}

// `allow_replace = true` lets the replacement of a tainted object through, so
// a deliberate `terraform apply -replace` still works, and an id the platform
// doesn't hold plans a normal create.
func TestThePlanToCreateReadsTheIDFirst(t *testing.T) {
	for kind := range hardwareKinds {
		t.Run(kind+" that exists, with allow_replace", func(t *testing.T) {
			platform := hardwarePlatform(t, kind)
			platform.set("creating")
			run := hardwareRun(t, kind, platform,
				map[string]tftypes.Value{allowReplaceAttribute: tftypes.NewValue(tftypes.Bool, true)}, nil)
			if _, errs := run.replanTainted(); len(errs) > 0 {
				t.Errorf("allow_replace = true was refused: %v", errs)
			}
		})
		t.Run(kind+" that exists and is active", func(t *testing.T) {
			platform := hardwarePlatform(t, kind)
			platform.set("active")
			run := hardwareRun(t, kind, platform, nil, nil)
			planned, errs := run.replanTainted()
			if kind == "instance group" {
				// No tags to carry a create token, so it passes on its state.
				if len(errs) > 0 {
					t.Fatalf("an active instance group was refused: %v", errs)
				}
				containsAll(t, "the plan's warning", strings.Join(planned.warnings, "\n"),
					"exists: applying this plan replaces it if this state holds it")
				return
			}
			containsAll(t, "the refused plan", strings.Join(errs, "\n"), "already exists", "terraform import")
		})
		t.Run(kind+" that is being deleted", func(t *testing.T) {
			platform := hardwarePlatform(t, kind)
			platform.set("deleting")
			run := hardwareRun(t, kind, platform, nil, nil)
			if _, errs := run.replanTainted(); len(errs) > 0 {
				t.Errorf("an id on its way out was refused: %v", errs)
			}
		})
		t.Run(kind+" that doesn't exist", func(t *testing.T) {
			platform := hardwarePlatform(t, kind)
			run := hardwareRun(t, kind, platform, nil, nil)
			if _, errs := run.replanTainted(); len(errs) > 0 {
				t.Errorf("a 404 was refused: %v", errs)
			}
		})
		t.Run(kind+" with no id", func(t *testing.T) {
			platform := hardwarePlatform(t, kind)
			run := hardwareRun(t, kind, platform,
				map[string]tftypes.Value{"id": tftypes.NewValue(tftypes.String, nil)}, nil)
			containsAll(t, "validation", strings.Join(run.validate(), "\n"), "id")
		})
	}
}

// The cluster's create token says whether a Terraform apply made what the
// plan found: with it the plan passes and says so, and without it the plan
// is refused and says so. An instance group has no tags to carry one, and its
// warning says it passed on its state alone.
func TestAPlanSaysWhereTheObjectCameFrom(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		tags   map[string]any
		refuse bool
		want   string
	}{
		{"cluster", map[string]any{createTokenTag(): "abc"}, false, "carries this provider's create token"},
		{"cluster", map[string]any{}, true, "carries no create token"},
		{"instance group", nil, false, "passes on its state alone"},
	} {
		platform := hardwarePlatform(t, tc.kind)
		if tc.tags != nil {
			platform.body["tags"] = tc.tags
		}
		platform.set("creating")
		run := hardwareRun(t, tc.kind, platform, nil, nil)
		planned, errs := run.replanTainted()
		said := strings.Join(planned.warnings, "\n")
		if tc.refuse {
			said = strings.Join(errs, "\n")
		} else if len(errs) > 0 {
			t.Errorf("the %s with tags %v was refused: %v", tc.kind, tc.tags, errs)
		}
		containsAll(t, tc.kind+"'s plan", said, tc.want)
	}
}

// Every state a tainted object can be in passes the plan that replaces it,
// since that's the plan `terraform destroy` refreshes with, and the warning
// says what applying it does in that state. A cluster without this
// provider's create token is refused in every one of them.
func TestAPlanPassesATaintedCreateInEveryState(t *testing.T) {
	for _, state := range []struct{ state, says string }{
		{"creating", "is still being created: applying this plan cancels it if this state holds it"},
		{"updating", "is still being updated: applying this plan cancels it if this state holds it"},
		{"active", "exists: applying this plan replaces it if this state holds it"},
		{"failed", "failed: applying this plan replaces it if this state holds it"},
	} {
		for _, tc := range []struct {
			kind   string
			tags   map[string]any
			refuse bool
		}{
			{"cluster", map[string]any{createTokenTag(): "abc"}, false},
			{"cluster", map[string]any{}, true},
			{"instance group", nil, false},
		} {
			t.Run(fmt.Sprintf("%s %s with tags %v", state.state, tc.kind, tc.tags), func(t *testing.T) {
				platform := hardwarePlatform(t, tc.kind)
				if tc.tags != nil {
					platform.body["tags"] = tc.tags
				}
				platform.set(state.state)
				run := hardwareRun(t, tc.kind, platform, nil, nil)
				planned, errs := run.replanTainted()
				if tc.refuse {
					containsAll(t, "the refused plan", strings.Join(errs, "\n"),
						"carries no create token", "Nothing was sent: the plan was refused.",
						"so it's refused too", "terraform untaint <address>\n    terraform destroy")
					return
				}
				if len(errs) > 0 {
					t.Fatalf("the plan was refused, so a destroy would be too: %v", errs)
				}
				warned := strings.Join(planned.warnings, "\n")
				containsAll(t, "the plan's warning", warned, state.says, "`terraform destroy` shows this warning too")
				if state.state != "failed" {
					containsAll(t, "the plan's warning", warned, "terraform untaint <address>\n    terraform apply")
				}
			})
		}
	}
}

// A create the platform failed isn't being built, so the plan that replaces
// it passes, which is the plan a destroy refreshes with too. It warns that
// applying it deletes the failed object first. A failed cluster without this
// provider's create token is still refused: it isn't this configuration's to
// replace.
func TestAPlanPassesAFailedCreate(t *testing.T) {
	for _, tc := range []struct {
		kind   string
		tags   map[string]any
		refuse bool
	}{
		{"cluster", map[string]any{createTokenTag(): "abc"}, false},
		{"cluster", map[string]any{}, true},
		{"instance group", nil, false},
	} {
		platform := hardwarePlatform(t, tc.kind)
		if tc.tags != nil {
			platform.body["tags"] = tc.tags
		}
		platform.set("failed")
		run := hardwareRun(t, tc.kind, platform, nil, nil)
		planned, errs := run.replanTainted()
		switch {
		case tc.refuse:
			containsAll(t, tc.kind+"'s refused plan", strings.Join(errs, "\n"), "already exists", "`failed`")
		case len(errs) > 0:
			t.Errorf("a failed %s with tags %v was refused: %v", tc.kind, tc.tags, errs)
		default:
			containsAll(t, tc.kind+"'s plan warning", strings.Join(planned.warnings, "\n"),
				"applying this plan replaces it", "Its delete can fail", "run `terraform destroy` instead",
				"`terraform destroy` shows this warning too")
		}
	}
}

// A resumed wait that stops again is an error, which an update never taints,
// and the object stays `creating` in state for the run after.
func TestAResumedCreateThatStopsAgainKeepsItsPlace(t *testing.T) {
	platform := hardwarePlatform(t, "cluster")
	run := hardwareRun(t, "cluster", platform, nil, map[string]string{"create": "100ms"})

	null := tftypes.NewValue(run.obj, nil)
	first := run.apply(context.Background(), null, run.plan(null, nil))
	refreshed, private := run.read(first.state, first.private)
	resume := run.plan(refreshed, private)
	second := run.apply(context.Background(), refreshed, resume)

	containsAll(t, "the error", strings.Join(diagnosticErrors(second.diags), "\n"),
		"is still being created", "still `creating`", "`terraform apply` again", "`timeouts`")
	if !second.state.Equal(refreshed) {
		t.Error("the state moved on an update that failed")
	}
	if n := platform.sent(http.MethodPost, ""); n != 1 {
		t.Errorf("%d creates sent, want one", n)
	}
}

// A destroy whose wait times out leaves the object in state and says the
// next destroy picks the delete up. That destroy reads it `deleting`, sends no
// second DELETE, and finishes once the platform reaps it.
func TestADeleteThatStopsWaitingResumesOnTheNextDestroy(t *testing.T) {
	for kind, hw := range hardwareKinds {
		t.Run(kind, func(t *testing.T) {
			k := pendingKinds()[kind]
			platform := hardwarePlatform(t, kind)
			run := newProtocolRun(t, platform.serve().URL, hw.suffix, k.plan(t, k.build(&Meta{})),
				map[string]string{"delete": "100ms"})
			null := tftypes.NewValue(run.obj, nil)
			go func() {
				// Settles the create once it's been sent, so the first apply
				// finishes.
				for platform.sent(http.MethodPost, "") == 0 {
					time.Sleep(time.Millisecond)
				}
				platform.settle()
			}()
			created := run.apply(context.Background(), null, run.plan(null, nil))
			if errs := diagnosticErrors(created.diags); len(errs) > 0 {
				t.Fatalf("create: %v", errs)
			}

			first := run.destroy(created.state, created.private)
			errs := strings.Join(diagnosticErrors(first.diags), "\n")
			for _, want := range []string{"did not finish deleting", "still `deleting`", "still be running",
				"`terraform destroy` again", "sending another", "`timeouts`"} {
				if !strings.Contains(errs, want) {
					t.Errorf("the error does not say %q:\n%s", want, errs)
				}
			}

			// The platform reaps it while the second destroy waits.
			reads := platform.sent(http.MethodGet, "/"+hw.id)
			go func() {
				for platform.sent(http.MethodGet, "/"+hw.id) < reads+2 {
					time.Sleep(time.Millisecond)
				}
				platform.reap()
			}()
			second := run.destroy(created.state, created.private)
			if errs := diagnosticErrors(second.diags); len(errs) > 0 {
				t.Fatalf("the second destroy failed: %v", errs)
			}
			if !second.state.IsNull() {
				t.Error("the object is still in state after the second destroy")
			}
			if n := platform.sent(http.MethodDelete, ""); n != 1 {
				t.Errorf("%d deletes sent, want one", n)
			}
		})
	}
}

// What holds for the hardware holds for every kind that waits on a create:
// the wait times out with the untaint advice, and after the untaint the next
// plan is an update that finishes the same object without a second create.
// The VPC security group binding creates nothing, so it has nothing to resume:
// its create writes the binding again.
func TestEveryKindThatWaitsResumesItsCreate(t *testing.T) {
	suffixes := map[string]string{
		"cluster": clusterResourceSuffix, "instance group": instanceGroupResourceSuffix,
		"file store": fileStoreResourceSuffix, "network": networkResourceSuffix,
		"subnet": subnetResourceSuffix, "security group": securityGroupResourceSuffix,
		"VPC peering": vpcPeeringResourceSuffix,
	}
	for kind, k := range pendingKinds() {
		suffix, ok := suffixes[kind]
		if !ok {
			continue
		}
		t.Run(kind, func(t *testing.T) {
			body := map[string]any{}
			for name, v := range k.body {
				body[name] = v
			}
			if hw, ok := hardwareKinds[kind]; ok {
				body["nodePools"] = hw.pools
			}
			if kind == "network" {
				// What a read of a settled network carries, which the plan
				// holds a refresh to.
				body["dhcpEnabled"], body["vlan"] = false, map[string]any{"id": "100"}
			}
			set := k.plan(t, k.build(&Meta{}))
			var id string
			if err := set[k.idAttr].As(&id); err != nil {
				t.Fatal(err)
			}
			platform := &slowPlatform{t: t, id: id, body: body}
			run := newProtocolRun(t, platform.serve().URL, suffix, set, map[string]string{"create": "100ms"})

			null := tftypes.NewValue(run.obj, nil)
			first := run.apply(context.Background(), null, run.plan(null, nil))
			containsAll(t, "the create's error", strings.Join(diagnosticErrors(first.diags), "\n"),
				"is still being created", "terraform untaint <address>\n    terraform apply")
			if first.state.IsNull() {
				t.Fatal("nothing went to state, so the next run can't find the object")
			}

			// After `terraform untaint`, with no private state relied on.
			refreshed, _ := run.read(first.state, nil)
			resume := run.plan(refreshed, nil)
			if len(resume.replace) > 0 {
				t.Fatalf("the plan replaces the %s over %v", kind, resume.replace)
			}
			if resume.state.Equal(refreshed) {
				t.Fatal("the plan has no changes, so nothing waits for the create")
			}
			platform.settle()
			second := run.apply(context.Background(), refreshed, resume)
			if errs := diagnosticErrors(second.diags); len(errs) > 0 {
				t.Fatalf("the resumed apply failed: %v", errs)
			}
			conforms(t, resume.state, second.state)
			if got := stateOf(t, second.state); got != "active" {
				t.Errorf("state after the resumed apply = %q, want active", got)
			}
			if n := platform.sent(http.MethodPost, ""); n != 1 {
				t.Errorf("%d creates sent, want one", n)
			}
		})
	}
}

// A file store found `deleting` has nothing left to detach and nothing to
// send: the destroy waits for that delete, and when the wait stops again it
// says so with the next command.
func TestAFileStoreAlreadyDeletingIsWaitedOut(t *testing.T) {
	platform := &slowPlatform{t: t, id: "fs1", body: fileStoreBody("deleting")}
	platform.set("deleting")
	server := platform.serve()
	r := &fileStoreResource{meta: testClusterMeta(t, server.URL, 100*time.Millisecond)}
	raw, s := testValue(t, r, fileStorePlan(nil))
	st := tfsdk.State{Raw: raw, Schema: s.Schema}
	resp := resource.DeleteResponse{State: st}
	r.Delete(context.Background(), resource.DeleteRequest{State: st}, &resp)

	text := detailOf(resp.Diagnostics)
	for _, want := range []string{"did not finish deleting", "still `deleting`", "`terraform destroy` again"} {
		if !strings.Contains(text, want) {
			t.Errorf("the error does not say %q:\n%s", want, text)
		}
	}
	for _, method := range []string{http.MethodDelete, http.MethodPatch} {
		if n := platform.sent(method, ""); n != 0 {
			t.Errorf("%d %s sent to a store already deleting, want none", n, method)
		}
	}
}

// A delete the platform ended gives the way out in order: destroy again, then
// report it and stop managing it with a removed block, never an edit to state.
// A delete still running keeps its own advice.
func TestADeleteThePlatformEndedSaysWhatNext(t *testing.T) {
	var refused diag.Diagnostics
	addDeleteWaitError(&refused, "cluster", ResourceName(clusterResourceSuffix), "The cluster did not finish deleting",
		platformRefusal{"the platform reports the cluster as failed: reason INTERNAL"})
	detail := refused.Errors()[0].Detail()
	for _, want := range []string{"run `terraform destroy` again", "removed {", "destroy = false", ResourceName(clusterResourceSuffix) + ".example"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the advice doesn't say %q:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "state rm") {
		t.Errorf("the advice sends the reader to edit state:\n%s", detail)
	}

	var running diag.Diagnostics
	addDeleteWaitError(&running, "cluster", ResourceName(clusterResourceSuffix), "The cluster did not finish deleting",
		fmt.Errorf("%w: %w", errWaitTimeout, provisionalFailure{platformRefusal{"failed"}}))
	if d := running.Errors()[0].Detail(); strings.Contains(d, deleteRefusedAdvice) {
		t.Errorf("a delete still running got the advice for one the platform ended:\n%s", d)
	}
}
