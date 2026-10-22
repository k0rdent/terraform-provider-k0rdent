package provider

import (
	"context"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// Plans that show only what changes.
//
// On any update the framework plans every Computed attribute the
// configuration leaves out as unknown, so a rename shows a network's state and
// a security group's rule count as "known after apply", though neither can
// move. What's here puts back what the apply is certain to return, and only
// that: a planned value the apply then contradicts fails it with "Provider
// produced inconsistent result after apply", which is worse than an unknown.
//
// Values that never change after a create take UseStateForUnknown. Values
// that change only with particular inputs take keepUnlessChanged, or a
// modifier below that works the new value out. A lifecycle state takes
// planSettledState in ModifyPlan, and the update reads it back through
// settledRead, which is what makes the planned value certain.

// inputsChanged reports whether any of the named top-level attributes differs
// between the plan and prior state. A value the plan doesn't know yet counts
// as changed.
func inputsChanged(ctx context.Context, plan tfsdk.Plan, state tfsdk.State, inputs []string, diags *diag.Diagnostics) bool {
	for _, name := range inputs {
		var planned, prior attr.Value
		diags.Append(plan.GetAttribute(ctx, path.Root(name), &planned)...)
		diags.Append(state.GetAttribute(ctx, path.Root(name), &prior)...)
		if diags.HasError() || planned == nil || prior == nil {
			return true
		}
		if planned.IsUnknown() || !planned.Equal(prior) {
			return true
		}
	}
	return false
}

// keepUnless keeps an attribute's prior value through an update that leaves
// every one of its inputs as they were. Build it with keepUnlessChanged.
type keepUnless struct {
	inputs []string
	// nullOnly keeps the prior value only when it's null, for a value that
	// can also move with time, such as a window that closes.
	nullOnly bool
	// settledBy keeps the prior value only when prior state's `state` is one
	// this table settles on, for a value the platform fills in while the
	// resource settles.
	settledBy *stateTable
}

// keepUnlessChanged keeps the prior value unless one of the named top-level
// attributes changes. Every input has to be one that the attribute can only
// move with, through this resource's update.
func keepUnlessChanged(inputs ...string) keepUnless {
	return keepUnless{inputs: inputs}
}

// whileNull keeps the prior value only while it's null.
func (k keepUnless) whileNull() keepUnless {
	k.nullOnly = true
	return k
}

// onceSettled keeps the prior value only once the resource has settled. One
// imported while it was still being made can have a value still to come.
func (k keepUnless) onceSettled(table stateTable) keepUnless {
	k.settledBy = &table
	return k
}

func (k keepUnless) Description(context.Context) string {
	s := "Keeps its value through an update"
	if k.nullOnly {
		s = "Stays null through an update"
	}
	if k.settledBy != nil {
		s += " once the resource has settled"
	}
	if len(k.inputs) > 0 {
		s += " unless `" + strings.Join(k.inputs, "`, `") + "` changes"
	}
	return s + "."
}

func (k keepUnless) MarkdownDescription(ctx context.Context) string { return k.Description(ctx) }

// keeps decides for every kind of attribute.
func (k keepUnless) keeps(ctx context.Context, plan tfsdk.Plan, state tfsdk.State, config, planned, prior attr.Value, diags *diag.Diagnostics) bool {
	if state.Raw.IsNull() || plan.Raw.IsNull() || !planned.IsUnknown() || config.IsUnknown() {
		return false
	}
	if k.nullOnly && !prior.IsNull() {
		return false
	}
	if k.settledBy != nil && !settledBefore(ctx, *k.settledBy, state, diags) {
		return false
	}
	return !inputsChanged(ctx, plan, state, k.inputs, diags)
}

// settledBefore reports whether prior state's `state` is one the table
// settles on.
func settledBefore(ctx context.Context, table stateTable, state tfsdk.State, diags *diag.Diagnostics) bool {
	var prior types.String
	diags.Append(state.GetAttribute(ctx, path.Root("state"), &prior)...)
	return !diags.HasError() && !prior.IsNull() && !prior.IsUnknown() && table.settles(prior.ValueString())
}

func (k keepUnless) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if k.keeps(ctx, req.Plan, req.State, req.ConfigValue, req.PlanValue, req.StateValue, &resp.Diagnostics) {
		resp.PlanValue = req.StateValue
	}
}

