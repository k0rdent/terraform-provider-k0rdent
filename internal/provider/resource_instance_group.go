package provider

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/setvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ resource.Resource                 = (*instanceGroupResource)(nil)
	_ resource.ResourceWithConfigure    = (*instanceGroupResource)(nil)
	_ resource.ResourceWithImportState  = (*instanceGroupResource)(nil)
	_ resource.ResourceWithIdentity     = (*instanceGroupResource)(nil)
	_ resource.ResourceWithModifyPlan   = (*instanceGroupResource)(nil)
	_ resource.ResourceWithUpgradeState = (*instanceGroupResource)(nil)
	_ resource.ResourceWithMoveState    = (*instanceGroupResource)(nil)
)

const instanceGroupResourceSuffix = "instance_group"

func newInstanceGroupResource() resource.Resource {
	return guardReplacements(&instanceGroupResource{}, instanceGroupTenantState)
}

type instanceGroupResource struct {
	meta *Meta
}

type instanceGroupModel struct {
	ID                  types.String `tfsdk:"id"`
	Region              types.String `tfsdk:"region"`
	Project             types.String `tfsdk:"project"`
	ClusterType         types.String `tfsdk:"cluster_type"`
	ClusterTypeResolved types.String `tfsdk:"cluster_type_resolved"`
	DisplayName         types.String `tfsdk:"display_name"`
	Description         types.String `tfsdk:"description"`
	NodePool            types.Map    `tfsdk:"node_pool"`
	SSHAccess           types.Set    `tfsdk:"ssh_access"`
	// SSHAccessHeld is a read's note that it kept ssh_access the platform
	// answered blank. Not stored in state: see teardown_read.go.
	SSHAccessHeld bool           `tfsdk:"-"`
	State         types.String   `tfsdk:"state"`
	SelfLink      types.String   `tfsdk:"self_link"`
	VPC           types.String   `tfsdk:"vpc"`
	Timeouts      timeouts.Value `tfsdk:"timeouts"`

	// uid is the platform's uid for the instance group read, which own_object.go
	// keeps in private state. Not an attribute.
	uid string
}

func (m instanceGroupModel) metadata() tenantMetadata {
	return tenantMetadata{
		DisplayName: m.DisplayName,
		Description: m.Description,
	}
}

type instanceGroupPoolModel struct {
	Size types.Int64 `tfsdk:"size"`
}

func instanceGroupPoolObjectType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"size": types.Int64Type,
	}}
}

func (r *instanceGroupResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = ResourceName(instanceGroupResourceSuffix)
}

// UpgradeState migrates a state written at an earlier schema version. See
// state_history.go.
func (r *instanceGroupResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	return stateHistories[instanceGroupResourceSuffix].upgraders()
}

// MoveState accepts a `moved` block from the type this resource was
// registered under before the provider name settled.
func (r *instanceGroupResource) MoveState(context.Context) []resource.StateMover {
	return stateHistories[instanceGroupResourceSuffix].movers()
}

