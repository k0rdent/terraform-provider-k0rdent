package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/identityschema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// guardedModifier is inner behind the guard, as the plan-time interface the
// tests call.
func guardedModifier(t *testing.T, inner guardable, what tenantState) resource.ResourceWithModifyPlan {
	t.Helper()
	r, ok := guardReplacements(inner, what).(resource.ResourceWithModifyPlan)
	if !ok {
		t.Fatal("a guarded resource has no ModifyPlan")
	}
	return r
}

// stubResource is a resource the guard wraps in the tests below. It records
// what it's handed and fails a test that hands it the opt-in.
type stubResource struct {
	t *testing.T
	// calls names each method called, in order.
	calls []string
	// replaceInPlan is a path the stub's ModifyPlan marks as a replacement,
	// the way scope.go marks a moved region.
	replaceInPlan *path.Path
	// gone makes Read find the resource deleted.
	gone bool
	// updateFails makes Update fail after writing what it planned, the way
	// a resource's Update fails partway. updateDrops makes it drop the
	// resource from state, which the destroy rule keeps every real Update
	// from doing.
	updateFails, updateDrops bool
}

var _ guardable = (*stubResource)(nil)

func (s *stubResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = "stub"
}

func (s *stubResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A stub.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Required: true, MarkdownDescription: "The id.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"name":  schema.StringAttribute{Optional: true, MarkdownDescription: "The name."},
			"state": schema.StringAttribute{Computed: true, MarkdownDescription: "What the platform says."},
			"size":  schema.Int64Attribute{Optional: true, MarkdownDescription: "A number."},
		},
	}
}

func (s *stubResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = identityschema.Schema{Attributes: map[string]identityschema.Attribute{
		"id": identityschema.StringAttribute{RequiredForImport: true},
	}}
}

func (s *stubResource) Configure(context.Context, resource.ConfigureRequest, *resource.ConfigureResponse) {
	s.calls = append(s.calls, "Configure")
}

func (s *stubResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader { return nil }

// sees fails the test if any value handed to the stub carries the opt-in.
func (s *stubResource) sees(method string, values ...tftypes.Value) {
	s.calls = append(s.calls, method)
	for _, v := range values {
		if obj, ok := v.Type().(tftypes.Object); ok {
			if _, has := obj.AttributeTypes[allowReplaceAttribute]; has {
				s.t.Errorf("%s was handed %s", method, allowReplaceAttribute)
			}
		}
	}
}

func (s *stubResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	s.sees("ModifyPlan", req.Config.Raw, req.Plan.Raw, req.State.Raw, resp.Plan.Raw)
	if s.replaceInPlan != nil && !req.State.Raw.IsNull() && !req.Plan.Raw.IsNull() {
		resp.RequiresReplace = append(resp.RequiresReplace, *s.replaceInPlan)
	}
	if !req.Plan.Raw.IsNull() {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("state"), types.StringUnknown())...)
	}
}

func (s *stubResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	s.sees("Create", req.Config.Raw, req.Plan.Raw, resp.State.Raw)
	var name types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("name"), &name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), "s1")...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("state"), "active")...)
}

func (s *stubResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	s.sees("Read", req.State.Raw, resp.State.Raw)
	if s.gone {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("state"), "read")...)
}

func (s *stubResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	s.sees("Update", req.Config.Raw, req.Plan.Raw, req.State.Raw, resp.State.Raw)
	if s.updateDrops {
		resp.State = tfsdk.State{Raw: tftypes.NewValue(resp.State.Raw.Type(), nil), Schema: resp.State.Schema}
		return
	}
	if s.updateFails {
		defer resp.Diagnostics.AddError("The stub failed", "on purpose")
	}
	var name types.String
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("name"), &name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("name"), name)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("state"), "updated")...)
}

func (s *stubResource) Delete(_ context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	s.sees("Delete", req.State.Raw, resp.State.Raw)
}

func (s *stubResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	s.sees("ImportState", resp.State.Raw)
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}

var stubTenantState = tenantState{noun: "stub", loses: "A stub holds what a test put in it."}

// guardedStub is a stub behind the guard, with the guarded schema.
func guardedStub(t *testing.T) (*stubResource, resource.Resource, schema.Schema) {
	t.Helper()
	stub := &stubResource{t: t}
	r := guardReplacements(stub, stubTenantState)
	return stub, r, schemaOf(t, r)
}

func stubValue(t *testing.T, s schema.Schema, set map[string]tftypes.Value) tftypes.Value {
	t.Helper()
	object, ok := s.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema is not an object")
	}
	values := make(map[string]tftypes.Value, len(object.AttributeTypes))
	for name, attrType := range object.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	for name, v := range set {
		values[name] = v
	}
	return tftypes.NewValue(object, values)
}

