package provider

import (
	"context"
	"fmt"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// The replacement guard as one seam.
//
// A tenant-state resource is registered wrapped: guardReplacements(&r{},
// what). The wrapped resource never sees `allow_replace`. Its schema, its
// model and every value it's handed leave the attribute out, and the wrapper
// owns everything about it:
//
//   - the attribute itself and the documentation that says what replaces the
//     resource and how to permit it (declareReplacementGuard);
//   - the plan-time refusal, run after the resource's own ModifyPlan, against
//     the plan as Terraform will act on it (guardReplacement);
//   - the opt-in's value in state: the plan's after a create or an update, the
//     prior state's after a read, and null after an import, which is what each
//     resource carried by hand before;
//   - an update that changes nothing but the opt-in and leaves nothing
//     unknown, answered here without calling the resource, so it makes no
//     request.
//
// So a resource can't hold the opt-in without the refusal, carry it into state
// wrongly, or send it to the platform, because it can't see it. A resource
// that should be guarded and isn't registered this way fails
// TestEveryReplaceableResourceIsGuardedOrSaysWhy.

// guardable is what a guarded resource implements. Every tenant-state
// resource does, and asking for it here means the wrapper never has to stand
// in for an import or an identity the resource doesn't have.
type guardable interface {
	resource.ResourceWithConfigure
	resource.ResourceWithImportState
	resource.ResourceWithIdentity
	resource.ResourceWithUpgradeState
}

// guardReplacements registers a tenant-state resource behind the replacement
// guard.
func guardReplacements(inner guardable, what tenantState) resource.Resource {
	g := &guardedResource{inner: inner, what: what}
	if mover, ok := inner.(resource.ResourceWithMoveState); ok {
		return guardedMovableResource{g, mover}
	}
	return g
}

// guardedResource is a tenant-state resource behind the replacement guard.
type guardedResource struct {
	inner guardable
	what  tenantState
	// meta is the provider's, kept for the identity an update answered here
	// has to carry.
	meta *Meta

	shapeOnce sync.Once
	shape     optInShape
}

var (
	_ resource.ResourceWithConfigure      = (*guardedResource)(nil)
	_ resource.ResourceWithImportState    = (*guardedResource)(nil)
	_ resource.ResourceWithIdentity       = (*guardedResource)(nil)
	_ resource.ResourceWithUpgradeState   = (*guardedResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*guardedResource)(nil)
	_ resource.ResourceWithValidateConfig = (*guardedResource)(nil)
	_ resource.ResourceWithMoveState      = guardedMovableResource{}
)

// guardedMovableResource is a guarded resource that accepts a `moved` block.
// It's a type of its own because the framework tells a resource that takes
// moves from one that doesn't by the method alone.
type guardedMovableResource struct {
	*guardedResource
	mover resource.ResourceWithMoveState
}

// MoveState is the resource's own, handed the guarded schema the way
// UpgradeState is. A moved state never had the opt-in, so it reads null.
func (g guardedMovableResource) MoveState(ctx context.Context) []resource.StateMover {
	return g.mover.MoveState(ctx)
}

// guardedTenantState reports what a guarded resource holds. It's how a test
// tells a guarded resource from one that isn't.
func (g *guardedResource) guardedTenantState() tenantState { return g.what }

func (g *guardedResource) Metadata(ctx context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	g.inner.Metadata(ctx, req, resp)
}

// Schema is the resource's, with the opt-in and the documentation added.
func (g *guardedResource) Schema(ctx context.Context, req resource.SchemaRequest, resp *resource.SchemaResponse) {
	g.inner.Schema(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		return
	}
	declareReplacementGuard(&resp.Schema, g.what)
}

func (g *guardedResource) Configure(ctx context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	g.inner.Configure(ctx, req, resp)
	// configureMeta, called by the resource, reports a wrong type.
	if meta, ok := req.ProviderData.(*Meta); ok {
		g.meta = meta
	}
}

func (g *guardedResource) IdentitySchema(ctx context.Context, req resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	g.inner.IdentitySchema(ctx, req, resp)
}

// UpgradeState is the resource's own, handed the guarded schema. Every
// upgrader rewrites the stored JSON and decodes it into that schema
// (state_history.go), so a stored opt-in survives. One that read the state
// into the resource's model instead would meet an attribute the model
// doesn't have, and fail.
func (g *guardedResource) UpgradeState(ctx context.Context) map[int64]resource.StateUpgrader {
	return g.inner.UpgradeState(ctx)
}

func (g *guardedResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	v, ok := g.inner.(resource.ResourceWithValidateConfig)
	if !ok {
		return
	}
	in := resource.ValidateConfigRequest{Config: g.hideConfig(ctx, req.Config, &resp.Diagnostics),
		ClientCapabilities: req.ClientCapabilities}
	if resp.Diagnostics.HasError() {
		return
	}
	v.ValidateConfig(ctx, in, resp)
}

// ModifyPlan runs the resource's own ModifyPlan, then the guard. The guard
// goes last so that it judges the plan Terraform will act on, with region and
// project already resolved and every value the resource set in place.
func (g *guardedResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if m, ok := g.inner.(resource.ResourceWithModifyPlan); ok {
		in := resource.ModifyPlanRequest{
			Config:             g.hideConfig(ctx, req.Config, &resp.Diagnostics),
			State:              g.hideState(ctx, req.State, &resp.Diagnostics),
			Identity:           req.Identity,
			Plan:               g.hidePlan(ctx, req.Plan, &resp.Diagnostics),
			ProviderMeta:       req.ProviderMeta,
			Private:            req.Private,
			ClientCapabilities: req.ClientCapabilities,
		}
		out := resource.ModifyPlanResponse{
			Plan:            g.hidePlan(ctx, resp.Plan, &resp.Diagnostics),
			Identity:        resp.Identity,
			RequiresReplace: resp.RequiresReplace,
			Private:         resp.Private,
			Diagnostics:     resp.Diagnostics,
			Deferred:        resp.Deferred,
		}
		if resp.Diagnostics.HasError() {
			return // fails closed: the plan is refused
		}
		m.ModifyPlan(ctx, in, &out)
		resp.Identity, resp.RequiresReplace, resp.Private, resp.Diagnostics, resp.Deferred =
			out.Identity, out.RequiresReplace, out.Private, out.Diagnostics, out.Deferred
		if !out.Plan.Raw.IsNull() {
			g.carry(ctx, out.Plan, &resp.Plan, &resp.Diagnostics)
		}
	}
	guardReplacement(ctx, g.what, req, resp)
	if c, ok := g.inner.(createGuard); ok && req.State.Raw.IsNull() && !resp.Plan.Raw.IsNull() &&
		!resp.Diagnostics.HasError() && !replacementAllowed(ctx, req.Config) {
		c.refuseExisting(ctx, resp.Plan, &resp.Diagnostics)
	}
}

// createGuard is a kind whose plan to create reads the configured id first.
//
// Terraform plans the replacement of a tainted object as a create, with no
// prior state, and `terraform destroy` runs that same plan first to refresh
// state, so a plan can't tell a replacement, a destroy's refresh or a new
// object apart. A refusal here blocks the destroy of a tainted object, so
// one a Terraform apply made passes, in every state, with a warning that says
// what applying the plan does to it and the untaint that keeps it instead. A
// cluster shows that by this provider's create token. An instance group has
// no tags, so it passes on its state alone. A cluster without the token was
// made some other way and is refused: the whole plan fails, so nothing is
// destroyed. One that's deleting passes. `allow_replace` opts out, which is
// how a deliberate replacement of a cluster made elsewhere gets through. See
// resume.go.
type createGuard interface {
	refuseExisting(ctx context.Context, plan tfsdk.Plan, diags *diag.Diagnostics)
}

func (g *guardedResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	optIn := g.optIn(ctx, req.Plan, &resp.Diagnostics)
	in := resource.CreateRequest{
		Config:       g.hideConfig(ctx, req.Config, &resp.Diagnostics),
		Plan:         g.hidePlan(ctx, req.Plan, &resp.Diagnostics),
		Identity:     req.Identity,
		ProviderMeta: req.ProviderMeta,
	}
	out := resource.CreateResponse{
		State:       g.hideState(ctx, resp.State, &resp.Diagnostics),
		Identity:    resp.Identity,
		Private:     resp.Private,
		Diagnostics: resp.Diagnostics,
	}
	if resp.Diagnostics.HasError() {
		return
	}
	g.inner.Create(ctx, in, &out)
	resp.Identity, resp.Private, resp.Diagnostics = out.Identity, out.Private, out.Diagnostics
	if !out.State.Raw.IsNull() {
		g.carry(ctx, out.State, &resp.State, &resp.Diagnostics)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(allowReplaceAttribute), optIn)...)
	}
}

