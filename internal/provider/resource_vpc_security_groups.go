package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ resource.Resource                 = (*vpcSecurityGroupsResource)(nil)
	_ resource.ResourceWithConfigure    = (*vpcSecurityGroupsResource)(nil)
	_ resource.ResourceWithImportState  = (*vpcSecurityGroupsResource)(nil)
	_ resource.ResourceWithIdentity     = (*vpcSecurityGroupsResource)(nil)
	_ resource.ResourceWithModifyPlan   = (*vpcSecurityGroupsResource)(nil)
	_ resource.ResourceWithUpgradeState = (*vpcSecurityGroupsResource)(nil)
)

const vpcSecurityGroupsResourceSuffix = "vpc_security_groups"

func newVPCSecurityGroupsResource() resource.Resource { return &vpcSecurityGroupsResource{} }

type vpcSecurityGroupsResource struct {
	meta *Meta

	// beforeClear, when set, runs in Delete between deciding to clear the
	// binding and clearing it. It's a seam for tests that force another
	// write into that gap, and nil otherwise.
	beforeClear func()
}

type vpcSecurityGroupsModel struct {
	VPC            types.String   `tfsdk:"vpc"`
	Region         types.String   `tfsdk:"region"`
	Project        types.String   `tfsdk:"project"`
	SecurityGroups types.List     `tfsdk:"security_groups"`
	Attached       types.List     `tfsdk:"attached"`
	State          types.String   `tfsdk:"state"`
	Timeouts       timeouts.Value `tfsdk:"timeouts"`

	// uid is the platform's uid for the VPC read, which own_object.go keeps
	// in private state. Not an attribute.
	uid string
}

func (r *vpcSecurityGroupsResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = ResourceName(vpcSecurityGroupsResourceSuffix)
}

// UpgradeState migrates a state written at an earlier schema version. See
// state_history.go.
func (r *vpcSecurityGroupsResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	return stateHistories[vpcSecurityGroupsResourceSuffix].upgraders()
}

// Schema declares the resource.
//
// A VPC has no create and no delete, so the binding is its own resource with
// the VPC's id. Sending an empty list reinstates the platform's default group
// rather than clearing the binding, and a VPC with no default to fall back to
// refuses the call with a 409. Only a `nico` VPC can carry groups at all:
// anything else is a 409 rather than recorded and ignored.
func (r *vpcSecurityGroupsResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Version: stateHistories[vpcSecurityGroupsResourceSuffix].version(),
		MarkdownDescription: "The security groups attached to a VPC, in the order the fabric applies " +
			"them.\n\n" +
			"A VPC is created with a cluster or instance group and has no lifecycle of its own, " +
			"so this binding is the only part of one that Terraform can write. It is a whole-list " +
			"replace: the groups named here are attached, and any attached before and absent now " +
			"are detached. A list without the VPC's default group replaces it, and with it the " +
			"default's rules for traffic between the VPC's subnets. To keep them, list the default " +
			"group's id too.\n\n" +
			"**Destroying this resource does not leave the VPC unfiltered.** It reinstates the " +
			"VPC's platform-managed DEFAULT group. From the k0rdent API v1.3.0-rc.3, a new VPC's " +
			"default group permits traffic between the VPC's own subnet prefixes and nothing " +
			"else. Traffic leaving the VPC needs a group of your own. A default group from an " +
			"earlier release also carries an allow-all egress rule, which never took effect on " +
			"the `nico` fabric.\n\n" +
			"From the API v1.3.0-rc.4, a VPC with no subnet prefixes, or more than 16, has no " +
			"default group, and its binding starts empty. The platform won't take the groups off " +
			"such a VPC once any are bound. A destroy leaves them on, warns and drops the resource " +
			"from state, and they stay until the VPC is deleted with its cluster or instance " +
			"group. A delete of one of those groups in the same run waits for that. The plan warns " +
			"when a binding is created on such a VPC, if the VPC is known by then, and the apply " +
			"warns again.\n\n" +
			"A replacement destroys the old binding, so it does either of these to the old VPC. The " +
			"plan warns about a destroy, and about a replacement that a change to `vpc`, `region` " +
			"or `project` causes. It can't warn about one forced by `-replace` or " +
			"`replace_triggered_by`, because Terraform decides those without asking the " +
			"provider.\n\n" +
			"A destroy only clears the binding this resource holds. It reads the VPC first, and " +
			"if the VPC holds anything but the list this resource last read, in the same order, " +
			"or holds a list another instance of this resource wrote earlier in the same apply, " +
			"it leaves the VPC as it is, drops the resource from state and warns. That's what " +
			"makes `create_before_destroy` safe on the same VPC: the new binding is written first, " +
			"and the old one's destroy finds it and leaves it, even when it names the same groups, " +
			"as `-replace` does. It also means a destroy doesn't undo a binding that another " +
			"configuration or the console set since the last refresh. Writes to the same VPC from " +
			"this apply wait for the destroy's read and write to finish. A change from anywhere " +
			"else that lands between them is still overwritten, because the platform has no " +
			"conditional write for a binding.\n\n" +
			"These groups are applied *after* the owning cluster or instance group's own groups, " +
			"so an interface inside this VPC filters on the owner's groups first. A group " +
			"attached to both is applied once, at the owner's position. Writing here re-renders " +
			"the owner's network bundles, which is why the owner goes transitional for a moment.\n\n" +
			"Only a VPC on the `nico` fabric backend can carry security groups. Read `backend` " +
			"from the `" + ResourceName(vpcDataSourceSuffix) + "` data source first.\n\n" +
			"Requires `region` and `project` on the provider.",
		Blocks: map[string]schema.Block{
			// Create and update are the same call, and delete makes it again
			// with the default. All three wait.
			"timeouts": timeouts.Block(ctx, timeouts.Opts{
				Create: true, Update: true, Delete: true,
			}),
		},
		Attributes: map[string]schema.Attribute{
			"region":  regionAttribute(),
			"project": projectAttribute(),
			"vpc": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Id of the VPC to bind. This is also the id this resource is " +
					"imported by, because a VPC has exactly one binding.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{urlSafeIdentifier{}},
			},
			// A List and not a Set, and this is the one place in the provider
			// where order is enforcement rather than presentation: the contract
			// says the list "reaches the fabric as sent", and rules are matched
			// first-match, so reordering two groups changes what is permitted.
			"security_groups": schema.ListAttribute{
				Required:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Ids of the groups to attach, in the order the fabric should " +
					"apply them. Order is significant: it reaches the fabric as written, and rule " +
					"evaluation is first-match.\n\n" +
					"An empty list is accepted and means *reinstate the default group*, not " +
					"*attach nothing*. A VPC with no default group refuses it with `409`.",
				Validators: []validator.List{
					listvalidator.SizeAtMost(k0rdent.VPCSecurityGroupsRequestSecurityGroupsMaxItems),
					idList("security group id"),
				},
			},
			// A List, in the same significant order as `security_groups`: this
			// is what the platform reports it will apply, and it round-trips
			// what was written.
			"attached": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "What the platform reports as attached after the write. This " +
					"differs from `security_groups` when the list sent was empty, because the " +
					"default group is then reported here explicitly.",
				PlanModifiers: []planmodifier.List{attachedGroups{}},
			},
			"state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The VPC's lifecycle state as last read. `updating` means the " +
					"binding has been accepted and is still being rendered onto the owner's " +
					"network bundles, so `attached` is what the platform will converge on rather " +
					"than necessarily what the fabric enforces yet.",
			},
		},
	}
	describeReplacement(&resp.Schema, "binding")
}