// Schema declares the resource.
//
// The same omissions as `kubernetes_cluster` apply, for the same reasons, with
// two asymmetries. An instance group cannot carry tags where a cluster can, and
// tags have nowhere to go in the API. And an instance group echoes `sshAccess`
// where a cluster does not, so here `ssh_access` is in the schema and
// drift-checked. The platform returns the users sorted by username, with each
// user's keys in the order sent. So the users are a set and each user's keys a list. The contract
// makes the field immutable after create, so a change to it is a replacement,
// and the replacement guard refuses it like any other.
//
// `node_pool` is where this resource differs most from its twin. The contract
// calls `InstanceGroup.nodePools` immutable in the same document that defines
// `scaleInstanceGroup`, which mutates exactly that field. Taking the wording
// literally would mean marking it `RequiresReplace` and destroying a running
// group to change a node count. The operations support scale up only, so this
// resource scales up in place and replaces for anything else.
func (r *instanceGroupResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Version: stateHistories[instanceGroupResourceSuffix].version(),
		MarkdownDescription: "A group of bare hosts provisioned from a ClusterType's node pools, without a " +
			"Kubernetes control plane.\n\nRequires `region` and `project`, on the provider " +
			"block or on this resource.",
		Blocks: map[string]schema.Block{
			// Create, update and delete each wait. Read does not. Create
			// and delete provision and free hosts, and default longer.
			"timeouts": hardwareTimeouts(ctx),
		},
		Attributes: map[string]schema.Attribute{
			"region":  regionAttribute(),
			"project": projectAttribute(),
			"id":      hardwareIDAttribute("Instance group"),
			"cluster_type": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Reference to the ClusterType version whose node pools describe the hardware " +
					"to provision. The `/versions/{id}` segment is optional. " +
					"See `cluster_type_resolved`, and the note on the `kubernetes_cluster` " +
					"resource's equivalent attribute." +
					knownGap("A tenant can't read the hardware or the image behind a ClusterType's machine types, "+
						"so choosing a ClusterType for its hardware means writing one an operator named."),
				PlanModifiers: []planmodifier.String{clusterTypeRefReplacement()},
			},
			"cluster_type_resolved": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The `clusterType` value the platform returned, verbatim. " +
					"It can be a relative path or an absolute URI, so it can differ in form from " +
					"`cluster_type` while naming the same version.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"display_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Human-friendly name. One of only two mutable fields on this resource.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form description. The other mutable field.",
			},
			// Optional+Computed. See the same attribute on the cluster: an
			// unnamed pool is materialized by the platform and not drift-checked.
			"node_pool": schema.MapNestedAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Host counts per pool, keyed by the ClusterType node pool's id.\n\n" +
					"Unlike a cluster's `node_pool` this is not sparse: the platform provisions " +
					"exactly the pools named here. Growing a pool scales in place. **Shrinking a " +
					"pool, or adding or removing one, replaces the whole group**, because the API " +
					"implements scale up only and rejects everything else on this field with " +
					"`409 CONFLICT_IMMUTABLE`.\n\n" +
					"A scale waits, within the `update` timeout, while another operation on the group " +
					"finishes or its quota accounting settles. A pool whose desired size disagrees with " +
					"the hosts it holds cannot be scaled until that is corrected, and a plan that grows " +
					"one warns before the apply fails." +
					knownGap("A pool's machine type is readable only as a name, not as hardware, "+
						"so choosing a pool for its hardware means writing its id as a literal."),
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"size": schema.Int64Attribute{
							Required:            true,
							MarkdownDescription: "Number of hosts to provision in this pool.",
							// The contract's int32, so a larger number can't
							// wrap on its way to the platform.
							Validators: []validator.Int64{int64validator.Between(0, math.MaxInt32)},
						},
					},
				},
				Validators: []validator.Map{
					describedMap{mapvalidator.SizeAtLeast(1), "must name at least one pool"},
					poolKeys(),
				},
				// Left out, the update sends no scale, so the pools read
				// back as they were.
				PlanModifiers: []planmodifier.Map{
					mapplanmodifier.UseStateForUnknown(),
					mapplanmodifier.RequiresReplaceIf(
						instanceGroupPoolsRequireReplace,
						"replaced when a pool shrinks, or when pools are added or removed",
						"replaced when a pool shrinks, or when pools are added or removed",
					),
				},
			},
			"ssh_access": schema.SetNestedAttribute{
				Optional: true,
				MarkdownDescription: "Linux users written onto every host in the group when it is provisioned, " +
					"each with the public keys allowed to log in as that user. Up to " +
					fmt.Sprint(k0rdent.InstanceGroupCreateRequestSshAccessMaxItems) + " users. Only public keys " +
					"go here. The contract says the platform never stores or generates a private key.\n\n" +
					"Set it. A group with no SSH keys can fail with `INVALID_CONFIGURATION`.\n\n" +
					"**Changing it replaces the whole group**, unless it's left out or empty and the " +
					"platform already reports no users. The platform returns the users sorted " +
					"by username, so the order they are written in here makes no difference.",
				Validators: []validator.Set{setvalidator.SizeAtMost(k0rdent.InstanceGroupCreateRequestSshAccessMaxItems)},
				PlanModifiers: []planmodifier.Set{setplanmodifier.RequiresReplaceIf(sshAccessRequiresReplace,
					"replaced when it changes, unless the platform already reports none and none is configured",
					"replaced when it changes, unless the platform already reports none and none is configured")},
				NestedObject: sshUserNestedObject(),
			},
			"state": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Lifecycle state as last read from the platform.",
			},
			"self_link": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Platform-defined reference to this instance group.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"vpc": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Id of the VPC this instance group brought with it. Name the VPC through " +
					"this rather than a data source: it stays known while the instance group changes, so " +
					"a peering or a security-group binding that names it is not replaced. A peering takes it " +
					"as `vpc`, and a peering in this project that points at it takes it as `remote_vpc`.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// instanceGroupPoolsRequireReplace decides between a scale and a replacement.
//
// Scale up is the only in-place change the API offers on this field, so a
// smaller size, a new pool or a removed pool has to be a replacement. Saying so
// in the plan is what stops an apply failing halfway with a 409 or a 422 after
// the user has approved it.
func instanceGroupPoolsRequireReplace(ctx context.Context, req planmodifier.MapRequest, resp *mapplanmodifier.RequiresReplaceIfFuncResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return // the platform's counts are being adopted, not changed
	}

	have, diags := instanceGroupPoolsFrom(ctx, req.StateValue)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Pool ids are map keys, so they are known even when a size is not.
	want := req.ConfigValue.Elements()
	if len(want) != len(have) {
		resp.RequiresReplace = true
		return
	}
	for id, pool := range want {
		current, known := have[id]
		if !known {
			resp.RequiresReplace = true
			return
		}
		// A size known only at apply is decided again once it is known.
		if size, ok := poolSize(pool); ok && size < current {
			resp.RequiresReplace = true
			return
		}
	}
}

func poolSize(pool attr.Value) (int64, bool) {
	obj, ok := pool.(types.Object)
	if !ok || obj.IsNull() || obj.IsUnknown() {
		return 0, false
	}
	size, ok := obj.Attributes()["size"].(types.Int64)
	if !ok || size.IsNull() || size.IsUnknown() {
		return 0, false
	}
	return size.ValueInt64(), true
}