// Read keeps the opt-in the prior state had: it's configuration, and the
// platform knows nothing of it.
func (g *guardedResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	in := resource.ReadRequest{
		State:              g.hideState(ctx, req.State, &resp.Diagnostics),
		Identity:           req.Identity,
		Private:            req.Private,
		ProviderMeta:       req.ProviderMeta,
		ClientCapabilities: req.ClientCapabilities,
	}
	out := resource.ReadResponse{
		State:       g.hideState(ctx, resp.State, &resp.Diagnostics),
		Identity:    resp.Identity,
		Private:     resp.Private,
		Diagnostics: resp.Diagnostics,
		Deferred:    resp.Deferred,
	}
	if resp.Diagnostics.HasError() {
		return
	}
	g.inner.Read(ctx, in, &out)
	resp.Identity, resp.Private, resp.Diagnostics, resp.Deferred = out.Identity, out.Private, out.Diagnostics, out.Deferred
	if out.State.Raw.IsNull() {
		resp.State.RemoveResource(ctx) // the resource found it gone
		return
	}
	g.carry(ctx, out.State, &resp.State, &resp.Diagnostics)
}

// Update answers a change to the opt-in alone itself: prior state with the
// planned opt-in, and no request. Anything else goes to the resource, which
// sees the change without the opt-in in it.
func (g *guardedResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	optIn := g.optIn(ctx, req.Plan, &resp.Diagnostics)
	in := resource.UpdateRequest{
		Config:       g.hideConfig(ctx, req.Config, &resp.Diagnostics),
		Plan:         g.hidePlan(ctx, req.Plan, &resp.Diagnostics),
		State:        g.hideState(ctx, req.State, &resp.Diagnostics),
		Identity:     req.Identity,
		ProviderMeta: req.ProviderMeta,
		Private:      req.Private,
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if in.Plan.Raw.Equal(in.State.Raw) {
		// Every other value is planned as it stands, with none left unknown
		// for the resource to fill, so there's nothing for the platform to
		// change and nothing to read back. The framework starts the response
		// from prior state. The identity is synced the way the resource's
		// own Update would, for a row written before there was one.
		defer syncIdentity(ctx, g.meta, resp.Identity, &resp.Diagnostics, &resp.State)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(allowReplaceAttribute), optIn)...)
		return
	}
	out := resource.UpdateResponse{
		State:       g.hideState(ctx, resp.State, &resp.Diagnostics),
		Identity:    resp.Identity,
		Private:     resp.Private,
		Diagnostics: resp.Diagnostics,
	}
	if resp.Diagnostics.HasError() {
		return
	}
	g.inner.Update(ctx, in, &out)
	resp.Identity, resp.Private, resp.Diagnostics = out.Identity, out.Private, out.Diagnostics
	if out.State.Raw.IsNull() {
		// Local: no request made here. Only a destroy may drop a resource
		// from state (destroy_rule_internal_test.go keeps every Update from
		// doing it), so prior state is kept and the apply fails.
		resp.Diagnostics.AddError("The resource dropped itself from state during an update",
			"This is a bug in the provider. The resource is kept in state as it was before the update.")
		return
	}
	g.carry(ctx, out.State, &resp.State, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		// The change the opt-in came with didn't finish, so state keeps the
		// opt-in it had, as each resource's own Update left it before.
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(allowReplaceAttribute), optIn)...)
}