func optInOf(t *testing.T, st tfsdk.State) types.Bool {
	t.Helper()
	var allow types.Bool
	if d := st.GetAttribute(context.Background(), path.Root(allowReplaceAttribute), &allow); d.HasError() {
		t.Fatalf("read %s: %v", allowReplaceAttribute, d)
	}
	return allow
}

var (
	yes      = tftypes.NewValue(tftypes.Bool, true)
	pending  = tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue)
	stubBase = map[string]tftypes.Value{"id": str("s1"), "name": str("a"), "state": str("active")}
)

// The guarded schema is the resource's, with the opt-in and the sentences
// saying what replaces it and how to permit that.
func TestTheGuardAddsTheOptInAndSaysWhatItGuards(t *testing.T) {
	_, _, s := guardedStub(t)
	a, ok := s.Attributes[allowReplaceAttribute].(schema.BoolAttribute)
	if !ok || !a.Optional || a.Computed || a.Default != nil {
		t.Fatalf("%s = %#v, want an optional bool with no default", allowReplaceAttribute, s.Attributes[allowReplaceAttribute])
	}
	for _, want := range []string{"**What replaces it.** Changing `id` cannot be done in place",
		"unless `allow_replace = true` is set on the resource, a change that would replace it plans as an update",
		"the apply refuses it before it sends any request. `terraform destroy` isn't affected."} {
		if !strings.Contains(s.MarkdownDescription, want) {
			t.Errorf("description lacks %q:\n%s", want, s.MarkdownDescription)
		}
	}
	if id := s.Attributes["id"].GetMarkdownDescription(); !strings.Contains(id, "(needs `allow_replace = true`)") {
		t.Errorf("id's description doesn't say it needs the opt-in: %q", id)
	}
}

func planStub(t *testing.T, r resource.Resource, s schema.Schema, prior, planned tftypes.Value) resource.ModifyPlanResponse {
	t.Helper()
	resp := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Raw: planned, Schema: s}}
	freshPrivate(t, &resp.Private, nil)
	modifier, ok := r.(resource.ResourceWithModifyPlan)
	if !ok {
		t.Fatal("a guarded resource has no ModifyPlan")
	}
	// The configuration can't set `state`, which is computed alone.
	attrs, err := attributesOf(planned)
	if err != nil {
		t.Fatal(err)
	}
	attrs["state"] = tftypes.NewValue(tftypes.String, nil)
	req := resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: tftypes.NewValue(planned.Type(), attrs), Schema: s},
		Plan:   tfsdk.Plan{Raw: planned, Schema: s},
		State:  tfsdk.State{Raw: prior, Schema: s},
	}
	runServedReplacementModifiers(t, req, &resp)
	modifier.ModifyPlan(context.Background(), req, &resp)
	return resp
}

