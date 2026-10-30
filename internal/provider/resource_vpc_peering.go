package provider

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ resource.Resource                 = (*vpcPeeringResource)(nil)
	_ resource.ResourceWithConfigure    = (*vpcPeeringResource)(nil)
	_ resource.ResourceWithImportState  = (*vpcPeeringResource)(nil)
	_ resource.ResourceWithIdentity     = (*vpcPeeringResource)(nil)
	_ resource.ResourceWithModifyPlan   = (*vpcPeeringResource)(nil)
	_ resource.ResourceWithUpgradeState = (*vpcPeeringResource)(nil)
)

const vpcPeeringResourceSuffix = "vpc_peering"

// vpcPeeringResourceWith is the peering as the provider registers it, sharing
// the provider's default_tags with its tags plan modifier.
func vpcPeeringResourceWith(defaults *providerDefaults) func() resource.Resource {
	return func() resource.Resource { return &vpcPeeringResource{defaults: defaults} }
}

type vpcPeeringResource struct {
	meta *Meta

	// defaults is the provider's default_tags for the tags plan modifier,
	// which can't reach meta. Nil outside a provider, which reads as not
	// known.
	defaults *providerDefaults
}

type vpcPeeringModel struct {
	ID          types.String   `tfsdk:"id"`
	Region      types.String   `tfsdk:"region"`
	Project     types.String   `tfsdk:"project"`
	VPC         types.String   `tfsdk:"vpc"`
	RemoteVPC   types.String   `tfsdk:"remote_vpc"`
	DisplayName types.String   `tfsdk:"display_name"`
	Description types.String   `tfsdk:"description"`
	Tags        types.Map      `tfsdk:"tags"`
	TagsAll     types.Map      `tfsdk:"tags_all"`
	LocalVPC    types.String   `tfsdk:"local_vpc"`
	State       types.String   `tfsdk:"state"`
	SelfLink    types.String   `tfsdk:"self_link"`
	Timeouts    timeouts.Value `tfsdk:"timeouts"`

	// uid is the platform's uid for the VPC peering read, which own_object.go
	// keeps in private state. Not an attribute.
	uid string
}

func (m vpcPeeringModel) metadata() tenantMetadata {
	return tenantMetadata{
		DisplayName: m.DisplayName,
		Description: m.Description,
		Tags:        m.Tags,
		TagsAll:     m.TagsAll,
	}
}

func (r *vpcPeeringResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = ResourceName(vpcPeeringResourceSuffix)
}

// UpgradeState migrates a state written at an earlier schema version. See
// state_history.go.
func (r *vpcPeeringResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	return stateHistories[vpcPeeringResourceSuffix].upgraders()
}