func (r *vpcSecurityGroupsResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

// What the DEFAULT group lets through, for the destroy and replace warnings.
const defaultGroupFilters = "From the k0rdent API v1.3.0-rc.3, a new VPC's default group " +
	"permits traffic between the VPC's own subnet prefixes and nothing else. So traffic between " +
	"those prefixes is permitted, and all other traffic, in or out, is denied unless the " +
	"cluster or instance group's own groups permit it. A default group from an earlier " +
	"release also carries an allow-all egress rule, which never took effect on the nico fabric."

// What a destroy does when a read before the plan couldn't tell whether the
// VPC has a default group.
const ifNoDefaultGroup = " If the VPC turns out to have no default group, as a VPC with no " +
	"subnet prefixes or more than 16 doesn't from the k0rdent API v1.3.0-rc.4, the groups stay " +
	"on it until it's deleted, and the resource is only dropped from state."

// warnNoDefaultOnCreate is the warning for a binding written to a VPC with
// no default group, which nothing can take off again.
func warnNoDefaultOnCreate(diags *diag.Diagnostics, vpc string) {
	diags.AddWarning(
		"This binding can't be taken off the VPC later",
		fmt.Sprintf("%q has no default security group to fall back to. %s Once this binding is "+
			"written, a destroy leaves its groups on %q, warns and drops the resource from state. "+
			"They stay until the VPC is deleted with its cluster or instance group.",
			vpc, noDefaultGroupWhy, vpc),
	)
}

// scopedDefaultGroup looks up the default group of the VPC an object of this
// resource names, when its vpc, region and project are all known.
func (r *vpcSecurityGroupsResource) scopedDefaultGroup(ctx context.Context, vpc, region, project types.String) defaultGroupState {
	if _, ok := known(vpc); !ok || region.IsUnknown() || project.IsUnknown() {
		return defaultGroupUnknown
	}
	var diags diag.Diagnostics
	target, ok := resolveScope(r.meta, region, project, &diags)
	if !ok {
		return defaultGroupUnknown
	}
	return vpcDefaultGroup(ctx, r.meta, target, vpc.ValueString())
}

func (r *vpcSecurityGroupsResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	modifyScopePlan(ctx, r.meta, req, resp)
	if resp.Diagnostics.HasError() {
		return
	}

	// A binding created on a VPC with no default group stays until the VPC
	// goes, which is worth knowing before it's written.
	if req.State.Raw.IsNull() && !req.Plan.Raw.IsNull() {
		var planned vpcSecurityGroupsModel
		resp.Diagnostics.Append(resp.Plan.Get(ctx, &planned)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if r.scopedDefaultGroup(ctx, planned.VPC, planned.Region, planned.Project) == defaultGroupMissing {
			warnNoDefaultOnCreate(&resp.Diagnostics, planned.VPC.ValueString())
		}
	}

	// Taking the binding off reinstates the VPC's platform-managed DEFAULT
	// group, which filters differently from the groups it replaces. A destroy
	// does that, and so does a replacement: Terraform destroys the old binding
	// whichever order it runs the two halves in. When the new one names
	// another VPC the old one is left on DEFAULT for good. On the same VPC
	// create_before_destroy runs the destroy last, and Delete finds the new
	// binding there and leaves it, so only the other order passes through
	// DEFAULT. A destroy plan is the one
	// place Terraform renders no provider text, so the warning is the only sign
	// of it there. A replacement forced by -replace or replace_triggered_by
	// never reaches this: Terraform decides it after ModifyPlan answers, and
	// the plan it hands over looks like any other.
	var prior types.String
	replacing, ok := plannedReplacements(ctx, req, resp)
	oldDefault := defaultGroupUnknown
	if !req.State.Raw.IsNull() {
		var priorState vpcSecurityGroupsModel
		resp.Diagnostics.Append(req.State.Get(ctx, &priorState)...)
		if resp.Diagnostics.HasError() {
			return
		}
		prior = priorState.VPC
		// Read only for the two plans that take the binding off.
		if req.Plan.Raw.IsNull() || !ok || len(replacing) > 0 {
			oldDefault = r.scopedDefaultGroup(ctx, priorState.VPC, priorState.Region, priorState.Project)
		}
	}
	if !req.State.Raw.IsNull() && req.Plan.Raw.IsNull() {
		if oldDefault == defaultGroupMissing {
			resp.Diagnostics.AddWarning(
				"Destroying this resource leaves the groups on the VPC",
				fmt.Sprintf("%q has no default security group to fall back to. %s So the destroy "+
					"leaves the tenant security groups on %q, warns and drops this resource from "+
					"state. They keep filtering the VPC until it's deleted with its cluster or "+
					"instance group.", prior.ValueString(), noDefaultGroupWhy, prior.ValueString()),
			)
			return
		}
		unsure := ""
		if oldDefault == defaultGroupUnknown {
			unsure = ifNoDefaultGroup
		}
		resp.Diagnostics.AddWarning(
			"Destroying this resource changes the VPC's filter",
			fmt.Sprintf("The tenant security groups come off %q and the VPC's platform-managed "+
				"DEFAULT group goes back on. "+defaultGroupFilters+" If the VPC holds anything "+
				"but what this resource last read by the time the destroy runs, it's left as it "+
				"is and the resource is only dropped from state."+unsure,
				prior.ValueString()),
		)
		return
	}
	if !ok || len(replacing) > 0 {
		// Local: decided from configuration and prior state. A schema that
		// can't be read is taken as a replacement, since the warning is the
		// cautious side of it.
		changed := "a change that can't be made in place"
		if len(replacing) > 0 {
			changed = "`" + replacing[0].path.String() + "` changing"
		}
		if oldDefault == defaultGroupMissing {
			resp.Diagnostics.AddWarning(
				"Replacing this resource leaves the old groups on the VPC",
				fmt.Sprintf("This plan replaces the binding on %q, because of %s. Terraform destroys "+
					"the old binding as part of that, and %q has no default security group to fall "+
					"back to. %s So its tenant security groups stay on it until it's deleted with "+
					"its cluster or instance group, unless the new binding names the same VPC and "+
					"replaces them.", prior.ValueString(), changed, prior.ValueString(), noDefaultGroupWhy),
			)
		} else {
			unsure := ""
			if oldDefault == defaultGroupUnknown {
				unsure = ifNoDefaultGroup
			}
			resp.Diagnostics.AddWarning(
				"Replacing this resource changes the VPC's filter",
				fmt.Sprintf("This plan replaces the binding on %q, because of %s. Terraform destroys "+
					"the old binding as part of that, which takes the tenant security groups off %q and "+
					"puts its platform-managed DEFAULT group back on. "+defaultGroupFilters+
					" If the new binding names another VPC, %q stays on DEFAULT after the apply. If "+
					"it names the same VPC, it's on DEFAULT between the destroy and the create. With "+
					"`create_before_destroy` it "+
					"isn't, because the new binding is written first and the destroy that runs after "+
					"finds it and leaves it."+unsure, prior.ValueString(), changed,
					prior.ValueString(), prior.ValueString()),
			)
		}
	}

	planSettledState(ctx, vpcStates.settle, req, resp)
}

// Create and Update are the same operation. The platform has one call, it
// replaces the whole binding, and re-sending the same body converges on the
// same state, so there is nothing for a create to do that an update does not.
func (r *vpcSecurityGroupsResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan vpcSecurityGroupsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := withTimeout(ctx, plan.Timeouts.Create, r.meta.Wait.Timeout, &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}

	// The framework always hands a write its private state. Only a test
	// calling this directly passes none, and then no token is kept.
	var private privateSetter
	if resp.Private != nil {
		private = resp.Private
	}
	state, diags := r.apply(ctx, plan, &resp.State, private)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	resp.Diagnostics.Append(recordUID(ctx, resp.Private, state.uid)...)
	// Again here, for a VPC the plan couldn't read because it wasn't known yet.
	if r.scopedDefaultGroup(ctx, state.VPC, state.Region, state.Project) == defaultGroupMissing {
		warnNoDefaultOnCreate(&resp.Diagnostics, state.VPC.ValueString())
	}
}

func (r *vpcSecurityGroupsResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan vpcSecurityGroupsModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	ctx, cancel := withTimeout(ctx, plan.Timeouts.Update, r.meta.Wait.Timeout, &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}

	// The framework always hands a write its private state. Only a test
	// calling this directly passes none, and then no token is kept.
	var private privateSetter
	if resp.Private != nil {
		private = resp.Private
	}
	state, diags := r.apply(ctx, plan, &resp.State, private)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// apply posts the binding and waits for it to render.
//
// dst is the caller's state, and it is written to before the wait rather than
// only after it. The POST answers 202, which means the binding is already
// committed and being rendered onto the owner's network bundles. Returning an
// error from the wait without recording anything left the fabric enforcing a
// list that state did not mention: on a create Terraform had no row to destroy,
// so the VPC was never handed back to its DEFAULT group, and on an update prior
// state kept saying the old list while the fabric enforced the new one.
//
// private is where the write's token goes, so this instance's own destroy can
// tell the write it finds in the ledger is its own. See bindingWriteKey.
func (r *vpcSecurityGroupsResource) apply(ctx context.Context, plan vpcSecurityGroupsModel, dst *tfsdk.State, private privateSetter) (vpcSecurityGroupsModel, diag.Diagnostics) {
	var diags diag.Diagnostics

	target, ok := resolveScope(r.meta, plan.Region, plan.Project, &diags)
	if !ok {
		return vpcSecurityGroupsModel{}, diags
	}

	// Never nil. The field is required and omitting it is a 422 rather than
	// "change nothing", which is deliberate upstream: a dropped field must not
	// be able to silently leave a VPC's security posture as it was.
	groups := []string{}
	if !plan.SecurityGroups.IsNull() && !plan.SecurityGroups.IsUnknown() {
		diags.Append(plan.SecurityGroups.ElementsAs(ctx, &groups, false)...)
		if diags.HasError() {
			return vpcSecurityGroupsModel{}, diags
		}
	}

	vpc := plan.VPC.ValueString()
	// The call replaces the whole binding, so sending it again after a
	// refusal asks for exactly what the first one did.
	busy := newBusyRetry("setVPCSecurityGroups", r.meta.Wait, vpcDependency(r.meta, target, vpc))
	var set *k0rdent.SetVPCSecurityGroupsResponse
	var err error
	// The VPC is held from the write to its record, so a destroy of an older
	// instance on it in this run sees either neither or both. See Delete.
	release := r.meta.vpcBindings.hold(target, vpc)
	waitErr := busy.sendUntilSettled(ctx, func(ctx context.Context) (*http.Response, []byte, error) {
		set, err = r.meta.Client.SetVPCSecurityGroupsWithResponse(ctx, target.region, target.project, vpc,
			k0rdent.VPCSecurityGroupsRequest{SecurityGroups: groups})
		if err != nil {
			return nil, nil, err
		}
		return set.HTTPResponse, set.Body, nil
	})
	token := newBindingToken()
	if waitErr == nil && err == nil && set.JSON202 != nil {
		// Committed, so it's recorded for this run, as the platform reports
		// it, and a destroy of an older instance on the same VPC later in the
		// apply leaves it be.
		r.meta.vpcBindings.wrote(target, vpc, set.JSON202.SecurityGroups, token)
	}
	release()
	if waitErr != nil {
		busy.report(&diags, "attach the security groups to the VPC", set.HTTPResponse, set.Body, waitErr)
		return vpcSecurityGroupsModel{}, diags
	}
	if err != nil || set.JSON202 == nil {
		var respBody []byte
		var httpResp *http.Response
		if set != nil {
			respBody, httpResp = set.Body, set.HTTPResponse
		}
		addAPIError(&diags, "attach the security groups to the VPC", httpResp, respBody, err)
		// `CONFLICT_IMMUTABLE` is the backend refusing, and the upstream message
		// does not say which backend the VPC is on. The other 409 causes are a
		// change in flight or a VPC being deleted, which the message names.
		var apiErr *k0rdent.APIError
		if set != nil && errors.As(k0rdent.CheckResponse(set.HTTPResponse, set.Body), &apiErr) && apiErr.HasReason("CONFLICT_IMMUTABLE") {
			diags.AddWarning(
				"Check the VPC's fabric backend",
				fmt.Sprintf("Only a VPC on the `nico` backend can carry security groups; attaching "+
					"is refused on any other. Read `backend` on `%s` for %q before retrying.",
					ResourceName(vpcDataSourceSuffix), vpc),
			)
		}
		return vpcSecurityGroupsModel{}, diags
	}

	// Committed as of here, so it goes into state as of here, and so does the
	// token that marks the write as this instance's.
	diags.Append(setBindingToken(ctx, private, token)...)
	diags.Append(dst.SetAttribute(ctx, path.Root("vpc"), vpc)...)
	diags.Append(dst.SetAttribute(ctx, path.Root("region"), target.region)...)
	diags.Append(dst.SetAttribute(ctx, path.Root("project"), target.project)...)
	diags.Append(dst.SetAttribute(ctx, path.Root("security_groups"), plan.SecurityGroups)...)
	if diags.HasError() {
		return vpcSecurityGroupsModel{}, diags
	}

	if err := r.waitForSettled(ctx, target, vpc); err != nil {
		addWaitError(&diags, "The VPC did not settle after the security groups were attached", err)
		return vpcSecurityGroupsModel{}, diags
	}

	out, found, d, settleErr := settledRead(ctx, plan.State, vpcStates.settle,
		func(ctx context.Context) (vpcSecurityGroupsModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, vpc, plan)
		},
		func(m vpcSecurityGroupsModel) types.String { return m.State },
		func(ctx context.Context) error { return r.waitForSettled(ctx, target, vpc) })
	diags.Append(d...)
	if settleErr != nil {
		addWaitError(&diags, "The VPC did not settle after the security groups were attached", settleErr)
	}
	if diags.HasError() {
		return vpcSecurityGroupsModel{}, diags
	}
	if !found {
		// Local: the provider's conclusion from two calls.
		diags.AddError(
			"The VPC could not be read back",
			fmt.Sprintf("The security groups were attached to %q and reading it afterwards "+
				"answered 404.", vpc),
		)
		return vpcSecurityGroupsModel{}, diags
	}
	return out, diags
}

func (r *vpcSecurityGroupsResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State, &req.State)
	var prior vpcSecurityGroupsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, prior.Region, prior.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	state, found, diags := r.read(ctx, target, prior.VPC.ValueString(), prior)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		// The VPC is gone, which means its owner was destroyed. The binding
		// went with it and there is nothing to reconcile.
		resp.State.RemoveResource(ctx)
		return
	}
	if !refreshedOwnObject(ctx, req.Private, resp, "VPC", prior.VPC.ValueString(), state.uid) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Delete hands the VPC back to its default group, if the binding is still the
// one this instance holds.
//
// There is no operation that detaches everything, and an empty list is defined
// as "reinstate the default" rather than "attach nothing", so this is the whole
// of what destroy can mean here. Doing nothing instead would leave the tenant's
// groups enforced on a VPC that Terraform no longer tracks, which is the worse
// of the two surprises.
//
// A VPC has one binding, though, and this instance may no longer be the one
// holding it. With create_before_destroy the replacement writes its binding
// first and this destroy runs after, so sending the empty list unasked would
// take off what the same apply had just put on. So the VPC is read first, and
// anything but the binding this instance last read is left alone, the way a
// provider leaves a singleton association that a newer instance has taken
// over. Writes from this run wait out the gap between the read and the write.
// There's no conditional write to close it to anything else, so a change from
// outside landing in that gap is still overwritten.
//
// A VPC with no default group, which API v1.3.0-rc.4 makes for no subnet
// prefixes or more than 16, refuses the empty list. Its groups are left on,
// recorded for the run so a delete of one of them waits for the VPC to go,
// and the resource leaves state with a warning. The VPC's groups are read
// first so the refused write isn't sent, and the refusal is handled too, for
// a read that couldn't tell.
func (r *vpcSecurityGroupsResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state vpcSecurityGroupsModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, state.Region, state.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, state.Timeouts.Delete, r.meta.Wait.Timeout, &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}
	vpc := state.VPC.ValueString()

	// Held from the read to the write. Otherwise another instance in this run
	// could write the same VPC, and record it, between this destroy deciding
	// the binding is its own and clearing it, as a swap of two bindings
	// between two VPCs does under parallelism. The busy wait around the write
	// is inside it, because a write let in during that wait is exactly what
	// the clear would then wipe. That's bounded by this destroy's timeout,
	// and the wait for the render after the write isn't held, so a create
	// queued behind this one waits for a read and a write, not for a render.
	// Nothing that holds one VPC waits on another, so it can't deadlock.
	release := r.meta.vpcBindings.hold(target, vpc)
	defer release()

	got, err := r.meta.Client.GetVPCWithResponse(ctx, target.region, target.project, vpc)
	switch {
	case err != nil:
		addAPIError(&resp.Diagnostics, "read the VPC before returning it to its default security group", nil, nil, err)
		return
	case isNotFound(got.HTTPResponse, got.Body):
		// The VPC went with its owner. Nothing to hand back.
		return
	case got.JSON200 == nil:
		addAPIError(&resp.Diagnostics, "read the VPC before returning it to its default security group",
			got.HTTPResponse, got.Body, nil)
		return
	}
	live := got.JSON200.SecurityGroups
	lastRead, diags := lastReadBinding(ctx, state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	own, diags := bindingToken(ctx, req.Private)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	verdict := destroyVerdict(live, lastRead, r.meta.vpcBindings.newerThan(target, vpc, own))
	if verdict != bindingOurs {
		// A VPC on nothing but its own default is where clearing would have
		// left it, as a new binding of [] does. Nothing was kept from
		// anyone, so there's nothing to warn about.
		if onlyDefault, d := r.allDefaultGroups(ctx, target, live); onlyDefault && !d.HasError() {
			return
		}
	}
	switch verdict {
	case bindingNewer:
		resp.Diagnostics.AddWarning(
			"Left the newer binding on the VPC",
			redactDiagnosticText(fmt.Sprintf("%q holds %s, which a newer instance of this resource wrote earlier in this "+
				"apply. That's how `create_before_destroy` runs a replacement: the new binding is "+
				"written first and the old one is destroyed after. Clearing it would have taken the "+
				"new groups off and put the VPC's DEFAULT group back on, so the old binding was only "+
				"dropped from state.", vpc, groupList(live))),
		)
		return
	case bindingChanged:
		resp.Diagnostics.AddWarning(
			"Left a binding this resource no longer holds",
			redactDiagnosticText(fmt.Sprintf("%q holds %s, and this resource last read %s there. Something else has set it "+
				"since, such as another configuration or the console. Clearing it would have taken "+
				"those groups off and put the VPC's DEFAULT group back on, so the VPC was left as it is "+
				"and this resource was only dropped from state. To put the VPC back on DEFAULT, set "+
				"its groups to [] from whatever manages it now.", vpc, groupList(live), groupList(lastRead))),
		)
		return
	}

	// Read before the write, so a VPC with no default group isn't sent a
	// write it refuses. The refusal is still handled below, for a read that
	// couldn't tell.
	if defaultGroupOf(ctx, r.meta, target, *got.JSON200) == defaultGroupMissing {
		r.leaveBound(target, vpc, live, &resp.Diagnostics)
		return
	}

	if r.beforeClear != nil {
		r.beforeClear()
	}
	// The same call as apply, so a VPC still rendering an earlier change
	// refuses it the same way, and it's waited out the same way.
	busy := newBusyRetry("setVPCSecurityGroups", r.meta.Wait, vpcDependency(r.meta, target, vpc))
	var set *k0rdent.SetVPCSecurityGroupsResponse
	waitErr := busy.sendUntilSettled(ctx, func(ctx context.Context) (*http.Response, []byte, error) {
		set, err = r.meta.Client.SetVPCSecurityGroupsWithResponse(ctx, target.region, target.project, vpc,
			k0rdent.VPCSecurityGroupsRequest{SecurityGroups: []string{}})
		if err != nil {
			return nil, nil, err
		}
		return set.HTTPResponse, set.Body, nil
	})
	switch {
	case set != nil && refusedForNoDefault(set.HTTPResponse, set.Body):
		r.leaveBound(target, vpc, live, &resp.Diagnostics)
		return
	case waitErr != nil:
		busy.report(&resp.Diagnostics, "return the VPC to its default security group", set.HTTPResponse, set.Body, waitErr)
		return
	case err != nil:
		addAPIError(&resp.Diagnostics, "return the VPC to its default security group", nil, nil, err)
		return
	case isNotFound(set.HTTPResponse, set.Body):
		// The VPC went with its owner. Nothing to hand back.
		return
	case set.JSON202 == nil:
		addAPIError(&resp.Diagnostics, "return the VPC to its default security group",
			set.HTTPResponse, set.Body, nil)
		return
	}

	release()
	if err := r.waitForSettled(ctx, target, vpc); err != nil {
		addWaitError(&resp.Diagnostics, "The VPC did not settle after its groups were detached", err)
	}
}

// leaveBound ends a destroy on a VPC with no default group: the platform
// won't take its groups off, so they stay and the resource leaves state.
// Failing instead would leave a destroy nothing could get past but
// `terraform state rm`.
func (r *vpcSecurityGroupsResource) leaveBound(target clusterTarget, vpc string, groups []string, diags *diag.Diagnostics) {
	r.meta.vpcBindings.leftOnVPC(target, vpc, groups)
	warnLeftBound(diags, vpc, groups)
}

// bindingVerdict is what a destroy makes of the binding it finds on the VPC.
type bindingVerdict int

const (
	// bindingOurs is still what this instance last read, so the destroy
	// clears it.
	bindingOurs bindingVerdict = iota
	// bindingNewer is what a newer instance wrote earlier in this run.
	bindingNewer
	// bindingChanged is something else, set since this instance last read.
	bindingChanged
)

// destroyVerdict decides whether a destroy may clear the binding it finds.
//
// live is what the VPC holds now, lastRead what this instance last read there,
// and newer what a Create or Update wrote to the same VPC earlier in this run,
// or nil. The comparison is exact and ordered. The fabric matches rules
// first-match, so the same groups in another order are another filter, and a
// destroy that cleared them would be clearing a binding nobody here wrote.
// The VPC's own default group needs nothing special: when this instance asked
// for [], the platform reported the default and that's what it last read.
//
// newer is checked first because only it can tell a replacement that wrote
// the same groups again, as -replace does, from the binding being unchanged.
// It's never this instance's own write: one provider can create a binding
// and destroy it later in the same run, as `terraform test` does across its
// runs and teardown, and that binding is this instance's to clear.
// With nothing recorded to compare, which only a state written by a failed
// create leaves, the binding is taken as this instance's, as it always was.
func destroyVerdict(live, lastRead, newer []string) bindingVerdict {
	switch {
	case newer != nil && slices.Equal(live, newer):
		return bindingNewer
	case len(lastRead) > 0 && !slices.Equal(live, lastRead):
		return bindingChanged
	default:
		return bindingOurs
	}
}

// lastReadBinding is the binding this instance last saw on the VPC: `attached`,
// which every apply and refresh records, or the configured list when a failed
// create left `attached` unset.
func lastReadBinding(ctx context.Context, state vpcSecurityGroupsModel) ([]string, diag.Diagnostics) {
	from := state.Attached
	if from.IsNull() || from.IsUnknown() {
		from = state.SecurityGroups
	}
	var out []string
	if from.IsNull() || from.IsUnknown() {
		return out, nil
	}
	diags := from.ElementsAs(ctx, &out, false)
	return out, diags
}

func groupList(ids []string) string {
	return "[" + strings.Join(ids, ", ") + "]"
}

// bindingLedger records, per VPC, the binding a Create or Update last wrote in
// this run of the provider.
//
// Terraform runs one provider process for a whole apply, and every instance
// of this resource in it shares one Meta, so this is the one place the
// replacement's write and the old instance's destroy can meet. The platform
// gives a binding no id or generation to compare instead. `updatedAt` also
// moves on the platform's own state transitions, so it can't say who wrote.
type bindingLedger struct {
	mu    sync.Mutex
	byVPC map[string]bindingWrite
	held  map[string]*sync.Mutex
	// left is, per group, the VPCs with no default group that a destroy in
	// this run left the group bound to. See leftOnVPC.
	left map[string][]string
}

// bindingWrite is one recorded write: the groups the platform reported, and
// the token the writing instance keeps in its private state.
type bindingWrite struct {
	groups []string
	token  string
}

// bindingWriteKey is the private state key for the token of the instance's
// last write. Private state follows the instance through refreshes, and a
// replacement's new instance starts without it, which is the whole point.
const bindingWriteKey = "binding_write"

// privateSetter is the part of the framework's private state a write uses.
type privateSetter interface {
	SetKey(ctx context.Context, key string, value []byte) diag.Diagnostics
}

// privateGetter is the part a destroy uses.
type privateGetter interface {
	GetKey(ctx context.Context, key string) ([]byte, diag.Diagnostics)
}

func newBindingToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error.
	return hex.EncodeToString(b)
}