// The refusal, through the guard: a replacing change plans with a warning
// naming the attribute and records the refusal for the apply, the opt-in lets
// it through, and an opt-in known only at apply doesn't. The resource's own
// ModifyPlan runs first and never sees the opt-in.
func TestTheGuardWarnsOfAReplacementTheResourceDoesntKnowAbout(t *testing.T) {
	stub, r, s := guardedStub(t)
	prior := stubValue(t, s, stubBase)

	refused := planStub(t, r, s, prior, stubValue(t, s, with(stubBase, map[string]tftypes.Value{"id": str("s2")})))
	if got := replacementWarnings(refused.Diagnostics); len(got) != 1 || got[0] != "Changing `id` would replace this stub, so the apply will refuse it" {
		t.Errorf("warnings = %q, want the warning naming id", got)
	}
	if refused.Diagnostics.HasError() {
		t.Errorf("errors = %q, want none, so a destroy's plan goes ahead", errorSummaries(refused.Diagnostics))
	}
	if got := refusalRecorded(t, refused.Private); !reflect.DeepEqual(got, []string{"id"}) {
		t.Errorf("recorded for the apply: %q, want id", got)
	}
	// The resource planned `state` unknown. The apply refuses, so it keeps
	// prior state's value, and a dependent that reads it isn't replaced. The
	// refused attribute still shows its change.
	var kept, id types.String
	refused.Plan.GetAttribute(context.Background(), path.Root("state"), &kept)
	refused.Plan.GetAttribute(context.Background(), path.Root("id"), &id)
	if kept.ValueString() != "active" || id.ValueString() != "s2" {
		t.Errorf("state = %v, id = %v, want prior state's \"active\" and the planned \"s2\"", kept, id)
	}
	if len(stub.calls) != 1 || stub.calls[0] != "ModifyPlan" {
		t.Errorf("calls = %q, want the resource's ModifyPlan", stub.calls)
	}

	permitted := planStub(t, r, s, prior, stubValue(t, s, with(stubBase, map[string]tftypes.Value{
		"id": str("s2"), allowReplaceAttribute: yes})))
	if permitted.Diagnostics.HasError() || len(replacementWarnings(permitted.Diagnostics)) != 0 {
		t.Errorf("refused with the opt-in: %v", permitted.Diagnostics)
	}
	if got := refusalRecorded(t, permitted.Private); len(got) != 0 {
		t.Errorf("recorded with the opt-in: %q", got)
	}
	// The resource's plan is kept, and the opt-in is planned as configured.
	var state types.String
	permitted.Plan.GetAttribute(context.Background(), path.Root("state"), &state)
	if !state.IsUnknown() {
		t.Errorf("state = %v, want the unknown the resource planned", state)
	}
	var allow types.Bool
	permitted.Plan.GetAttribute(context.Background(), path.Root(allowReplaceAttribute), &allow)
	if !allow.ValueBool() {
		t.Errorf("planned %s = %v, want true", allowReplaceAttribute, allow)
	}

	unknown := planStub(t, r, s, prior, stubValue(t, s, with(stubBase, map[string]tftypes.Value{
		"id": str("s2"), allowReplaceAttribute: pending})))
	if got := refusalRecorded(t, unknown.Private); len(got) != 1 {
		t.Error("an opt-in known only at apply let the replacement through")
	}

	inPlace := planStub(t, r, s, prior, stubValue(t, s, with(stubBase, map[string]tftypes.Value{"name": str("b")})))
	if inPlace.Diagnostics.HasError() || len(replacementWarnings(inPlace.Diagnostics)) != 0 {
		t.Errorf("an in-place change was refused: %v", inPlace.Diagnostics)
	}
	if got := refusalRecorded(t, inPlace.Private); len(got) != 0 {
		t.Errorf("an in-place change was recorded for refusal: %q", got)
	}
}

// A replacement the resource decides in its own ModifyPlan, the way a moved
// region is, is refused too, because the guard runs after it, and it's taken
// out of RequiresReplace so Terraform plans the update the apply refuses.
func TestTheGuardWarnsOfAReplacementTheResourcePlans(t *testing.T) {
	stub, r, s := guardedStub(t)
	name := path.Root("name")
	stub.replaceInPlan = &name
	prior := stubValue(t, s, stubBase)
	resp := planStub(t, r, s, prior, stubValue(t, s, with(stubBase, map[string]tftypes.Value{"name": str("b")})))
	if got := replacementWarnings(resp.Diagnostics); len(got) != 1 || got[0] != "Changing `name` would replace this stub, so the apply will refuse it" {
		t.Errorf("warnings = %q, want the warning naming name", got)
	}
	if len(resp.RequiresReplace) != 0 {
		t.Errorf("RequiresReplace = %v, want none", resp.RequiresReplace)
	}
	if got := refusalRecorded(t, resp.Private); !reflect.DeepEqual(got, []string{"name"}) {
		t.Errorf("recorded for the apply: %q, want name", got)
	}
}

// A refused value known only after apply may turn out unchanged, and then
// Terraform's final plan has no replacement and plans the update with its
// computed values unknown. So nothing is kept from prior state, or that
// final plan would be inconsistent, and the warning says the apply refuses
// only if the value differs.
func TestAnUnknownRefusedValueKeepsNothing(t *testing.T) {
	_, r, s := guardedStub(t)
	prior := stubValue(t, s, stubBase)
	resp := planStub(t, r, s, prior, stubValue(t, s, with(stubBase, map[string]tftypes.Value{
		"id": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)})))
	if got := refusalRecorded(t, resp.Private); !reflect.DeepEqual(got, []string{"id"}) {
		t.Errorf("recorded for the apply: %q, want id", got)
	}
	var state types.String
	resp.Plan.GetAttribute(context.Background(), path.Root("state"), &state)
	if !state.IsUnknown() {
		t.Errorf("state = %v, want the unknown the resource planned", state)
	}
	if got := replacementWarnings(resp.Diagnostics); len(got) != 1 ||
		got[0] != "Changing `id` would replace this stub, so the apply will refuse it if the new value differs" {
		t.Errorf("warnings = %q", got)
	}
	if d := resp.Diagnostics.Warnings()[0].Detail(); !strings.Contains(d, "If it turns out to be the current value, the stub isn't replaced") {
		t.Errorf("the warning doesn't say what happens if the value is unchanged: %q", d)
	}
}