func (k keepUnless) PlanModifyInt64(ctx context.Context, req planmodifier.Int64Request, resp *planmodifier.Int64Response) {
	if k.keeps(ctx, req.Plan, req.State, req.ConfigValue, req.PlanValue, req.StateValue, &resp.Diagnostics) {
		resp.PlanValue = req.StateValue
	}
}

func (k keepUnless) PlanModifyBool(ctx context.Context, req planmodifier.BoolRequest, resp *planmodifier.BoolResponse) {
	if k.keeps(ctx, req.Plan, req.State, req.ConfigValue, req.PlanValue, req.StateValue, &resp.Diagnostics) {
		resp.PlanValue = req.StateValue
	}
}

func (k keepUnless) PlanModifyList(ctx context.Context, req planmodifier.ListRequest, resp *planmodifier.ListResponse) {
	if k.keeps(ctx, req.Plan, req.State, req.ConfigValue, req.PlanValue, req.StateValue, &resp.Diagnostics) {
		resp.PlanValue = req.StateValue
	}
}

func (k keepUnless) PlanModifyMap(ctx context.Context, req planmodifier.MapRequest, resp *planmodifier.MapResponse) {
	if k.keeps(ctx, req.Plan, req.State, req.ConfigValue, req.PlanValue, req.StateValue, &resp.Diagnostics) {
		resp.PlanValue = req.StateValue
	}
}

func (k keepUnless) PlanModifyObject(ctx context.Context, req planmodifier.ObjectRequest, resp *planmodifier.ObjectResponse) {
	if k.keeps(ctx, req.Plan, req.State, req.ConfigValue, req.PlanValue, req.StateValue, &resp.Diagnostics) {
		resp.PlanValue = req.StateValue
	}
}

// settles reports whether a state ends a wait that's settling a change.
func (t stateTable) settles(state string) bool {
	switch t.states[state].verdict {
	case settled, settledOnceReady:
		return true
	}
	return false
}

// planSettledState plans `state` for an update, from ModifyPlan. Every update
// reads the resource back through settledRead, so one that starts settled
// ends settled, and the plan says which state that is rather than "known
// after apply". A resource that starts anywhere else, or reported no state,
// is left unknown. A create, a destroy and a plan with no change are left
// alone.
func planSettledState(ctx context.Context, table stateTable, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() || resp.Plan.Raw.Equal(req.State.Raw) {
		return
	}
	var prior types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("state"), &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	planned := types.StringUnknown()
	if !prior.IsNull() && !prior.IsUnknown() && table.settles(prior.ValueString()) {
		planned = prior
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("state"), planned)...)
}

// settledRead is the read an update ends with. A plan from planSettledState
// promised a settled state, so a read that finds the resource busy, because
// the platform took the change up after the wait last looked, waits for it to
// settle and reads again rather than return a state the plan didn't promise.
// A plan that left the state unknown promised nothing, and gets the first
// read. The error is the wait's, for the caller to report its own way.
func settledRead[M any](ctx context.Context, planned types.String, table stateTable,
	read func(context.Context) (M, bool, diag.Diagnostics), stateOf func(M) types.String,
	wait func(context.Context) error) (M, bool, diag.Diagnostics, error) {
	for {
		m, found, diags := read(ctx)
		if diags.HasError() || !found || planned.IsUnknown() || planned.IsNull() {
			return m, found, diags, nil
		}
		got := stateOf(m)
		if got.IsNull() || got.IsUnknown() || got.Equal(planned) || table.settles(got.ValueString()) {
			return m, found, diags, nil
		}
		if err := wait(ctx); err != nil {
			return m, found, diags, err
		}
	}
}

// ruleCount plans a security group's `rule_count`. The contract derives it as
// the ingress list's length plus the egress list's on the same read, so a
// change to the rules plans the new count. A group the platform reported no
// count for stays unknown when its rules change.
type ruleCount struct{}

func (ruleCount) Description(context.Context) string {
	return "Planned as the number of `ingress` and `egress` rules."
}

func (r ruleCount) MarkdownDescription(ctx context.Context) string { return r.Description(ctx) }