func setBindingToken(ctx context.Context, private privateSetter, token string) diag.Diagnostics {
	if private == nil {
		return nil
	}
	value, err := json.Marshal(token)
	if err != nil {
		var diags diag.Diagnostics
		diags.AddError("Could not record the binding write", err.Error())
		return diags
	}
	return private.SetKey(ctx, bindingWriteKey, value)
}

// bindingToken is the token of this instance's last write, or "" when it has
// none, as a state written before tokens existed doesn't.
func bindingToken(ctx context.Context, private privateGetter) (string, diag.Diagnostics) {
	// GetKey answers nothing for a nil private state, which only a test
	// calling Delete directly passes.
	value, diags := private.GetKey(ctx, bindingWriteKey)
	if diags.HasError() || len(value) == 0 {
		return "", diags
	}
	var token string
	if err := json.Unmarshal(value, &token); err != nil {
		// Not this resource's to read, so not its own write either.
		return "", diags
	}
	return token, diags
}

// hold takes the VPC for one read-then-write or write-then-record, and returns
// what gives it back. Only instances in this run can be kept out this way.
func (l *bindingLedger) hold(target clusterTarget, vpc string) func() {
	l.mu.Lock()
	if l.held == nil {
		l.held = map[string]*sync.Mutex{}
	}
	key := bindingKey(target, vpc)
	m, ok := l.held[key]
	if !ok {
		m = &sync.Mutex{}
		l.held[key] = m
	}
	l.mu.Unlock()
	m.Lock()
	// Safe to call more than once, so a caller can give it back early and
	// still defer it for every other return.
	var once sync.Once
	return func() { once.Do(m.Unlock) }
}