// Schema declares the resource.
//
// # This resource is one side of a peering, and saying so is the whole job
//
// A peering is symmetric in effect and asymmetric in declaration: each project
// creates its own object pointing at the other, and traffic flows only once
// both exist. `state: active` means this side's declaration is applied. It does
// not mean the counterpart exists, and it does not mean anything is reachable.
//
// That gap is the hazard. A Terraform user reads a green apply as "the thing I
// asked for is now true", and here it is not: a half-open peering is a normal,
// indefinite, non-error state that this side's own object can't distinguish
// from a finished one. The counterpart is listed under the remote VPC, which
// may sit in another project, outside what the provider reads. So the schema
// says it in the resource description and in `state`, and Create reads the far
// side when it's in this peering's scope and warns whenever there's no active
// mirror to show.
//
// # Nothing here can be updated
//
// The API offers create, read and delete. There is no updateVPCPeering, so
// every attribute replaces. Unlike a subnet's, a replacement here isn't
// guarded, because it's cheap: it tears down one side's declaration and puts
// it back, and the counterpart is untouched throughout.
func (r *vpcPeeringResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replaceString := []planmodifier.String{stringplanmodifier.RequiresReplace()}

	resp.Schema = schema.Schema{
		Version: stateHistories[vpcPeeringResourceSuffix].version(),
		MarkdownDescription: "One side of a peering between two VPCs in the same region.\n\n" +
			"**Creating this does not connect anything on its own.** The project that owns " +
			"`remote_vpc` has to declare its own mirrored peering pointing back, and until it " +
			"does the peering is half-open. That is a normal state, not an error, and this side's " +
			"`state` can't show it. The mirror is listed under `remote_vpc`, so after a create the " +
			"provider lists that VPC's peerings: when it finds an `active` one pointing back it says " +
			"nothing, and otherwise it warns, saying what it found. It reads only a `remote_vpc` in " +
			"this peering's own region and project. For one anywhere else, or one whose read is " +
			"refused, the warning says so and the far side has to be confirmed out of band.\n\n" +
			"**A peering outlives the VPC it points at.** If the remote VPC's owning cluster is " +
			"deleted, this object survives, still `active`, naming a VPC that is gone, carrying " +
			"no traffic. The platform will not remove it on the strength of another project's " +
			"delete. Destroying it, or destroying the owner of the local VPC, is what clears it.\n\n" +
			"Requires `region` and `project` on the provider.",
		Blocks: map[string]schema.Block{
			// No Update: the API has no operation for one.
			"timeouts": timeouts.Block(ctx, timeouts.Opts{Create: true, Delete: true}),
		},
		Attributes: map[string]schema.Attribute{
			"region":  regionAttribute(),
			"project": projectAttribute(),
			// Optional+Computed: the provider generates a name when one is
			// omitted, and the name is immutable.
			"id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Peering name, unique within the project. Omit it and the " +
					"provider generates one, `tf-` and 16 hex digits, so that a create whose answer " +
					"is lost can be read back rather than sent twice.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
				Validators: idValidators(),
			},
			"vpc": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Id of the local VPC this peering is declared under. Find it " +
					"with the `" + ResourceName(vpcsDataSourceSuffix) + "` data source: a VPC is " +
					"materialized by a cluster or instance group, never created directly.",
				PlanModifiers: replaceString,
				Validators:    []validator.String{urlSafeIdentifier{}},
			},
			"remote_vpc": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The counterpart VPC: its id, for one in this peering's own " +
					"region and project, or its `self_link`, for one anywhere else.\n\n" +
					"Name a VPC in the same project by id, through the `vpc` attribute of the " +
					"cluster or instance group that brought it. That stays known while the owner " +
					"changes, so renaming it replaces no peering. A `self_link` read from the `" +
					ResourceName(vpcDataSourceSuffix) + "` data source does not: Terraform reads the " +
					"data source during the apply when the owner has a change pending, and a value " +
					"it doesn't know at plan replaces the peering. The provider sends an id as the " +
					"URI the API takes, built from the contract's path for a VPC. Moving between an " +
					"id and the `self_link` of the same VPC replaces nothing.\n\n" +
					"It must be an `active` VPC in the same region, and not the local one. It may " +
					"be in a project you have no permission on, because declaring this side is " +
					"not by itself enough to establish the peering.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplaceIf(remoteVPCRequiresReplace,
					"Replaces the peering unless the new value names the same VPC.",
					"Replaces the peering unless the new value names the same VPC.")},
				Validators: []validator.String{remoteVPCReference{}},
			},
			"display_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Human-friendly name. The API has no update for a peering, so even this can't change in place.",
				PlanModifiers:       replaceString,
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form text describing it, for people to read.",
				PlanModifiers:       replaceString,
			},
			"tags": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "User-defined labels, settable only at creation.",
				PlanModifiers:       []planmodifier.Map{replaceTagsIfTheyChange(r.defaults)},
				Validators:          []validator.Map{reservedTagKey{}},
			},
			tagsAllAttribute: tagsAllSchemaAttribute(" The API sets a peering's tags only at creation, " +
				"so a change to the provider's `" + defaultTagsAttribute + "` reaches a new peering and leaves " +
				"one that exists as it is, with a warning in the plan."),

			"local_vpc": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "URI of the local VPC, as the platform resolved it from `vpc`. " +
					"Always in this resource's own project.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"state": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Lifecycle state **of this side only**. `active` means this " +
					"declaration is applied on the fabric. It says nothing about whether the " +
					"counterpart has been declared or whether traffic flows.",
			},
			"self_link": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Platform-defined reference to this peering.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
	describeReplacement(&resp.Schema, "peering")
}