// Delete hands the destroy to the resource. Whatever state it leaves on a
// failed delete keeps the opt-in it had.
func (g *guardedResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	in := resource.DeleteRequest{
		State:        g.hideState(ctx, req.State, &resp.Diagnostics),
		Identity:     req.Identity,
		ProviderMeta: req.ProviderMeta,
		Private:      req.Private,
	}
	out := resource.DeleteResponse{
		State:       g.hideState(ctx, resp.State, &resp.Diagnostics),
		Identity:    resp.Identity,
		Private:     resp.Private,
		Diagnostics: resp.Diagnostics,
	}
	if resp.Diagnostics.HasError() {
		return
	}
	g.inner.Delete(ctx, in, &out)
	resp.Identity, resp.Private, resp.Diagnostics = out.Identity, out.Private, out.Diagnostics
	if out.State.Raw.IsNull() {
		resp.State.Raw = tftypes.NewValue(g.shapes(ctx).withIt, nil)
		return
	}
	g.carry(ctx, out.State, &resp.State, &resp.Diagnostics)
}

// ImportState leaves the opt-in null: an imported resource is guarded until
// the configuration says otherwise.
func (g *guardedResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	out := resource.ImportStateResponse{
		Diagnostics: resp.Diagnostics,
		State:       g.hideState(ctx, resp.State, &resp.Diagnostics),
		Identity:    resp.Identity,
		Private:     resp.Private,
		Deferred:    resp.Deferred,
	}
	if resp.Diagnostics.HasError() {
		return
	}
	g.inner.ImportState(ctx, req, &out)
	resp.Identity, resp.Private, resp.Diagnostics, resp.Deferred = out.Identity, out.Private, out.Diagnostics, out.Deferred
	if !out.State.Raw.IsNull() {
		g.carry(ctx, out.State, &resp.State, &resp.Diagnostics)
	}
}