func bindingKey(target clusterTarget, vpc string) string {
	return target.region + "/" + target.project + "/" + vpc
}

func (l *bindingLedger) wrote(target clusterTarget, vpc string, groups []string, token string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.byVPC == nil {
		l.byVPC = map[string]bindingWrite{}
	}
	// Never nil, so a recorded write of nothing still reads as a write.
	l.byVPC[bindingKey(target, vpc)] = bindingWrite{groups: append([]string{}, groups...), token: token}
}

func (l *bindingLedger) lookup(target clusterTarget, vpc string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.byVPC[bindingKey(target, vpc)].groups
}

// newerThan is what another instance wrote to the VPC in this run, or nil
// when nothing was written or the last write was own's, the token of the
// instance asking. An empty own matches nothing, so a state from before
// tokens still finds a replacement's write.
func (l *bindingLedger) newerThan(target clusterTarget, vpc, own string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.byVPC[bindingKey(target, vpc)]
	if !ok || (own != "" && w.token == own) {
		return nil
	}
	return w.groups
}

// ImportState takes the VPC id, because a VPC has exactly one binding and the
// binding has no id of its own.
func (r *vpcSecurityGroupsResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importScoped(ctx, r.meta, importForm{
		resource: ResourceName(vpcSecurityGroupsResourceSuffix),
		key:      "vpc",
	}, req, resp)
}