func (r *instanceGroupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (r *instanceGroupResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	modifyScopePlan(ctx, r.meta, req, resp)
	if resp.Diagnostics.HasError() {
		return
	}

	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	// A create an earlier apply stopped waiting for plans an update that
	// waits for it, though nothing in the configuration moved.
	planResume(ctx, req, resp)
	if resp.Plan.Raw.Equal(req.State.Raw) {
		return
	}
	planSettledState(ctx, instanceGroupStates.settle, req, resp)
	r.warnDivergedPools(ctx, req, resp)
}

func (r *instanceGroupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan instanceGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, plan.Region, plan.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, plan.Timeouts.Create, instanceGroupStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}

	pools, diags := instanceGroupPoolsFrom(ctx, plan.NodePool)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if len(pools) == 0 {
		// Local: configuration validation, no request made.
		resp.Diagnostics.AddError(
			"An instance group needs at least one node pool",
			"`node_pool` is empty. Unlike a cluster, an instance group provisions exactly the "+
				"pools it is given, so there is no ClusterType default to fall back on. "+
				"Pool ids come from the bound ClusterType version.",
		)
		return
	}

	// An instance group has no tags, so it carries no create token, and its
	// id comes from configuration, so a group found after a lost answer can't
	// be shown to be this one: that's reported with the import command.
	key, err := newCreateKey(plan.ID, false)
	if err != nil {
		addCreateKeyError(&resp.Diagnostics, "instance group", err)
		return
	}
	sshAccess, diags := sshAccessRequest(ctx, plan.SSHAccess)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := k0rdent.InstanceGroupCreateRequest{
		Id:          &key.id,
		ClusterType: plan.ClusterType.ValueString(),
		SshAccess:   sshAccess,
	}
	md, diags := plan.metadata().create(ctx)
	resp.Diagnostics.Append(diags...)
	body.DisplayName, body.Description = md.DisplayName, md.Description
	for _, poolID := range sortedKeys(pools) {
		entry := zeroElem(body.NodePools)
		// The contract sets `format: int32` on this count and not on the
		// cluster twin's, so the generated widths differ. Narrowing here rather
		// than in the model keeps the Terraform type int64, which is the only
		// integer Terraform has.
		size := int32(pools[poolID]) //nolint:gosec // bounded by the ClusterType's nodeCountMax
		entry.NodePool = poolID
		entry.Size = &size
		body.NodePools = append(body.NodePools, entry)
	}

	create := convergingCreate{
		key: key, kind: "instance group", what: "create the instance group",
		resourceName: ResourceName(instanceGroupResourceSuffix),
		importID:     scopedImportID(target),
		send: func(ctx context.Context) createAnswer {
			created, err := r.meta.Client.CreateInstanceGroupWithResponse(ctx, target.region, target.project, body)
			if err != nil {
				return createAnswer{err: err}
			}
			a := createAnswer{resp: created.HTTPResponse, body: created.Body}
			if created.JSON201 != nil {
				a.id = created.JSON201.Id
			}
			return a
		},
		lookup: func(ctx context.Context, id string) createLookup {
			got, err := r.meta.Client.GetInstanceGroupWithResponse(ctx, target.region, target.project, id, nil)
			if err != nil {
				return createLookup{err: err}
			}
			return lookupFrom(got.HTTPResponse, got.Body, got.JSON200 != nil, func() string { return "" })
		},
		timeoutID: timeoutMetadataID("instanceGroupId"),
	}
	// The last body the wait read, so a failure's advice reads the reason the
	// platform reported rather than the provider's wording of it.
	var last *k0rdent.InstanceGroup
	created, found := createFinish[instanceGroupModel]{
		operation: "createInstanceGroup", target: target,
		reportWait: func(diags *diag.Diagnostics, summary string, err error) {
			addAdvisedError(diags, summary, missingSSHAccessAdvice(plan.SSHAccess, last), err)
		},
		wait: func(ctx context.Context, id string) error {
			read := r.reader(target, id)
			return instanceGroupStates.awaitSettled(ctx, r.meta.Wait, "the instance group to finish provisioning",
				func(ctx context.Context) (*k0rdent.InstanceGroup, *http.Response, []byte, error) {
					body, resp, raw, err := read(ctx)
					if body != nil {
						last = body
					}
					return body, resp, raw, err
				})
		},
		read: func(ctx context.Context, id string) (instanceGroupModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, id, plan)
		},
	}.run(ctx, create, req.Plan, &resp.State, &resp.Diagnostics)
	if found {
		resp.Diagnostics.Append(recordUID(ctx, resp.Private, created.uid)...)
	}
}