func (r *vpcPeeringResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (r *vpcPeeringResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	modifyScopePlan(ctx, r.meta, req, resp)
	planTagsAllAtCreation(ctx, r.meta, "VPC peering", req, resp)
	if resp.Diagnostics.HasError() {
		return
	}
	planResume(ctx, req, resp)
	planSettledState(ctx, vpcPeeringStates.settle, req, resp)
}

func (r *vpcPeeringResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan vpcPeeringModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, plan.Region, plan.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, plan.Timeouts.Create, vpcPeeringStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}

	md, diags := plan.metadata().create(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	key, err := newCreateKey(plan.ID, true)
	if err != nil {
		addCreateKeyError(&resp.Diagnostics, "VPC peering", err)
		return
	}

	vpc := plan.VPC.ValueString()
	remote := remoteVPCURI(target, plan.RemoteVPC.ValueString())
	// Only the fields the create schema lists. `additionalProperties: false`
	// means a server-owned field is rejected rather than ignored, so a peering
	// read from a GET cannot be posted back, and nothing here is built from one.
	body := k0rdent.VPCPeeringCreateRequest{
		Id:          &key.id,
		RemoteVpc:   remote,
		DisplayName: md.DisplayName,
		Description: md.Description,
		Tags:        key.tagged(md.Tags),
	}
	// A 504 means the peering was committed and its workflow was not
	// dispatched: it exists in `creating` and settles on its own. The create
	// always names it, so it is read back, and once its token matches it is
	// recorded and waited out like a 201. A 409 for the same direction under
	// another id finds nothing under this one, and is reported as it came.
	create := convergingCreate{
		key: key, kind: "VPC peering", what: "create the VPC peering",
		resourceName: ResourceName(vpcPeeringResourceSuffix),
		importID:     nestedScopedImportID(target, vpc),
		send: func(ctx context.Context) createAnswer {
			created, err := r.meta.Client.CreateVPCPeeringWithResponse(ctx, target.region, target.project, vpc, body)
			if err != nil {
				return createAnswer{err: err}
			}
			a := createAnswer{resp: created.HTTPResponse, body: created.Body}
			if created.JSON201 != nil {
				a.id, a.uid = deref(created.JSON201.Id), uidOf(created.JSON201.Uid)
			}
			return a
		},
		lookup: func(ctx context.Context, id string) createLookup {
			got, err := r.meta.Client.GetVPCPeeringWithResponse(ctx, target.region, target.project, vpc, id)
			if err != nil {
				return createLookup{err: err}
			}
			return lookupFrom(got.HTTPResponse, got.Body, got.JSON200 != nil, func() (string, string) {
				return createTokenOf(got.JSON200.Tags), uidOf(got.JSON200.Uid)
			})
		},
		busy: newBusyRetry("createVPCPeering", r.meta.Wait, r.dependencies(target, vpc, remote)...),
	}
	created, ok := createFinish[vpcPeeringModel]{
		operation: "createVPCPeering", target: target,
		parentAttr: "vpc", parentID: vpc,
		waitSummary: "This side of the VPC peering did not become active",
		wait: func(ctx context.Context, id string) error {
			return r.waitForSettled(ctx, target, vpc, id)
		},
		read: func(ctx context.Context, id string) (vpcPeeringModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, vpc, id, plan)
		},
	}.run(ctx, create, req.Plan, &resp.State, resp.Private, &resp.Diagnostics)
	if ok {
		resp.Diagnostics.Append(recordUID(ctx, resp.Private, created.uid)...)
	}
	if !ok {
		return
	}

	resp.Diagnostics.Append(r.counterpart(ctx, target, vpc, created.ID.ValueString(), remote)...)
}