// A path both the resource's ModifyPlan and an attribute's modifier name is
// one replacement, warned about and refused once.
func TestAPathNamedTwiceIsRefusedOnce(t *testing.T) {
	stub, r, s := guardedStub(t)
	id := path.Root("id")
	stub.replaceInPlan = &id
	resp := planStub(t, r, s, stubValue(t, s, stubBase), stubValue(t, s, with(stubBase, map[string]tftypes.Value{"id": str("s2")})))
	if got := refusalRecorded(t, resp.Private); !reflect.DeepEqual(got, []string{"id"}) {
		t.Errorf("recorded for the apply: %q, want id once", got)
	}
}

// updateStub runs the guarded stub's Update with private data that holds
// refused, as the plan left it.
func updateStub(t *testing.T, r resource.Resource, s schema.Schema, prior, planned tftypes.Value, refused []refusedReplacement) resource.UpdateResponse {
	t.Helper()
	ctx := context.Background()
	req := resource.UpdateRequest{Config: tfsdk.Config{Raw: planned, Schema: s},
		Plan: tfsdk.Plan{Raw: planned, Schema: s}, State: tfsdk.State{Raw: prior, Schema: s}}
	freshPrivate(t, &req.Private, nil)
	if refused != nil {
		w, ok := any(req.Private).(privateWriter)
		if !ok {
			t.Fatal("private state cannot be written")
		}
		var diags diag.Diagnostics
		recordRefusal(ctx, w, refused, &diags)
		if diags.HasError() {
			t.Fatal(diags)
		}
	}
	resp := resource.UpdateResponse{State: tfsdk.State{Raw: prior, Schema: s}, Private: req.Private}
	r.Update(ctx, req, &resp)
	return resp
}

// The apply's half: an update the plan recorded as a refused replacement
// fails before the resource is called, so no request goes out, state stays
// as it was, and the refusal isn't written back to outlive the apply. An
// update the plan recorded nothing for goes to the resource as before.
func TestTheGuardedUpdateRefusesARecordedReplacement(t *testing.T) {
	stub, r, s := guardedStub(t)
	prior := stubValue(t, s, stubBase)
	planned := stubValue(t, s, with(stubBase, map[string]tftypes.Value{"id": str("s2"), "name": str("b")}))
	change := "`id` goes from \"s1\" to \"s2\"."

	resp := updateStub(t, r, s, prior, planned, []refusedReplacement{{Path: []string{"id"}, Change: change}})
	if got := errorSummaries(resp.Diagnostics); len(got) != 1 || got[0] != "Changing `id` would replace this stub, so the apply refused it" {
		t.Errorf("errors = %q, want the refusal naming id", got)
	}
	if d, ok := resp.Diagnostics.Errors()[0].(diag.DiagnosticWithPath); !ok || d.Path().String() != "id" {
		t.Errorf("the refusal does not point at id: %v", resp.Diagnostics.Errors()[0])
	}
	if d := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(d, change) {
		t.Errorf("the refusal doesn't say what changed: %q", d)
	}
	if len(stub.calls) != 0 {
		t.Errorf("calls = %q, want none", stub.calls)
	}
	if !resp.State.Raw.Equal(prior) {
		t.Errorf("state = %v, want prior state", resp.State.Raw)
	}
	if got := refusalRecorded(t, resp.Private); len(got) != 0 {
		t.Errorf("the refusal is written back to state: %q", got)
	}

	resp = updateStub(t, r, s, prior, stubValue(t, s, with(stubBase, map[string]tftypes.Value{"name": str("b")})), nil)
	if resp.Diagnostics.HasError() || !reflect.DeepEqual(stub.calls, []string{"Update"}) {
		t.Errorf("an update with nothing recorded: calls = %q, %v", stub.calls, resp.Diagnostics)
	}
}

// A refused Update writes the plan's private data back to state, so the
// next plan starts by clearing it: an in-place change after a refusal goes
// through.
func TestAPlanClearsAnEarlierRefusal(t *testing.T) {
	_, r, s := guardedStub(t)
	prior := stubValue(t, s, stubBase)
	planned := stubValue(t, s, with(stubBase, map[string]tftypes.Value{"name": str("b")}))
	resp := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Raw: planned, Schema: s}}
	freshPrivate(t, &resp.Private, nil)
	w, ok := any(resp.Private).(privateWriter)
	if !ok {
		t.Fatal("private state cannot be written")
	}
	var diags diag.Diagnostics
	recordRefusal(context.Background(), w, []refusedReplacement{{Path: []string{"id"}, Change: "earlier"}}, &diags)
	if got := refusalRecorded(t, resp.Private); len(got) != 1 || diags.HasError() {
		t.Fatalf("the earlier refusal wasn't set up: %q %v", got, diags)
	}
	modifier, ok := r.(resource.ResourceWithModifyPlan)
	if !ok {
		t.Fatal("a guarded resource has no ModifyPlan")
	}
	modifier.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: planned, Schema: s},
		Plan:   tfsdk.Plan{Raw: planned, Schema: s},
		State:  tfsdk.State{Raw: prior, Schema: s},
	}, &resp)
	if got := refusalRecorded(t, resp.Private); len(got) != 0 {
		t.Errorf("the earlier refusal outlived the plan: %q", got)
	}
}