// IdentitySchema is what an import block's `identity` takes.
func (r *vpcSecurityGroupsResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = scopedIdentitySchema("vpc", "")
}

func (r *vpcSecurityGroupsResource) read(ctx context.Context, target clusterTarget, vpc string, prior vpcSecurityGroupsModel) (vpcSecurityGroupsModel, bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	got, err := r.meta.Client.GetVPCWithResponse(ctx, target.region, target.project, vpc)
	if err != nil {
		addAPIError(&diags, "read the VPC", nil, nil, err)
		return vpcSecurityGroupsModel{}, false, diags
	}
	if isNotFound(got.HTTPResponse, got.Body) {
		return vpcSecurityGroupsModel{}, false, diags
	}
	if got.JSON200 == nil {
		addAPIError(&diags, "read the VPC", got.HTTPResponse, got.Body, nil)
		return vpcSecurityGroupsModel{}, false, diags
	}

	attached, d := types.ListValueFrom(ctx, types.StringType, got.JSON200.SecurityGroups)
	diags.Append(d...)

	out := vpcSecurityGroupsModel{
		VPC:      types.StringValue(vpc),
		Region:   prior.Region,
		Project:  prior.Project,
		Timeouts: prior.Timeouts,
		Attached: attached,
		State:    types.StringNull(),
	}
	if got.JSON200.State != nil {
		out.State = types.StringValue(string(*got.JSON200.State))
	}

	// The binding is compared against what the platform reports, so a change
	// made in the console shows as a plan, on a security boundary. The one
	// exception is an empty configured list: it means "use the platform's
	// default", and the platform reports that default here. Only a VPC's own
	// default group (`ownerKind: vpc`) satisfies it. Any other group got there
	// outside Terraform, and is planned away.
	switch {
	case prior.SecurityGroups.IsNull() || prior.SecurityGroups.IsUnknown():
		// An import has nothing configured, so the platform's list is the
		// starting point, except that a binding holding only the VPC's own
		// default group is what `[]` means, and the group's owner says so.
		// Recording `[]` lets the configuration the contract documents for
		// that plan clean. One that names the default group outright plans
		// an update that sends the same group again.
		out.SecurityGroups = attached
		if len(got.JSON200.SecurityGroups) > 0 {
			defaults, d := r.allDefaultGroups(ctx, target, got.JSON200.SecurityGroups)
			diags.Append(d...)
			if defaults {
				out.SecurityGroups = types.ListValueMust(types.StringType, []attr.Value{})
			}
		}
	case len(prior.SecurityGroups.Elements()) == 0:
		out.SecurityGroups = prior.SecurityGroups
		defaults, d := r.allDefaultGroups(ctx, target, got.JSON200.SecurityGroups)
		diags.Append(d...)
		if !defaults {
			out.SecurityGroups = attached
		}
	default:
		out.SecurityGroups = attached
	}

	out.uid = uidOf(got.JSON200.Uid)
	return out, true, diags
}