func (ruleCount) PlanModifyInt64(ctx context.Context, req planmodifier.Int64Request, resp *planmodifier.Int64Response) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() || !req.PlanValue.IsUnknown() || req.ConfigValue.IsUnknown() {
		return
	}
	lists := []string{"ingress", "egress"}
	if !inputsChanged(ctx, req.Plan, req.State, lists, &resp.Diagnostics) {
		resp.PlanValue = req.StateValue
		return
	}
	if req.StateValue.IsNull() {
		return
	}
	total := 0
	for _, name := range lists {
		var rules types.List
		resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root(name), &rules)...)
		if resp.Diagnostics.HasError() || rules.IsUnknown() {
			return
		}
		total += len(rules.Elements())
	}
	resp.PlanValue = types.Int64Value(int64(total))
}

// attachedGroups plans a VPC binding's `attached`. The read writes what the
// platform reports into `security_groups` whenever the list sent wasn't empty,
// and Terraform holds that to the plan, so for a list with entries `attached`
// is the list itself. An empty list is the VPC's default group, which only a
// read can name, so that's kept only when the list was empty already.
type attachedGroups struct{}

func (attachedGroups) Description(context.Context) string {
	return "Planned as `security_groups` when that names any group."
}

func (a attachedGroups) MarkdownDescription(ctx context.Context) string { return a.Description(ctx) }

func (attachedGroups) PlanModifyList(ctx context.Context, req planmodifier.ListRequest, resp *planmodifier.ListResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() || !req.PlanValue.IsUnknown() || req.ConfigValue.IsUnknown() {
		return
	}
	if !inputsChanged(ctx, req.Plan, req.State, []string{"security_groups"}, &resp.Diagnostics) {
		resp.PlanValue = req.StateValue
		return
	}
	var groups types.List
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("security_groups"), &groups)...)
	if resp.Diagnostics.HasError() || groups.IsNull() || groups.IsUnknown() || len(groups.Elements()) == 0 {
		return
	}
	for _, g := range groups.Elements() {
		if g.IsUnknown() {
			return
		}
	}
	resp.PlanValue = groups
}

// keptGrantStatus plans a file store's `consumer_status` through an update that
// sends no consumers and doesn't resize the store. It keeps the prior value
// only for a settled store whose every consumer has an `active` grant to a
// single address, and nothing else: a group's grant moves with the group's
// membership, an entry that isn't `active` is one the platform is still
// working on, and a store still being made has grants still to report.
type keptGrantStatus struct{ settledBy stateTable }

func (keptGrantStatus) Description(context.Context) string {
	return "Keeps its value through an update that leaves `consumers` and `capacity_gib` as they are, " +
		"while every grant is an active one to an address."
}

func (g keptGrantStatus) MarkdownDescription(ctx context.Context) string { return g.Description(ctx) }

func (g keptGrantStatus) PlanModifyMap(ctx context.Context, req planmodifier.MapRequest, resp *planmodifier.MapResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() || !req.PlanValue.IsUnknown() || req.ConfigValue.IsUnknown() {
		return
	}
	if !settledBefore(ctx, g.settledBy, req.State, &resp.Diagnostics) ||
		inputsChanged(ctx, req.Plan, req.State, []string{"consumers", "capacity_gib"}, &resp.Diagnostics) {
		return
	}
	var consumers types.Map
	resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root("consumers"), &consumers)...)
	if resp.Diagnostics.HasError() {
		return
	}
	entries, statuses := consumers.Elements(), req.StateValue.Elements()
	if len(entries) != len(statuses) {
		return
	}
	for ref, status := range statuses {
		if !activeAddressGrant(status, entries[ref]) {
			return
		}
	}
	resp.PlanValue = req.StateValue
}

// activeAddressGrant reports whether a consumer_status entry is an `active`
// grant, with no reason given, of an `ipAddress` entry in `consumers`.
func activeAddressGrant(status, consumer attr.Value) bool {
	s, ok := status.(types.Object)
	if !ok || s.IsNull() || s.IsUnknown() {
		return false
	}
	c, ok := consumer.(types.Object)
	if !ok || c.IsNull() || c.IsUnknown() {
		return false
	}
	state, okState := s.Attributes()["state"].(types.String)
	reason, okReason := s.Attributes()["state_reason"].(types.String)
	kind, okKind := c.Attributes()["type"].(types.String)
	return okState && okReason && okKind &&
		state.ValueString() == string(k0rdent.FileStoreConsumerStatusStateActive) && reason.IsNull() &&
		kind.ValueString() == string(k0rdent.ConsumerTypeIpAddress)
}