// The served schema's RequiresReplace modifiers are wrapped, and a wrapper
// answers as the modifier it wraps only when the configuration sets
// `allow_replace = true`. Otherwise Terraform would plan the replacement and
// call Delete, which can't tell a refused replacement from a permitted one.
func TestTheServedModifierDropsAReplacementTheConfigDoesntPermit(t *testing.T) {
	_, _, s := guardedStub(t)
	id, ok := s.Attributes["id"].(schema.StringAttribute)
	if !ok || len(id.PlanModifiers) != 1 {
		t.Fatalf("id = %#v", s.Attributes["id"])
	}
	m := id.PlanModifiers[0]
	if _, wrapped := m.(replacementWrapper); !wrapped {
		t.Fatalf("id's RequiresReplace is served unwrapped: %T", m)
	}
	prior := stubValue(t, s, stubBase)
	for allow, want := range map[string]bool{"null": false, "false": false, "unknown": false, "true": true} {
		v := map[string]tftypes.Value{"null": tftypes.NewValue(tftypes.Bool, nil), "false": tftypes.NewValue(tftypes.Bool, false),
			"unknown": pending, "true": yes}[allow]
		config := stubValue(t, s, with(stubBase, map[string]tftypes.Value{"id": str("s2"), allowReplaceAttribute: v}))
		resp := planmodifier.StringResponse{PlanValue: types.StringValue("s2")}
		freshPrivate(t, &resp.Private, nil)
		m.PlanModifyString(context.Background(), planmodifier.StringRequest{
			Path: path.Root("id"), Config: tfsdk.Config{Raw: config, Schema: s}, ConfigValue: types.StringValue("s2"),
			Plan: tfsdk.Plan{Raw: config, Schema: s}, PlanValue: types.StringValue("s2"),
			State: tfsdk.State{Raw: prior, Schema: s}, StateValue: types.StringValue("s1"),
		}, &resp)
		if resp.RequiresReplace != want || resp.Diagnostics.HasError() {
			t.Errorf("allow_replace %s: RequiresReplace = %v, want %v: %v", allow, resp.RequiresReplace, want, resp.Diagnostics)
		}
		// What it dropped is recorded for the guard, and nothing else is.
		var diags diag.Diagnostics
		recorded := takeDropped(context.Background(), resp.Private, &diags)
		if got := len(recorded) == 1 && recorded[0].Equal(path.Root("id")); got == want || diags.HasError() {
			t.Errorf("allow_replace %s: recorded %v, want the drop recorded: %v", allow, recorded, !want)
		}
	}
}

// The guard refuses a recorded path only when its final planned value
// differs from prior state, as Terraform replaces only then. A modifier that
// asked while the value was unknown, before UseStateForUnknown put the prior
// value back, plans an update in place.
func TestARecordedPathWhoseValueDidntChangeIsNotRefused(t *testing.T) {
	_, r, s := guardedStub(t)
	prior := stubValue(t, s, stubBase)
	planned := stubValue(t, s, with(stubBase, map[string]tftypes.Value{"name": str("b")}))
	resp := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Raw: planned, Schema: s}}
	freshPrivate(t, &resp.Private, nil)
	var diags diag.Diagnostics
	recordDropped(context.Background(), resp.Private, path.Root("id"), &diags)
	if diags.HasError() {
		t.Fatal(diags)
	}
	modifier, ok := r.(resource.ResourceWithModifyPlan)
	if !ok {
		t.Fatal("a guarded resource has no ModifyPlan")
	}
	modifier.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: planned, Schema: s},
		Plan:   tfsdk.Plan{Raw: planned, Schema: s},
		State:  tfsdk.State{Raw: prior, Schema: s},
	}, &resp)
	if got := replacementWarnings(resp.Diagnostics); len(got) != 0 {
		t.Errorf("warnings = %q, want none: id kept its value", got)
	}
	if got := refusalRecorded(t, resp.Private); len(got) != 0 {
		t.Errorf("recorded for the apply: %q", got)
	}
}