// allDefaultGroups reports whether every group is a VPC's own default.
func (r *vpcSecurityGroupsResource) allDefaultGroups(ctx context.Context, target clusterTarget, ids []string) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	for _, id := range ids {
		got, err := r.meta.Client.GetSecurityGroupWithResponse(ctx, target.region, target.project, id)
		if err != nil {
			addAPIError(&diags, "read a security group attached to the VPC", nil, nil, err)
			return false, diags
		}
		if isNotFound(got.HTTPResponse, got.Body) {
			return false, diags
		}
		if got.JSON200 == nil {
			addAPIError(&diags, "read a security group attached to the VPC", got.HTTPResponse, got.Body, nil)
			return false, diags
		}
		if got.JSON200.OwnerKind == nil || *got.JSON200.OwnerKind != k0rdent.SecurityGroupOwnerKindVpc {
			return false, diags
		}
	}
	return true, diags
}

func (r *vpcSecurityGroupsResource) waitForSettled(ctx context.Context, target clusterTarget, vpc string) error {
	return vpcStates.awaitSettled(ctx, r.meta.Wait, "the VPC's security group binding to be rendered",
		vpcReader(r.meta, target, vpc))
}

// vpcReader reads a VPC for a wait.
func vpcReader(meta *Meta, target clusterTarget, vpc string) stateRead[k0rdent.VPC] {
	return func(ctx context.Context) (*k0rdent.VPC, *http.Response, []byte, error) {
		got, err := meta.Client.GetVPCWithResponse(ctx, target.region, target.project, vpc)
		if err != nil {
			return nil, nil, nil, err
		}
		return got.JSON200, got.HTTPResponse, got.Body, nil
	}
}