// carry copies every attribute and block the resource wrote into the guarded
// value, one at a time. The opt-in is left as it was, and a value is never
// replaced whole, so carrying can't drop a resource from state: only Read
// and Delete do that, above, the way the destroy rule asks.
func (g *guardedResource) carry(ctx context.Context, from attributeGetter, to attributeSetter, diags *diag.Diagnostics) {
	s := g.shapes(ctx).inner
	names := make([]string, 0, len(s.Attributes)+len(s.Blocks))
	for name := range s.Attributes {
		names = append(names, name)
	}
	for name := range s.Blocks {
		names = append(names, name)
	}
	for _, name := range names {
		var v attr.Value
		diags.Append(from.GetAttribute(ctx, path.Root(name), &v)...)
		if v != nil {
			diags.Append(to.SetAttribute(ctx, path.Root(name), v)...)
		}
	}
}

// attributeGetter and attributeSetter are what tfsdk.State and tfsdk.Plan
// share.
type attributeGetter interface {
	GetAttribute(ctx context.Context, p path.Path, target interface{}) diag.Diagnostics
}

type attributeSetter interface {
	SetAttribute(ctx context.Context, p path.Path, val interface{}) diag.Diagnostics
}

// optIn reads the planned opt-in.
func (g *guardedResource) optIn(ctx context.Context, plan tfsdk.Plan, diags *diag.Diagnostics) types.Bool {
	var allow types.Bool
	if plan.Raw.IsNull() {
		return types.BoolNull()
	}
	diags.Append(plan.GetAttribute(ctx, path.Root(allowReplaceAttribute), &allow)...)
	return allow
}