// servedReplacements walks a schema, every kind of attribute and block and
// however deep, and returns the RequiresReplace modifiers it serves bare and
// the number it serves wrapped.
func servedReplacements(s schema.Schema) (bare []string, wrapped int) {
	var walk func(where string, v reflect.Value)
	walk = func(where string, v reflect.Value) {
		if v.Kind() == reflect.Interface {
			v = v.Elem()
		}
		if v.Kind() != reflect.Struct {
			return
		}
		if mods := v.FieldByName("PlanModifiers"); mods.IsValid() && mods.Kind() == reflect.Slice {
			for i := range mods.Len() {
				m := mods.Index(i).Interface()
				if _, ok := m.(replacementWrapper); ok {
					wrapped++
					continue
				}
				if IsReplacementModifier(m) {
					bare = append(bare, fmt.Sprintf("%s: %T", where, m))
				}
			}
		}
		for _, field := range []string{"Attributes", "Blocks", "NestedObject"} {
			f := v.FieldByName(field)
			switch {
			case !f.IsValid():
			case f.Kind() == reflect.Map:
				for _, k := range f.MapKeys() {
					walk(strings.TrimPrefix(where+"."+k.String(), "."), f.MapIndex(k))
				}
			default:
				walk(where, f)
			}
		}
	}
	walk("", reflect.ValueOf(s))
	slices.Sort(bare)
	return bare, wrapped
}

// Every RequiresReplace modifier a guarded resource serves is wrapped,
// whatever kind of attribute carries it and however deep. One served bare,
// in a block too, would plan a replacement Terraform carries out without the
// guard.
func TestEveryServedReplacementIsWrapped(t *testing.T) {
	for suffix, newResource := range tenantStateResources {
		t.Run(suffix, func(t *testing.T) {
			bare, wrapped := servedReplacements(schemaOf(t, newResource()))
			for _, b := range bare {
				t.Errorf("%s is served unwrapped", b)
			}
			if wrapped == 0 {
				t.Error("no wrapped modifier found, so the walk read nothing")
			}
		})
	}
	// No guarded resource has one inside a nested object or a block today,
	// so they're made here: the walk has to find both.
	t.Run("nested", func(t *testing.T) {
		replacing := func() schema.StringAttribute {
			return schema.StringAttribute{Optional: true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}}
		}
		s := schema.Schema{
			Attributes: map[string]schema.Attribute{"outer": schema.SingleNestedAttribute{Optional: true,
				Attributes: map[string]schema.Attribute{"inner": replacing()}}},
			Blocks: map[string]schema.Block{"rule": schema.ListNestedBlock{NestedObject: schema.NestedBlockObject{
				Attributes: map[string]schema.Attribute{"port": replacing()}}}},
		}
		deferReplacements(s.Attributes)
		bare, wrapped := servedReplacements(s)
		if wrapped != 1 || !slices.Equal(bare, []string{"rule.port: stringplanmodifier.requiresReplaceIfModifier"}) {
			t.Errorf("wrapped %d, bare %q: want the nested attribute wrapped and the block's reported", wrapped, bare)
		}
	})
}

// The values Terraform hands in are left as they were. The guard reads the
// opt-in from the configuration after the resource has planned, so a guard
// that took it out of the request it was given would refuse every permitted
// replacement.
func TestTheGuardLeavesTheRequestAsItWas(t *testing.T) {
	_, r, s := guardedStub(t)
	prior := stubValue(t, s, stubBase)
	planned := stubValue(t, s, with(stubBase, map[string]tftypes.Value{"id": str("s2"), allowReplaceAttribute: yes}))
	before := planned.Copy()
	planStub(t, r, s, prior, planned)
	if !planned.Equal(before) {
		t.Errorf("the configuration changed under the guard: %v", planned)
	}
}