// vpcDependency is a VPC as a write that's refused while it changes depends
// on it. A VPC is `creating` while it is first materialized and `updating`
// while a binding renders, and the contract refuses a peering create and a
// second binding during either.
func vpcDependency(meta *Meta, target clusterTarget, vpc string) dependency {
	return dependsOn(fmt.Sprintf("the VPC %q", vpc), vpcReader(meta, target, vpc), vpcStates.state,
		string(k0rdent.VPCStateCreating), string(k0rdent.VPCStateUpdating))
}

// vpcStates is what a wait on a VPC makes of each state it reports. Nothing
// here deletes a VPC, so there is no delete table.
var vpcStates = stateKind[k0rdent.VPC]{
	name: "VPC",
	state: func(v k0rdent.VPC) (string, bool) {
		return stateString(v.State), stateString(v.State) != ""
	},
	detail: func(v k0rdent.VPC) string { return stateDetail(v.StateMessage, "") },
	settle: stateTable{
		missing: stateRule{verdict: settled},
		states: map[string]stateRule{
			string(k0rdent.VPCStateActive):   {verdict: settled},
			string(k0rdent.VPCStateCreating): {verdict: keepWaiting},
			string(k0rdent.VPCStateUpdating): {verdict: keepWaiting},
			string(k0rdent.VPCStateDeleting): {verdict: keepWaiting},
			string(k0rdent.VPCStateFailed):   {verdict: failsTerminally, detail: true},
		},
	},
}