func (r *instanceGroupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State, &req.State)
	var prior instanceGroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, prior.Region, prior.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	state, found, diags := r.read(ctx, target, prior.ID.ValueString(), prior)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if found && prior.ClusterType.IsNull() {
		// An import: see importedClusterTypeRef.
		state.ClusterType = importedClusterTypeRef(ctx, r.meta, target.region, state.ClusterType)
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	if !refreshedOwnObject(ctx, req.Private, resp, "instance group", prior.ID.ValueString(), state.uid) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
	resp.Diagnostics.Append(recordReleased(ctx, resp.Private, state.SSHAccessHeld)...)
}

func (r *instanceGroupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan, state instanceGroupModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, state.Region, state.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	id := state.ID.ValueString()
	if (resumable[instanceGroupModel]{
		kind: "instance group", resourceName: ResourceName(instanceGroupResourceSuffix),
		timeout: plan.Timeouts.Create, fallback: instanceGroupStates.timeout(r.meta.Wait),
		wait: func(ctx context.Context) error {
			return r.waitForSettled(ctx, target, id, "the instance group to finish provisioning")
		},
		read: func(ctx context.Context) (instanceGroupModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, id, plan)
		},
	}).resume(ctx, req, resp) {
		return
	}
	ctx, cancel := withTimeout(ctx, plan.Timeouts.Update, r.meta.Wait.Timeout, &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}

	md, changed, diags := plan.metadata().patch(ctx, state.metadata())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if changed {
		updated, err := r.meta.Client.UpdateInstanceGroupWithResponse(ctx, target.region, target.project, id,
			k0rdent.InstanceGroupUpdateRequest{
				DisplayName: md.DisplayName,
				Description: md.Description,
			})
		if err != nil || (updated.JSON200 == nil && updated.JSON202 == nil) {
			var respBody []byte
			var httpResp *http.Response
			if updated != nil {
				respBody, httpResp = updated.Body, updated.HTTPResponse
			}
			addAPIError(&resp.Diagnostics, "update the instance group", httpResp, respBody, err)
			return
		}
	}

	scaled, diags := r.scaleUp(ctx, target, id, plan, state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// A change that sent nothing, such as dropping `ssh_access` a failed
	// group no longer has, has nothing to wait for, and a failed group
	// would never settle.
	if changed || scaled {
		if err := r.waitForSettled(ctx, target, id, "the instance group to settle"); err != nil {
			addWaitError(&resp.Diagnostics, "The instance group did not settle after the change", err)
			return
		}
	}

	next, found, diags, err := settledRead(ctx, plan.State, instanceGroupStates.settle,
		func(ctx context.Context) (instanceGroupModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, id, plan)
		},
		func(m instanceGroupModel) types.String { return m.State },
		func(ctx context.Context) error {
			return r.waitForSettled(ctx, target, id, "the instance group to settle")
		})
	resp.Diagnostics.Append(diags...)
	if err != nil {
		addWaitError(&resp.Diagnostics, "The instance group did not settle after the change", err)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		// Local: the provider's conclusion from two calls, as in createFinish.
		resp.Diagnostics.AddError(
			"The instance group disappeared during the update",
			fmt.Sprintf("Reading %q after the change answered 404.", id),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

// scaleUp grows the pools whose size increased. A shrink never reaches here,
// because the plan modifier turns it into a replacement, so anything that is
// not an increase is skipped. It reports whether it sent a scale.
func (r *instanceGroupResource) scaleUp(ctx context.Context, target clusterTarget, id string, plan, state instanceGroupModel) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	want, d := instanceGroupPoolsFrom(ctx, plan.NodePool)
	diags.Append(d...)
	have, d := instanceGroupPoolsFrom(ctx, state.NodePool)
	diags.Append(d...)
	if diags.HasError() {
		return false, diags
	}

	var body k0rdent.InstanceGroupScaleRequest
	pools := derefOrNil(body.NodePools)
	for _, poolID := range sortedKeys(want) {
		size, current := want[poolID], have[poolID]
		if size <= current {
			continue
		}
		entry := zeroElem(pools)
		entry.NodePool = poolID
		entry.Size = int32(size) //nolint:gosec // bounded by the ClusterType's nodeCountMax
		pools = append(pools, entry)
	}
	if len(pools) == 0 {
		return false, diags
	}
	body.NodePools = &pools
	return true, r.scaleOnce(ctx, target, id, body)
}

// scaleOnce sends one scale and waits until the platform admits it.
//
// Two answers are a wait rather than a refusal, and the platform writes nothing
// on either, so resending is safe. A reason-less 409 on an active group is its
// quota accounting settling, which the contract says clears within seconds, and
// on a creating or updating group it is another operation in flight, which
// clears when that finishes. A 503 is a pool size the platform could not
// confirm, which it says clears on its own, but the service also answers 503
// for causes that never clear, so a 503 is tolerated only for the provisional
// window. Everything else is the answer.
func (r *instanceGroupResource) scaleOnce(ctx context.Context, target clusterTarget, id string, body k0rdent.InstanceGroupScaleRequest) diag.Diagnostics {
	var diags diag.Diagnostics

	var scaled *k0rdent.ScaleInstanceGroupResponse
	var outcome scaleOutcome
	attempt := 0
	err := awaitAdmitted(ctx, r.meta.Wait, "the instance group to admit the scale",
		func(ctx context.Context) (*http.Response, []byte, error) {
			attempt++
			var err error
			scaled, err = r.meta.Client.ScaleInstanceGroupWithResponse(ctx, target.region, target.project, id, body)
			if err != nil {
				return nil, nil, err
			}
			return scaled.HTTPResponse, scaled.Body, nil
		},
		func(ctx context.Context, resp *http.Response, raw []byte) (bool, *k0rdent.APIError, error) {
			if scaled.JSON200 != nil {
				return true, nil, nil
			}
			var refusal *k0rdent.APIError
			if !errors.As(k0rdent.CheckResponse(resp, raw), &refusal) {
				return false, nil, answeredWithout(resp, "instance group")
			}
			switch {
			case refusal.StatusCode == http.StatusServiceUnavailable:
				logScaleRetry(ctx, id, attempt, refusal)
				return false, refusal, provisionalFailure{refusal}
			case isLifecycleConflict(refusal):
				got, readErr := r.meta.Client.GetInstanceGroupWithResponse(ctx, target.region, target.project, id, nil)
				if readErr != nil {
					return false, nil, readErr
				}
				if got.JSON200 == nil || got.JSON200.State == nil {
					return false, nil, refusal
				}
				switch state := *got.JSON200.State; state {
				case k0rdent.InstanceGroupStateActive, k0rdent.InstanceGroupStateCreating,
					k0rdent.InstanceGroupStateUpdating:
				case k0rdent.InstanceGroupStateFailed, k0rdent.InstanceGroupStateDeleting,
					k0rdent.InstanceGroupStateDeleted:
					outcome.blocked = string(state)
					return false, nil, refusal
				default:
					return false, nil, unknownStateError("instance group", string(state))
				}
			default:
				return false, nil, refusal
			}
			logScaleRetry(ctx, id, attempt, refusal)
			return false, refusal, nil
		})
	if err == nil {
		return diags
	}
	outcome.timedOut = isRefusedUntilTimeout(err)
	outcome.interrupted = isWriteInFlight(err)
	var apiErr *k0rdent.APIError
	errors.As(err, &apiErr)
	addAdvisedError(&diags, "Could not scale the instance group", outcome.advice(apiErr), err)
	return diags
}

func logScaleRetry(ctx context.Context, id string, attempt int, refusal *k0rdent.APIError) {
	tflog.Info(ctx, "the platform has not admitted the instance group scale yet, retrying", map[string]any{
		"instance_group": id,
		"attempt":        attempt,
		"status":         refusal.StatusCode,
		"request_id":     refusal.RequestID,
	})
}

// scaleOutcome is what scaleOnce learned on the way to failing.
type scaleOutcome struct {
	blocked     string // the state of a group whose conflict waiting cannot clear
	timedOut    bool   // the timeout ran out while the platform kept refusing
	interrupted bool   // the timeout ran out with a scale request on its way
}

// isLifecycleConflict reports the 409 the contract calls reason-less. The
// service writes it with details[].reason CONFLICT_IN_USE, so that counts too.
func isLifecycleConflict(e *k0rdent.APIError) bool {
	if e.StatusCode != http.StatusConflict || e.Reason != "" {
		return false
	}
	for _, reason := range e.DetailReasons() {
		if reason != "CONFLICT_IN_USE" {
			return false
		}
	}
	return true
}

// advice says what to do about a scale the platform refused, or about one
// that may have landed. e is nil when no answer arrived.
func (o scaleOutcome) advice(e *k0rdent.APIError) string {
	switch {
	case o.interrupted:
		return "The timeout ran out while a scale request was on its way, so the platform may have " +
			"admitted it. Run `terraform apply -refresh-only` before applying again."
	case e == nil:
		return ""
	case e.StatusCode == http.StatusServiceUnavailable:
		return "The platform kept answering 503 to the scale, and its message above says why. Some of " +
			"these clear on their own and some need an operator. Nothing was scaled. Apply again later, " +
			"and quote the request id if it persists."
	case o.timedOut:
		return "The platform was still refusing the scale when the `update` timeout ran out: another " +
			"operation on the group was in flight, or its quota accounting had not settled. Nothing was " +
			"scaled. Apply again, or raise the timeout."
	case o.blocked != "":
		return fmt.Sprintf("The instance group is %s, and the platform scales only an active group, "+
			"so waiting would not help.", o.blocked)
	case e.HasReason("QUOTA_EXCEEDED"):
		return quotaAdvice(quotaRefusals(e))
	case e.HasReason("POOL_COUNT_MISMATCH"):
		return "A pool's desired size disagrees with the hosts it holds, usually after a scale that " +
			"failed, and the platform will not scale it until that is corrected. The message above names " +
			"the number it holds. The correction is a scale naming exactly that number, which provisions " +
			"nothing. This provider does not send it, because a size below the one in state plans a " +
			"replacement. Send it outside Terraform, run `terraform apply -refresh-only`, then apply again."
	case e.HasReason("POOL_EMPTY"):
		return "The pool holds no hosts, and the platform cannot grow an empty pool yet: a scale extends " +
			"the pool's existing reservation, and an empty pool has none. Replacing the group with " +
			"`terraform apply -replace` provisions it at the configured size, along with every other pool."
	case e.HasReason("POOL_HOSTS_NOT_PROVISIONED"):
		return "A host in the pool has not finished provisioning, and the platform scales a pool only " +
			"once every host it holds has. Apply again when it finishes. A host that cannot recover has to " +
			"be removed from the pool's reservation first."
	case e.HasReason("RESERVATION_RELEASED"):
		return "The pool's reservation was released, so it holds no hosts and can never grow. The " +
			"platform's answer is a new instance group: replace this one with `terraform apply -replace`."
	case e.HasReason("RESERVATION_DRIFTED"):
		return "The compute and infrastructure services disagree about this pool's reservation, and " +
			"nothing was changed. This clears once they agree again. If it does not, an administrator has " +
			"to reconcile them, and the request id is what to quote."
	}
	return ""
}

func quotaAdvice(refusals []quotaRefusal) string {
	const unchanged = " Applying again unchanged gets the same answer."
	if len(refusals) == 0 {
		return "The scale does not fit the quota. Grow the pools by less, free capacity, or have the limit raised." +
			unchanged
	}
	tightest := refusals[0]
	for _, q := range refusals[1:] {
		if q.Admissible < tightest.Admissible {
			tightest = q
		}
	}
	room := "free capacity in the organization or the project, or have the binding limit raised."
	if tightest.BindingScope != "" {
		room = "free capacity under the " + tightest.BindingScope + " limit, or have it raised."
	}
	const increases = "Requested and admissible are increases summed across the pools this apply grows, " +
		"not target sizes. "
	if tightest.Admissible <= 0 {
		return increases + "No increase fits: " + room + unchanged
	}
	hosts := "hosts"
	if tightest.Admissible == 1 {
		hosts = "host"
	}
	return increases + fmt.Sprintf("Grow them by at most %d %s in total, or %s", tightest.Admissible, hosts, room) +
		unchanged
}

// warnDivergedPools warns when the plan grows a pool whose desired size and
// held hosts disagree, because the platform refuses to scale it. It warns
// rather than correcting: the correction lowers the desired size to what the
// pool holds, and that is the user's number to change.
func (r *instanceGroupResource) warnDivergedPools(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if r.meta == nil || r.meta.Client == nil {
		return
	}
	var planned, prior types.Map
	if req.Plan.GetAttribute(ctx, path.Root("node_pool"), &planned).HasError() ||
		req.State.GetAttribute(ctx, path.Root("node_pool"), &prior).HasError() {
		return
	}
	want, d1 := instanceGroupPoolsFrom(ctx, planned)
	have, d2 := instanceGroupPoolsFrom(ctx, prior)
	if d1.HasError() || d2.HasError() || len(want) != len(have) {
		return
	}
	grown := map[string]bool{}
	for poolID, size := range want {
		current, known := have[poolID]
		if !known || size < current {
			return // a replacement, not a scale
		}
		if size > current {
			grown[poolID] = true
		}
	}
	if len(grown) == 0 {
		return
	}

	var state, plan instanceGroupModel
	var scratch diag.Diagnostics
	if req.State.Get(ctx, &state).HasError() || req.Plan.Get(ctx, &plan).HasError() {
		return
	}
	// Attribute-level replacements are not visible in resp.RequiresReplace
	// here, so compare what would replace the group directly.
	if !plan.ClusterType.Equal(state.ClusterType) || !plan.ID.Equal(state.ID) ||
		!plan.Region.Equal(state.Region) || !plan.Project.Equal(state.Project) ||
		!plan.SSHAccess.Equal(state.SSHAccess) {
		return
	}
	target, ok := resolveScope(r.meta, state.Region, state.Project, &scratch)
	if !ok {
		return
	}
	// Advisory only: the apply reports a failed read or a refused scale itself.
	got, err := r.meta.Client.GetInstanceGroupWithResponse(ctx, target.region, target.project, state.ID.ValueString(), nil)
	if err != nil || got.JSON200 == nil || got.JSON200.NodePools == nil {
		fields := map[string]any{"instance_group": state.ID.ValueString()}
		if got != nil {
			fields = withRequestID(fields, got.HTTPResponse, got.Body)
		}
		tflog.Debug(ctx, "could not read the instance group's pools before planning a scale", fields)
		return
	}
	// In flight, desiredCount is already the new target while hosts arrive.
	if got.JSON200.State == nil || *got.JSON200.State != k0rdent.InstanceGroupStateActive {
		return
	}
	for _, p := range *got.JSON200.NodePools {
		if !grown[p.NodePool] || p.ReadyCount == nil || p.NotReadyCount == nil {
			continue
		}
		held := *p.ReadyCount + *p.NotReadyCount
		if held == p.DesiredCount {
			continue
		}
		resp.Diagnostics.AddAttributeWarning(path.Root("node_pool").AtMapKey(p.NodePool),
			fmt.Sprintf("Pool %q cannot be scaled until its desired size is corrected", p.NodePool),
			fmt.Sprintf("The platform says this pool asks for %d hosts and holds %d, usually after a scale "+
				"that could not find hosts. It refuses to scale a pool in that state (POOL_COUNT_MISMATCH), "+
				"so this apply will fail at the scale.\n\nThe correction is a scale naming %d, which records "+
				"what the pool holds and provisions nothing. This provider does not send it, because a size "+
				"below the one in state plans a replacement. Send it outside Terraform, run "+
				"`terraform apply -refresh-only`, then plan again.", p.DesiredCount, held, held))
	}
}

func (r *instanceGroupResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state instanceGroupModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, state.Region, state.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, state.Timeouts.Delete, instanceGroupStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	send := func(ctx context.Context) (*http.Response, []byte, error) {
		deleted, err := r.meta.Client.DeleteInstanceGroupWithResponse(ctx, target.region, target.project, id)
		if err != nil {
			return nil, nil, err
		}
		return deleted.HTTPResponse, deleted.Body, nil
	}
	instanceGroupStates.destroy(ctx, r.meta.Wait, send, r.reader(target, id), &resp.Diagnostics)
}

func (r *instanceGroupResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importScoped(ctx, r.meta, importForm{
		resource: ResourceName(instanceGroupResourceSuffix),
		key:      "id",
	}, req, resp)
}

// IdentitySchema is what an import block's `identity` takes.
func (r *instanceGroupResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = scopedIdentitySchema("id", "")
}

func (r *instanceGroupResource) read(ctx context.Context, target clusterTarget, id string, prior instanceGroupModel) (instanceGroupModel, bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	got, err := r.meta.Client.GetInstanceGroupWithResponse(ctx, target.region, target.project, id, nil)
	if err != nil {
		addAPIError(&diags, "read the instance group", nil, nil, err)
		return instanceGroupModel{}, false, diags
	}
	if isNotFound(got.HTTPResponse, got.Body) {
		return instanceGroupModel{}, false, diags
	}
	if got.JSON200 == nil {
		addAPIError(&diags, "read the instance group", got.HTTPResponse, got.Body, nil)
		return instanceGroupModel{}, false, diags
	}

	out, d := instanceGroupStateFrom(ctx, *got.JSON200, prior)
	diags.Append(d...)
	out.VPC = types.StringNull()
	if got.JSON200.Uid != nil {
		uid := got.JSON200.Uid.String()
		out.VPC, d = ownedVPC(ctx, r.meta, target, k0rdent.ListVPCsParams{InstanceGroupId: &uid})
		diags.Append(d...)
	}
	out.uid = uidOf(got.JSON200.Uid)
	return out, true, diags
}

// instanceGroupStateFrom maps an InstanceGroup onto the Terraform model.
func instanceGroupStateFrom(ctx context.Context, g k0rdent.InstanceGroup, prior instanceGroupModel) (instanceGroupModel, diag.Diagnostics) {
	var diags diag.Diagnostics

	md := prior.metadata().read(metadataAnswer{DisplayName: g.DisplayName, Description: g.Description})
	out := instanceGroupModel{

		ID:                  types.StringValue(g.Id),
		Region:              prior.Region,
		Timeouts:            prior.Timeouts,
		Project:             prior.Project,
		DisplayName:         md.DisplayName,
		Description:         md.Description,
		SelfLink:            stringOrNull(g.SelfLink),
		ClusterTypeResolved: types.StringValue(g.ClusterType),
		State:               types.StringNull(),
	}
	if g.State != nil {
		out.State = types.StringValue(string(*g.State))
	}
	sshAccess, d := sshAccessState(ctx, g.SshAccess, prior.SSHAccess)
	diags.Append(d...)
	out.SSHAccess, out.SSHAccessHeld = heldThroughTeardown(instanceGroupStates.releasedInputs(g),
		g.SshAccess == nil || len(*g.SshAccess) == 0, sshAccess, prior.SSHAccess)
	if out.SSHAccessHeld && g.State != nil && *g.State == k0rdent.InstanceGroupStateFailed {
		diags.AddWarning(
			"The platform reports no SSH users on this failed instance group",
			fmt.Sprintf("%q is `failed` and the platform answers no SSH users for it, which is what a "+
				"delete whose teardown failed leaves. State keeps the configured `ssh_access` so a "+
				"destroy can run again. If the group is meant to stay, only the platform's record of "+
				"those users is gone: SSH access is set when a host is provisioned and isn't removed "+
				"from a running one, so its hosts may still take their keys. To revoke them, destroy "+
				"the group or replace it with `terraform apply -replace`. Removing `ssh_access` from "+
				"the configuration only records that state has none, and changes nothing on the hosts.", g.Id),
		)
	}

	configured := prior.ClusterType.ValueString()
	switch {
	case configured == "":
		out.ClusterType = types.StringValue(g.ClusterType)
	case clusterTypeRefEquivalent(configured, g.ClusterType) != refMismatch:
		out.ClusterType = prior.ClusterType
	default:
		out.ClusterType = types.StringValue(g.ClusterType)
	}

	// Sizes. `desiredCount` is the response's name for the requested `size`,
	// and it is required, so 0 is a real size. The live counts beside it move
	// with hardware and stay out of a managed resource. A response with no
	// `nodePools` at all keeps the known pools rather than planning to
	// destroy every one of them.
	if g.NodePools == nil && !prior.NodePool.IsNull() && !prior.NodePool.IsUnknown() {
		out.NodePool = prior.NodePool
		diags.AddWarning(
			"The platform did not report the node pools",
			fmt.Sprintf("`getInstanceGroup` returned %q with no `nodePools`. The known pools "+
				"are kept in state.", g.Id),
		)
		return out, diags
	}
	pools := map[string]instanceGroupPoolModel{}
	if g.NodePools != nil {
		for _, p := range *g.NodePools {
			pools[p.NodePool] = instanceGroupPoolModel{Size: types.Int64Value(int64(p.DesiredCount))}
		}
	}

	m, d := types.MapValueFrom(ctx, instanceGroupPoolObjectType(), pools)
	diags.Append(d...)
	out.NodePool = m

	return out, diags
}

func (r *instanceGroupResource) waitForSettled(ctx context.Context, target clusterTarget, id, what string) error {
	return instanceGroupStates.awaitSettled(ctx, r.meta.Wait, what, r.reader(target, id))
}

// reader reads the instance group for a wait.
func (r *instanceGroupResource) reader(target clusterTarget, id string) stateRead[k0rdent.InstanceGroup] {
	return func(ctx context.Context) (*k0rdent.InstanceGroup, *http.Response, []byte, error) {
		got, err := r.meta.Client.GetInstanceGroupWithResponse(ctx, target.region, target.project, id, nil)
		if err != nil {
			return nil, nil, nil, err
		}
		return got.JSON200, got.HTTPResponse, got.Body, nil
	}
}

// instanceGroupStates is what an instance group's waits make of each state it
// reports.
var instanceGroupStates = stateKind[k0rdent.InstanceGroup]{
	name:     "instance group",
	resource: instanceGroupResourceSuffix,
	long:     true,
	// "the group is in `updating` or `deleting` and cannot be deleted yet."
	deleting: string(k0rdent.InstanceGroupStateDeleting),
	// StartInstanceGroupDelete soft-deletes the SSH users as it moves the
	// group to `deleting`, and a failed teardown settles `failed` with them
	// still gone. See teardown_read.go.
	released: []string{string(k0rdent.InstanceGroupStateDeleting), string(k0rdent.InstanceGroupStateFailed),
		string(k0rdent.InstanceGroupStateDeleted)},
	state: func(g k0rdent.InstanceGroup) (string, bool) {
		if g.State == nil {
			return "", false
		}
		return string(*g.State), true
	},
	detail: func(g k0rdent.InstanceGroup) string {
		return stateDetail(g.StateMessage, reasonString(g.StateReason))
	},
	settle: stateTable{
		missing: stateRule{verdict: settled},
		states: map[string]stateRule{
			// `active` can carry the reason a previous scale failed. The group
			// is usable either way, so that does not fail the wait.
			string(k0rdent.InstanceGroupStateActive):   {verdict: settled},
			string(k0rdent.InstanceGroupStateCreating): {verdict: keepWaiting},
			string(k0rdent.InstanceGroupStateUpdating): {verdict: keepWaiting},
			string(k0rdent.InstanceGroupStateDeleting): {verdict: keepWaiting},
			string(k0rdent.InstanceGroupStateFailed):   {verdict: failsTerminally, detail: true},
			string(k0rdent.InstanceGroupStateDeleted):  {verdict: failsTerminally},
		},
	},
	// See the cluster's delete table: the four state sets, not two.
	delete: stateTable{
		missing: stateRule{verdict: keepWaiting},
		states: map[string]stateRule{
			string(k0rdent.InstanceGroupStateDeleted):  {verdict: settled},
			string(k0rdent.InstanceGroupStateDeleting): {verdict: keepWaiting},
			// A delete accepted while the group is creating cancels the create,
			// and the group reads creating, then briefly failed, then 404.
			string(k0rdent.InstanceGroupStateCreating): {verdict: keepWaiting},
			// A failed teardown also reads failed, and stays there, so failed
			// is tolerated for the provisional window rather than waited on for
			// the timeout.
			string(k0rdent.InstanceGroupStateFailed):   {verdict: failsProvisionally, detail: true},
			string(k0rdent.InstanceGroupStateActive):   {verdict: failsTerminally, says: afterDelete},
			string(k0rdent.InstanceGroupStateUpdating): {verdict: failsTerminally, says: afterDelete},
		},
	},
}

func instanceGroupPoolsFrom(ctx context.Context, m types.Map) (map[string]int64, diag.Diagnostics) {
	var diags diag.Diagnostics
	out := map[string]int64{}
	if m.IsNull() || m.IsUnknown() {
		return out, diags
	}
	var pools map[string]instanceGroupPoolModel
	diags.Append(m.ElementsAs(ctx, &pools, false)...)
	for id, p := range pools {
		if p.Size.IsNull() || p.Size.IsUnknown() {
			continue
		}
		out[id] = p.Size.ValueInt64()
	}
	return out, diags
}