// A create, an update and a read each leave the opt-in where Terraform
// expects it: as planned after a create or an update, and as it was after a
// read, which the platform knows nothing about. An import leaves it null, so
// an imported resource is guarded until the configuration says otherwise.
func TestTheGuardKeepsTheOptInInState(t *testing.T) {
	ctx := context.Background()
	stub, r, s := guardedStub(t)
	nullState := tfsdk.State{Raw: tftypes.NewValue(s.Type().TerraformType(ctx), nil), Schema: s}

	planned := stubValue(t, s, map[string]tftypes.Value{"id": str("s1"), "name": str("a"),
		"state": tftypes.NewValue(tftypes.String, tftypes.UnknownValue), allowReplaceAttribute: yes})
	created := resource.CreateResponse{State: nullState}
	r.Create(ctx, resource.CreateRequest{Config: tfsdk.Config{Raw: planned, Schema: s},
		Plan: tfsdk.Plan{Raw: planned, Schema: s}}, &created)
	if created.Diagnostics.HasError() || !optInOf(t, created.State).ValueBool() {
		t.Errorf("after create: %s = %v, %v", allowReplaceAttribute, optInOf(t, created.State), created.Diagnostics)
	}

	read := resource.ReadResponse{State: created.State}
	r.Read(ctx, resource.ReadRequest{State: created.State}, &read)
	var state types.String
	read.State.GetAttribute(ctx, path.Root("state"), &state)
	if read.Diagnostics.HasError() || !optInOf(t, read.State).ValueBool() || state.ValueString() != "read" {
		t.Errorf("after read: %s = %v, state = %v, %v", allowReplaceAttribute, optInOf(t, read.State), state, read.Diagnostics)
	}

	removed := stubValue(t, s, map[string]tftypes.Value{"id": str("s1"), "name": str("b"), "state": str("read")})
	updated := resource.UpdateResponse{State: read.State}
	r.Update(ctx, resource.UpdateRequest{Config: tfsdk.Config{Raw: removed, Schema: s},
		Plan: tfsdk.Plan{Raw: removed, Schema: s}, State: read.State}, &updated)
	if updated.Diagnostics.HasError() || !optInOf(t, updated.State).IsNull() {
		t.Errorf("after an update that removes it: %s = %v, %v", allowReplaceAttribute, optInOf(t, updated.State), updated.Diagnostics)
	}

	imported := resource.ImportStateResponse{State: nullState}
	importer, ok := r.(resource.ResourceWithImportState)
	if !ok {
		t.Fatal("a guarded resource has no ImportState")
	}
	importer.ImportState(ctx, resource.ImportStateRequest{ID: "s9"}, &imported)
	var id types.String
	imported.State.GetAttribute(ctx, path.Root("id"), &id)
	if imported.Diagnostics.HasError() || !optInOf(t, imported.State).IsNull() || id.ValueString() != "s9" {
		t.Errorf("after import: %s = %v, id = %v, %v", allowReplaceAttribute, optInOf(t, imported.State), id, imported.Diagnostics)
	}

	stub.gone = true
	gone := resource.ReadResponse{State: created.State}
	r.Read(ctx, resource.ReadRequest{State: created.State}, &gone)
	if !gone.State.Raw.IsNull() {
		t.Errorf("a resource its Read found gone is still in state: %v", gone.State.Raw)
	}

	if want := []string{"Create", "Read", "Update", "ImportState", "Read"}; !reflect.DeepEqual(stub.calls, want) {
		t.Errorf("calls = %q, want %q", stub.calls, want)
	}
}

