package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
		"unless `allow_replace = true` is set"} {
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
	modifier, ok := r.(resource.ResourceWithModifyPlan)
	if !ok {
		t.Fatal("a guarded resource has no ModifyPlan")
	}
	modifier.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: planned, Schema: s},
		Plan:   tfsdk.Plan{Raw: planned, Schema: s},
		State:  tfsdk.State{Raw: prior, Schema: s},
	}, &resp)
	return resp
}

// The refusal, through the guard: a replacing change fails the plan naming
// the attribute, the opt-in lets it through, and an opt-in known only at
// apply doesn't. The resource's own ModifyPlan runs first and never sees the
// opt-in.
func TestTheGuardRefusesAReplacementTheResourceDoesntKnowAbout(t *testing.T) {
	stub, r, s := guardedStub(t)
	prior := stubValue(t, s, stubBase)

	refused := planStub(t, r, s, prior, stubValue(t, s, with(stubBase, map[string]tftypes.Value{"id": str("s2")})))
	if got := errorSummaries(refused.Diagnostics); len(got) != 1 || got[0] != "Changing `id` would replace this stub" {
		t.Errorf("errors = %q, want the refusal naming id", got)
	}
	if len(stub.calls) != 1 || stub.calls[0] != "ModifyPlan" {
		t.Errorf("calls = %q, want the resource's ModifyPlan", stub.calls)
	}

	permitted := planStub(t, r, s, prior, stubValue(t, s, with(stubBase, map[string]tftypes.Value{
		"id": str("s2"), allowReplaceAttribute: yes})))
	if permitted.Diagnostics.HasError() {
		t.Errorf("refused with the opt-in: %q", errorSummaries(permitted.Diagnostics))
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
	if !unknown.Diagnostics.HasError() {
		t.Error("an opt-in known only at apply let the replacement through")
	}

	inPlace := planStub(t, r, s, prior, stubValue(t, s, with(stubBase, map[string]tftypes.Value{"name": str("b")})))
	if inPlace.Diagnostics.HasError() {
		t.Errorf("an in-place change was refused: %q", errorSummaries(inPlace.Diagnostics))
	}
}

// A replacement the resource decides in its own ModifyPlan, the way a moved
// region is, is refused too, because the guard runs after it.
func TestTheGuardRefusesAReplacementTheResourcePlans(t *testing.T) {
	stub, r, s := guardedStub(t)
	name := path.Root("name")
	stub.replaceInPlan = &name
	prior := stubValue(t, s, stubBase)
	resp := planStub(t, r, s, prior, stubValue(t, s, with(stubBase, map[string]tftypes.Value{"name": str("b")})))
	if got := errorSummaries(resp.Diagnostics); len(got) != 1 || got[0] != "Changing `name` would replace this stub" {
		t.Errorf("errors = %q, want the refusal naming name", got)
	}
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
func TestTheGuardRefusesAReplacementOfANumber(t *testing.T) {
	stub, r, s := guardedStub(t)
	size := path.Root("size")
	stub.replaceInPlan = &size
	base := with(stubBase, map[string]tftypes.Value{"size": tftypes.NewValue(tftypes.Number, 1)})
	resp := planStub(t, r, s, stubValue(t, s, base),
		stubValue(t, s, with(base, map[string]tftypes.Value{"size": tftypes.NewValue(tftypes.Number, 2)})))
	got := errorSummaries(resp.Diagnostics)
	if len(got) != 1 || got[0] != "Changing `size` would replace this stub" {
		t.Fatalf("errors = %q, want the refusal naming size", got)
	}
	if d := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(d, "goes from 1 to 2") {
		t.Errorf("the refusal doesn't say what changed: %q", d)
	}
}