func (g *guardedResource) hideConfig(ctx context.Context, c tfsdk.Config, diags *diag.Diagnostics) tfsdk.Config {
	raw, s := g.hide(ctx, c.Raw, diags)
	return tfsdk.Config{Raw: raw, Schema: s}
}

func (g *guardedResource) hidePlan(ctx context.Context, p tfsdk.Plan, diags *diag.Diagnostics) tfsdk.Plan {
	raw, s := g.hide(ctx, p.Raw, diags)
	return tfsdk.Plan{Raw: raw, Schema: s}
}

func (g *guardedResource) hideState(ctx context.Context, st tfsdk.State, diags *diag.Diagnostics) tfsdk.State {
	raw, s := g.hide(ctx, st.Raw, diags)
	return tfsdk.State{Raw: raw, Schema: s}
}

// optInShape is the resource's schema as it declares it, and the object types
// with and without the opt-in.
type optInShape struct {
	inner           schema.Schema
	without, withIt tftypes.Object
}

func (g *guardedResource) shapes(ctx context.Context) optInShape {
	g.shapeOnce.Do(func() {
		var resp resource.SchemaResponse
		g.inner.Schema(ctx, resource.SchemaRequest{}, &resp)
		// A schema is always an object. Were it not, every value would fail
		// newObject against the empty type, and the guard would fail closed.
		without, ok := resp.Schema.Type().TerraformType(ctx).(tftypes.Object)
		if !ok {
			return
		}
		withIt := tftypes.Object{AttributeTypes: make(map[string]tftypes.Type, len(without.AttributeTypes)+1)}
		for name, t := range without.AttributeTypes {
			withIt.AttributeTypes[name] = t
		}
		withIt.AttributeTypes[allowReplaceAttribute] = tftypes.Bool
		g.shape = optInShape{inner: resp.Schema, without: without, withIt: withIt}
	})
	return g.shape
}

// hide is a value in the guarded shape as the resource sees it: the opt-in
// left out, typed by the resource's own schema.
func (g *guardedResource) hide(ctx context.Context, v tftypes.Value, diags *diag.Diagnostics) (tftypes.Value, schema.Schema) {
	s := g.shapes(ctx)
	switch {
	case v.Type() == nil || v.IsNull():
		return tftypes.NewValue(s.without, nil), s.inner
	case !v.IsKnown():
		return tftypes.NewValue(s.without, tftypes.UnknownValue), s.inner
	}
	attrs, err := attributesOf(v)
	if err != nil {
		addShapeError(diags, err)
		return tftypes.NewValue(s.without, nil), s.inner
	}
	delete(attrs, allowReplaceAttribute)
	return newObject(s.without, attrs, diags), s.inner
}

// attributesOf is an object's attributes in a map of the caller's own. As
// hands back the map the value holds, so changing that one would change the
// request the framework passed in, and the guard reads the opt-in from it.
func attributesOf(v tftypes.Value) (map[string]tftypes.Value, error) {
	var held map[string]tftypes.Value
	if err := v.As(&held); err != nil {
		return nil, err
	}
	out := make(map[string]tftypes.Value, len(held)+1)
	for name, value := range held {
		out[name] = value
	}
	return out, nil
}

// newObject is tftypes.NewValue without the panic: a value that doesn't fit
// the type is an error, reported, rather than a crashed provider.
func newObject(t tftypes.Object, attrs map[string]tftypes.Value, diags *diag.Diagnostics) tftypes.Value {
	if err := tftypes.ValidateValue(t, attrs); err != nil {
		addShapeError(diags, err)
		return tftypes.NewValue(t, nil)
	}
	return tftypes.NewValue(t, attrs)
}

func addShapeError(diags *diag.Diagnostics, err error) {
	// Local: fails closed. Unreachable while the resource's schema is the
	// guarded one less the opt-in, and a value that can't be checked is
	// refused rather than passed on.
	diags.AddAttributeError(path.Root(allowReplaceAttribute), "The replacement guard could not read this resource's values",
		fmt.Sprintf("This is a bug in the provider: %v", err))
}