// counterpart says, after every create, what the far side of the peering
// looks like. A green apply here means one side is declared, and the next
// thing the user does is assume the link is up, so it's said on the create
// and not once in the docs.
//
// The far side's peerings are listed under the remote VPC. When both VPCs are in
// one project, the remote VPC lists the mirror. So when the remote VPC is in the
// region and project this peering is in, it's read, and reported as a fact
// when the answer comes, or with what the platform answered when the read
// fails. A remote VPC anywhere else isn't read at all. The provider addresses
// only the scope it's configured for, and no cross-project peering has
// been run live, so the warning there keeps the condition and says the far
// side wasn't checked. A failed read is never an error: this side was
// created, and the far side isn't this resource's to manage.
//
// Every warning here quotes platform text, a refusal or a mirror's id and
// state, so each goes through redactDiagnosticText like any other
// diagnostic.
func (r *vpcPeeringResource) counterpart(ctx context.Context, target clusterTarget, vpc, id, remote string) diag.Diagnostics {
	var diags diag.Diagnostics
	const halfOpen = "This peering carries traffic only once the other side is declared"
	warn := func(detail string) { diags.AddWarning(halfOpen, redactDiagnosticText(detail)) }
	unchecked := func(why string) {
		warn(fmt.Sprintf("%q is now declared on %q. Traffic flows only when the "+
			"project owning %q creates its own mirrored peering pointing back at this VPC. %s, so "+
			"confirm it out of band: a half-open peering stays `active` and reports no error.",
			id, vpc, remote, why))
	}

	p, ok := vpcRoute.params(remote)
	if !ok {
		unchecked(fmt.Sprintf("%q isn't a VPC's self_link, so its peerings couldn't be listed", remote))
		return diags
	}
	if p["region"] != target.region || p["project"] != target.project {
		unchecked(fmt.Sprintf("That VPC is in project %q of region %q, outside the region and project "+
			"this peering is in, which is all the provider reads, so its peerings weren't listed", p["project"], p["region"]))
		return diags
	}
	listed, err := readVPCPeerings(ctx, r.meta, target, p["id"])
	if err != nil {
		unchecked(fmt.Sprintf("Listing that VPC's peerings to check failed (%s)",
			strings.TrimSuffix(strings.TrimSpace(err.Error()), ".")))
		return diags
	}

	// More than one can point back, such as a mirror being deleted beside its
	// replacement. Any active one carries the traffic, so it wins over the
	// list's order.
	var pending *k0rdent.VPCPeering
	for i, peer := range listed {
		back, ok := vpcRoute.params(deref(peer.RemoteVpc))
		if !ok || back["region"] != target.region || back["project"] != target.project || back["id"] != vpc {
			continue
		}
		if stateString(peer.State) == string(k0rdent.VPCPeeringStateActive) {
			return diags
		}
		if pending == nil {
			pending = &listed[i]
		}
	}
	if pending != nil {
		warn(fmt.Sprintf("%q is now declared on %q, and %q has its mirror, %q, which reads `%s`. "+
			"Traffic flows once both sides are `active`.",
			id, vpc, remote, deref(pending.Id), stateString(pending.State)))
		return diags
	}
	warn(fmt.Sprintf("%q is now declared on %q, and %q lists no peering pointing "+
		"back at it, so no traffic flows yet. Declare the mirrored peering on that VPC, with `remote_vpc` "+
		"naming this one: a half-open peering stays `active` and reports no error.", id, vpc, remote))
	return diags
}