// Setting or removing the opt-in on its own is answered without calling the
// resource: prior state, with the planned opt-in.
func TestAnUpdateOfTheOptInAloneNeverReachesTheResource(t *testing.T) {
	ctx := context.Background()
	stub, r, s := guardedStub(t)
	prior := stubValue(t, s, stubBase)
	planned := stubValue(t, s, with(stubBase, map[string]tftypes.Value{allowReplaceAttribute: yes}))
	resp := resource.UpdateResponse{State: tfsdk.State{Raw: prior, Schema: s}}
	r.Update(ctx, resource.UpdateRequest{Config: tfsdk.Config{Raw: planned, Schema: s},
		Plan: tfsdk.Plan{Raw: planned, Schema: s}, State: tfsdk.State{Raw: prior, Schema: s}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if len(stub.calls) != 0 {
		t.Errorf("calls = %q, want none", stub.calls)
	}
	if !resp.State.Raw.Equal(planned) {
		t.Errorf("state = %v, want prior state with the opt-in", resp.State.Raw)
	}
}

// The same on every guarded resource, against a platform that counts what
// it's sent: a change to the opt-in alone, with nothing else planned unknown,
// makes no request at all, not even a read. Where the plan leaves a computed
// value unknown, such as a network's `state`, the resource's own Update runs
// to fill it, as it did before the guard was one seam.
func TestEveryGuardedResourceAnswersAnOptInOnlyUpdateWithoutARequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Errorf("sent %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	meta := testClusterMeta(t, server.URL, time.Second)

	ctx := context.Background()
	for suffix, newResource := range tenantStateResources {
		t.Run(suffix, func(t *testing.T) {
			r := newResource()
			var configured resource.ConfigureResponse
			configurer, ok := r.(resource.ResourceWithConfigure)
			if !ok {
				t.Fatal("a guarded resource has no Configure")
			}
			configurer.Configure(ctx, resource.ConfigureRequest{ProviderData: meta}, &configured)
			s := schemaOf(t, r)
			values := map[string]tftypes.Value{"id": str("x1")}
			for _, scope := range []string{"region", "project"} {
				if _, ok := s.Attributes[scope]; ok {
					values[scope] = str("test")
				}
			}
			for _, set := range []map[string]tftypes.Value{
				{allowReplaceAttribute: yes},
				{allowReplaceAttribute: tftypes.NewValue(tftypes.Bool, false)},
			} {
				prior, _ := testValue(t, r, values)
				planned, _ := testValue(t, r, with(values, set))
				resp := resource.UpdateResponse{State: tfsdk.State{Raw: prior, Schema: s}}
				r.Update(ctx, resource.UpdateRequest{Config: tfsdk.Config{Raw: planned, Schema: s},
					Plan: tfsdk.Plan{Raw: planned, Schema: s}, State: tfsdk.State{Raw: prior, Schema: s}}, &resp)
				if resp.Diagnostics.HasError() {
					t.Errorf("update: %v", resp.Diagnostics)
				}
				if !resp.State.Raw.Equal(planned) {
					t.Errorf("state = %v, want prior state with the planned opt-in", resp.State.Raw)
				}
			}
		})
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests, want none", n)
	}
}

// An update the resource fails keeps the opt-in prior state had, the way
// each resource's own Update left it before the guard was one seam: the
// change the opt-in came with didn't happen.
func TestAFailedUpdateKeepsThePriorOptIn(t *testing.T) {
	ctx := context.Background()
	stub, r, s := guardedStub(t)
	stub.updateFails = true
	prior := stubValue(t, s, stubBase)
	planned := stubValue(t, s, with(stubBase, map[string]tftypes.Value{"name": str("b"), allowReplaceAttribute: yes}))
	resp := resource.UpdateResponse{State: tfsdk.State{Raw: prior, Schema: s}}
	r.Update(ctx, resource.UpdateRequest{Config: tfsdk.Config{Raw: planned, Schema: s},
		Plan: tfsdk.Plan{Raw: planned, Schema: s}, State: tfsdk.State{Raw: prior, Schema: s}}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("the stub's failure was lost")
	}
	if got := optInOf(t, resp.State); !got.IsNull() {
		t.Errorf("%s = %v after a failed update, want the prior null", allowReplaceAttribute, got)
	}
}

// An Update that drops the resource from state is a bug the destroy rule
// keeps out of every real resource. The guard doesn't pass it on: the
// resource stays in state as it was, and the apply fails saying why.
func TestAnUpdateThatDropsTheResourceKeepsItInState(t *testing.T) {
	ctx := context.Background()
	stub, r, s := guardedStub(t)
	stub.updateDrops = true
	prior := stubValue(t, s, stubBase)
	planned := stubValue(t, s, with(stubBase, map[string]tftypes.Value{"name": str("b")}))
	resp := resource.UpdateResponse{State: tfsdk.State{Raw: prior, Schema: s}}
	r.Update(ctx, resource.UpdateRequest{Config: tfsdk.Config{Raw: planned, Schema: s},
		Plan: tfsdk.Plan{Raw: planned, Schema: s}, State: tfsdk.State{Raw: prior, Schema: s}}, &resp)
	if !resp.State.Raw.Equal(prior) {
		t.Errorf("state = %v, want prior state", resp.State.Raw)
	}
	if got := errorSummaries(resp.Diagnostics); len(got) != 1 || !strings.Contains(got[0], "dropped") {
		t.Errorf("errors = %q, want one saying the update dropped the resource", got)
	}
}

// A replacement the resource plans on an attribute that isn't a string is
// refused by name, like any other.
func TestTheGuardWarnsOfAReplacementOfANumber(t *testing.T) {
	stub, r, s := guardedStub(t)
	size := path.Root("size")
	stub.replaceInPlan = &size
	base := with(stubBase, map[string]tftypes.Value{"size": tftypes.NewValue(tftypes.Number, 1)})
	resp := planStub(t, r, s, stubValue(t, s, base),
		stubValue(t, s, with(base, map[string]tftypes.Value{"size": tftypes.NewValue(tftypes.Number, 2)})))
	got := replacementWarnings(resp.Diagnostics)
	if len(got) != 1 || got[0] != "Changing `size` would replace this stub, so the apply will refuse it" {
		t.Fatalf("warnings = %q, want the warning naming size", got)
	}
	if d := resp.Diagnostics.Warnings()[0].Detail(); !strings.Contains(d, "goes from 1 to 2") {
		t.Errorf("the warning doesn't say what changed: %q", d)
	}
}