func (r *vpcPeeringResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State, &req.State)
	var prior vpcPeeringModel
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, prior.Region, prior.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	state, found, diags := r.read(ctx, target, prior.VPC.ValueString(), prior.ID.ValueString(), prior)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	if !refreshedOwnObject(ctx, req.Private, resp, "VPC peering", prior.ID.ValueString(), state.uid) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update makes no request: the API has none. A new `timeouts` block reaches
// here, and only needs writing to state, and so does a create an earlier
// apply stopped waiting for, which this waits for.
func (r *vpcPeeringResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan, state vpcPeeringModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, state.Region, state.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	id, vpc := state.ID.ValueString(), state.VPC.ValueString()
	if (resumable[vpcPeeringModel]{
		kind: "VPC peering", resourceName: ResourceName(vpcPeeringResourceSuffix),
		timeout: plan.Timeouts.Create, fallback: vpcPeeringStates.timeout(r.meta.Wait),
		wait: func(ctx context.Context) error { return r.waitForSettled(ctx, target, vpc, id) },
		read: func(ctx context.Context) (vpcPeeringModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, vpc, id, plan)
		},
	}).resume(ctx, req, resp) {
		return
	}
	next, found, diags, err := settledRead(ctx, plan.State, vpcPeeringStates.settle,
		func(ctx context.Context) (vpcPeeringModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, vpc, id, plan)
		},
		func(m vpcPeeringModel) types.String { return m.State },
		func(ctx context.Context) error { return r.waitForSettled(ctx, target, vpc, id) })
	resp.Diagnostics.Append(diags...)
	if err != nil {
		addWaitError(&resp.Diagnostics, "The VPC peering did not settle", err)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		// Local: the provider's conclusion from two calls, as in createFinish.
		resp.Diagnostics.AddError(
			"The VPC peering disappeared during the update",
			fmt.Sprintf("Reading %q after the change answered 404.", id),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

func (r *vpcPeeringResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state vpcPeeringModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, state.Region, state.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, state.Timeouts.Delete, vpcPeeringStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}
	vpc, id := state.VPC.ValueString(), state.ID.ValueString()

	send := func(ctx context.Context) (*http.Response, []byte, error) {
		deleted, err := r.meta.Client.DeleteVPCPeeringWithResponse(ctx, target.region, target.project, vpc, id)
		if err != nil {
			return nil, nil, err
		}
		return deleted.HTTPResponse, deleted.Body, nil
	}
	vpcPeeringStates.destroy(ctx, r.meta.Wait, send, r.reader(target, vpc, id), &resp.Diagnostics)
}

// ImportState takes the VPC too: a peering's id is unique within the project
// but every operation on one is addressed under its local VPC.
func (r *vpcPeeringResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importScoped(ctx, r.meta, importForm{
		resource: ResourceName(vpcPeeringResourceSuffix),
		key:      "id",
		parent:   "vpc",
		example:  "vpc-training/to-inference",
	}, req, resp)
}

// IdentitySchema is what an import block's `identity` takes.
func (r *vpcPeeringResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = scopedIdentitySchema("id", "vpc")
}

func (r *vpcPeeringResource) read(ctx context.Context, target clusterTarget, vpc, id string, prior vpcPeeringModel) (vpcPeeringModel, bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	got, err := r.meta.Client.GetVPCPeeringWithResponse(ctx, target.region, target.project, vpc, id)
	if err != nil {
		addAPIError(&diags, "read the VPC peering", nil, nil, err)
		return vpcPeeringModel{}, false, diags
	}
	if isNotFound(got.HTTPResponse, got.Body) {
		return vpcPeeringModel{}, false, diags
	}
	if got.JSON200 == nil {
		addAPIError(&diags, "read the VPC peering", got.HTTPResponse, got.Body, nil)
		return vpcPeeringModel{}, false, diags
	}

	out, d := vpcPeeringStateFrom(ctx, *got.JSON200, vpc, prior, r.meta.defaultTags())
	diags.Append(d...)
	// The platform answers with a URI. State keeps the configuration's
	// spelling while it names the same VPC, so an id written there plans no
	// change after the create.
	if !prior.RemoteVPC.IsNull() && !prior.RemoteVPC.IsUnknown() && !out.RemoteVPC.IsNull() &&
		sameRemoteVPC(target, prior.RemoteVPC.ValueString(), out.RemoteVPC.ValueString()) {
		out.RemoteVPC = prior.RemoteVPC
	}
	out.uid = uidOf(got.JSON200.Uid)
	return out, true, diags
}

// dependencies are the VPCs whose changes the platform refuses a peering
// create against: the local VPC, and the remote one when it's in this same
// project. A remote VPC in another project is never refused with a 409, so
// it isn't read, and this token may not be able to read it anyway.
func (r *vpcPeeringResource) dependencies(target clusterTarget, vpc, remote string) []dependency {
	deps := []dependency{vpcDependency(r.meta, target, vpc)}
	if p, ok := vpcRoute.params(remote); ok && p["region"] == target.region && p["project"] == target.project && p["id"] != vpc {
		deps = append(deps, vpcDependency(r.meta, target, p["id"]))
	}
	return deps
}

// vpcRoute is getVPC's route, which a VPC's self_link is: region, project
// and id.
var vpcRoute = routeOf("getVPC")

// remoteVPCURI is the URI the API takes for what `remote_vpc` names. A bare
// id is a VPC in the peering's own region and project, the same as `vpc`, and
// its URI is getVPC's path filled in from the contract's route, which is the
// shape the create request documents. Anything with a slash is a URI already
// and goes as written.
func remoteVPCURI(target clusterTarget, remote string) string {
	if strings.Contains(remote, "/") {
		return remote
	}
	return vpcRoute.path(map[string]string{"region": target.region, "project": target.project, "id": remote})
}

// sameRemoteVPC reports whether two spellings of `remote_vpc` name one VPC,
// such as an id and that VPC's self_link.
func sameRemoteVPC(target clusterTarget, a, b string) bool {
	if a == b {
		return true
	}
	pa, okA := vpcRoute.params(remoteVPCURI(target, a))
	pb, okB := vpcRoute.params(remoteVPCURI(target, b))
	return okA && okB && maps.Equal(pa, pb)
}

// remoteVPCRequiresReplace replaces the peering when `remote_vpc` names
// another VPC, and not when it only spells the same one another way. The
// API has no update, and none is needed: the request it sent named the same
// VPC. A value not known yet replaces, since it may name another. The scope
// is prior state's, since a change to either replaces on its own.
func remoteVPCRequiresReplace(ctx context.Context, req planmodifier.StringRequest, resp *stringplanmodifier.RequiresReplaceIfFuncResponse) {
	if req.PlanValue.IsUnknown() || req.PlanValue.IsNull() || req.StateValue.IsNull() {
		resp.RequiresReplace = true
		return
	}
	var region, project types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("region"), &region)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("project"), &project)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target := clusterTarget{region: region.ValueString(), project: project.ValueString()}
	resp.RequiresReplace = !sameRemoteVPC(target, req.StateValue.ValueString(), req.PlanValue.ValueString())
}

// remoteVPCReference checks a `remote_vpc` that is an id, since that becomes
// a path segment. A URI is the platform's to check.
type remoteVPCReference struct{}

func (remoteVPCReference) Description(context.Context) string {
	return "an id must be lower-case letters, digits and hyphens, starting with a letter and not " +
		"ending with a hyphen. A self_link is sent as written"
}

func (v remoteVPCReference) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (remoteVPCReference) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() || strings.Contains(req.ConfigValue.ValueString(), "/") {
		return
	}
	urlSafeIdentifier{}.ValidateString(ctx, req, resp)
}

func (r *vpcPeeringResource) waitForSettled(ctx context.Context, target clusterTarget, vpc, id string) error {
	return vpcPeeringStates.awaitSettled(ctx, r.meta.Wait, "this side of the peering to be applied", r.reader(target, vpc, id))
}

// reader reads this side of the peering for a wait.
func (r *vpcPeeringResource) reader(target clusterTarget, vpc, id string) stateRead[k0rdent.VPCPeering] {
	return func(ctx context.Context) (*k0rdent.VPCPeering, *http.Response, []byte, error) {
		got, err := r.meta.Client.GetVPCPeeringWithResponse(ctx, target.region, target.project, vpc, id)
		if err != nil {
			return nil, nil, nil, err
		}
		return got.JSON200, got.HTTPResponse, got.Body, nil
	}
}

// vpcPeeringStates is what a peering's waits make of each state this side of
// it reports.
//
// A peering declares no `updating`, so that value arriving here is a newer API
// and stops the wait rather than being assumed transient. There is no state for
// "the other side is missing", which is exactly why Create warns instead of
// waiting for one.
var vpcPeeringStates = stateKind[k0rdent.VPCPeering]{
	name:     "VPC peering",
	resource: vpcPeeringResourceSuffix,
	subject:  "this side of the peering",
	// "an operation is already in flight (`creating` or `deleting`)."
	deleting: string(k0rdent.VPCPeeringStateDeleting),
	state: func(p k0rdent.VPCPeering) (string, bool) {
		return stateString(p.State), stateString(p.State) != ""
	},
	detail: func(p k0rdent.VPCPeering) string { return stateDetail(p.StateMessage, "") },
	settle: stateTable{
		missing: stateRule{verdict: settled},
		states: map[string]stateRule{
			string(k0rdent.VPCPeeringStateActive):   {verdict: settled},
			string(k0rdent.VPCPeeringStateCreating): {verdict: keepWaiting},
			string(k0rdent.VPCPeeringStateDeleting): {verdict: keepWaiting},
			string(k0rdent.VPCPeeringStateFailed):   {verdict: failsTerminally, detail: true},
		},
	},
	delete: stateTable{
		missing: stateRule{verdict: keepWaiting},
		states: map[string]stateRule{
			string(k0rdent.VPCPeeringStateDeleting): {verdict: keepWaiting},
			string(k0rdent.VPCPeeringStateFailed):   {verdict: failsTerminally, detail: true},
			string(k0rdent.VPCPeeringStateActive):   {verdict: failsTerminally, says: afterDelete},
			string(k0rdent.VPCPeeringStateCreating): {verdict: failsTerminally, says: afterDelete},
		},
	},
}

func vpcPeeringStateFrom(ctx context.Context, p k0rdent.VPCPeering, vpc string, prior vpcPeeringModel, defaults defaultTags) (vpcPeeringModel, diag.Diagnostics) {
	// Only the tenant's own tags. `systemTags` is a separate field and is not
	// folded in, or every plan would propose removing what the platform wrote.
	// The create token is the provider's, so it is taken out too.
	tags, diags := tagAnswerOf(p.Tags).forResource(prior.Tags)
	md := prior.metadata().read(metadataAnswer{DisplayName: p.DisplayName, Description: p.Description, Tags: tags, Defaults: defaults})

	out := vpcPeeringModel{
		ID:          types.StringValue(deref(p.Id)),
		Region:      prior.Region,
		Project:     prior.Project,
		Timeouts:    prior.Timeouts,
		VPC:         types.StringValue(vpc),
		RemoteVPC:   stringOrNull(p.RemoteVpc),
		DisplayName: md.DisplayName,
		Description: md.Description,
		Tags:        md.Tags,
		TagsAll:     md.TagsAll,
		LocalVPC:    stringOrNull(p.LocalVpc),
		State:       types.StringNull(),
		SelfLink:    stringOrNull(p.SelfLink),
	}
	if p.State != nil {
		out.State = types.StringValue(string(*p.State))
	}

	return out, diags
}
