package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ resource.Resource                 = (*subnetResource)(nil)
	_ resource.ResourceWithConfigure    = (*subnetResource)(nil)
	_ resource.ResourceWithImportState  = (*subnetResource)(nil)
	_ resource.ResourceWithIdentity     = (*subnetResource)(nil)
	_ resource.ResourceWithModifyPlan   = (*subnetResource)(nil)
	_ resource.ResourceWithUpgradeState = (*subnetResource)(nil)
)

const subnetResourceSuffix = "subnet"

func newSubnetResource() resource.Resource {
	return guardReplacements(&subnetResource{}, subnetTenantState)
}

type subnetResource struct {
	meta *Meta
}

type subnetModel struct {
	ID              types.String   `tfsdk:"id"`
	Region          types.String   `tfsdk:"region"`
	Project         types.String   `tfsdk:"project"`
	Network         types.String   `tfsdk:"network"`
	AddressPool     types.String   `tfsdk:"address_pool"`
	PrefixLength    types.Int64    `tfsdk:"prefix_length"`
	AllocateGateway types.Bool     `tfsdk:"allocate_gateway"`
	ExcludeRanges   types.List     `tfsdk:"exclude_ranges"`
	DisplayName     types.String   `tfsdk:"display_name"`
	Description     types.String   `tfsdk:"description"`
	CIDR            types.String   `tfsdk:"cidr"`
	Gateway         types.String   `tfsdk:"gateway"`
	IPVersion       types.String   `tfsdk:"ip_version"`
	State           types.String   `tfsdk:"state"`
	SelfLink        types.String   `tfsdk:"self_link"`
	ClusterID       types.String   `tfsdk:"cluster_id"`
	Timeouts        timeouts.Value `tfsdk:"timeouts"`

	// uid is the platform's uid for the subnet read, which own_object.go
	// keeps in private state. Not an attribute.
	uid string
}

func (m subnetModel) metadata() tenantMetadata {
	return tenantMetadata{
		DisplayName: m.DisplayName,
		Description: m.Description,
	}
}

func (r *subnetResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = ResourceName(subnetResourceSuffix)
}

// UpgradeState migrates a state written at an earlier schema version. See
// state_history.go.
func (r *subnetResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	return stateHistories[subnetResourceSuffix].upgraders()
}

// Schema declares the resource.
//
// # Every attribute replaces, because there is no update
//
// There is no updateSubnet upstream, so every settable attribute carries
// RequiresReplace rather than meeting a 405 at apply. `display_name` replacing
// is the uncomfortable part: a rename tears down the CIDR allocation.
//
// # The CIDR is not settable
//
// `cidr` is allocated by the platform from the AddressPool and the contract
// says it "must not be supplied". Terraform users will reach for it, so it is
// Computed and documented rather than absent.
func (r *subnetResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	replaceString := []planmodifier.String{stringplanmodifier.RequiresReplace()}

	resp.Schema = schema.Schema{
		Version: stateHistories[subnetResourceSuffix].version(),
		MarkdownDescription: "A CIDR block allocated inside a `" + ResourceName(networkResourceSuffix) +
			"` from a regional AddressPool.\n\n" +
			"**Every attribute replaces the subnet.** The API offers no update operation, so a " +
			"change to anything, a display name included, is a destroy and recreate with a new " +
			"CIDR. `exclude_ranges` is compared as addresses, so writing the same addresses another " +
			"way is not a change. The API does not report `allocate_gateway`, so after an import " +
			"that cannot infer it, the first apply records the configured value in place.\n\n" +
			"Subnets created by a cluster are not managed here. They carry a `cluster_id`, " +
			"and `DELETE` against one answers `409`.\n\n" +
			"Requires `region` and `project` on the provider.",
		Blocks: map[string]schema.Block{
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
				MarkdownDescription: "Subnet name. Omit it and the provider generates one, `tf-` " +
					"and 16 hex digits, so that a create whose answer is lost can be read back rather " +
					"than sent twice.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
				Validators: idValidators(),
			},
			"network": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Id of the parent `" + ResourceName(networkResourceSuffix) +
					"`. A subnet cannot move between networks.",
				PlanModifiers: replaceString,
				Validators:    []validator.String{urlSafeIdentifier{}},
			},
			"address_pool": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "URI of the regional AddressPool the CIDR is allocated from. " +
					"Must resolve in the network's region.",
				PlanModifiers: replaceString,
			},
			"prefix_length": schema.Int64Attribute{
				Required: true,
				MarkdownDescription: "Prefix length to allocate, within the pool's " +
					"`[minPrefixLength, maxPrefixLength]`. `/31` is the RFC 3021 point-to-point " +
					"case and `/32` the single-host case, such as a control-plane VIP. When the pool " +
					"and this are known at plan, a value outside the pool's range, or larger than any " +
					"prefix it holds, fails the plan. The pool doesn't report its free space, so a pool " +
					"too full for this size still fails at create.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.RequiresReplace()},
				// The contract's bounds: subnets are IPv4 only today. The pool's
				// own narrower bounds need a read of another object, which a
				// validator can't make, so ModifyPlan checks them. See
				// subnet_pool_check.go.
				Validators: []validator.Int64{int64validator.Between(k0rdent.SubnetCreateRequestPrefixLengthMinimum,
					k0rdent.SubnetCreateRequestPrefixLengthMaximum)},
			},
			// Optional+Computed: the request field has no counterpart in the
			// response, so an import has to infer it from whether a gateway was
			// populated. A plain Optional would let the schema default fire on
			// the first plan after an import and propose a replacement.
			"allocate_gateway": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "Ask the platform to populate `gateway` with the last usable " +
					"host address. Only takes effect when the AddressPool is routable.",
				PlanModifiers: []planmodifier.Bool{boolplanmodifier.RequiresReplaceIf(replaceUnlessUnrecorded,
					"replaces the subnet when it changes. The platform doesn't report it, so after "+
						"importing a subnet on a pool that isn't routable the provider can't tell what it "+
						"was, and the first apply records the configured value without replacing anything",
					"replaces the subnet when it changes. The platform doesn't report it, so after "+
						"importing a subnet on a pool that isn't routable the provider can't tell what it "+
						"was, and the first apply records the configured value without replacing anything")},
			},
			// A List, not a Set: the contract accepts three syntaxes for the
			// same addresses, so two configurations that mean the same thing
			// are not equal as strings and set semantics would not help.
			//
			// Optional+Computed: the response carries the ranges the platform
			// applied, which need not be the ones requested, so a configured
			// value stands and an omitted one takes what came back and keeps it.
			"exclude_ranges": schema.ListAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Addresses inside the allocated CIDR to keep out of host " +
					"allocation. A single IP (`10.0.0.4`), a CIDR (`10.0.0.0/29`) or a range " +
					"(`10.0.0.10-10.0.0.20`). Left out, it reports what the platform applied, and " +
					"removing it from configuration keeps those addresses excluded. Set it to `[]` " +
					"to clear them, which replaces the subnet.",
				PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown(), listplanmodifier.RequiresReplaceIf(replaceExcludeRanges,
					"Replaces the subnet when the excluded addresses change.",
					"Replaces the subnet when the excluded addresses change.")},
			},
			"display_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Human-friendly name. The API has no update for a subnet, so even this can't change in place.",
				PlanModifiers:       replaceString,
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form text describing it, for people to read.",
				PlanModifiers:       replaceString,
			},

			// The allocation is fixed once the subnet is active, and still to
			// come on one imported while it was being made, so these three
			// keep their values only once it has settled.
			"cidr": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The allocated block, e.g. `10.0.1.0/24`. Chosen by the " +
					"platform from the AddressPool and empty until the subnet is `active`.",
				PlanModifiers: []planmodifier.String{keepUnlessChanged().onceSettled(subnetStates.settle)},
			},
			"gateway": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The resolved gateway address, when `allocate_gateway` was set " +
					"and the pool is routable. Null otherwise.",
				PlanModifiers: []planmodifier.String{keepUnlessChanged().onceSettled(subnetStates.settle)},
			},
			"ip_version": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "IP family of the allocated CIDR. IPv4 only, today.",
				PlanModifiers:       []planmodifier.String{keepUnlessChanged().onceSettled(subnetStates.settle)},
			},
			"state": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Lifecycle state as last read from the platform.",
			},
			"self_link": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Platform-defined reference to this subnet.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"cluster_id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "UID of the cluster that provisioned this subnet. Present only " +
					"on subnets the cluster lifecycle owns, which cannot be written through this " +
					"resource.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *subnetResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (r *subnetResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	modifyScopePlan(ctx, r.meta, req, resp)
	refusePlatformOwned(ctx, "cluster_id", "", req, resp)
	if resp.Diagnostics.HasError() {
		return
	}
	checkSubnetFitsPool(ctx, r.meta, req, resp)
	if resp.Diagnostics.HasError() {
		return
	}
	planResume(ctx, req, resp)
	planSettledState(ctx, subnetStates.settle, req, resp)
}

func (r *subnetResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan subnetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, plan.Region, plan.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, plan.Timeouts.Create, subnetStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}

	excludes, diags := optionalStringList(ctx, plan.ExcludeRanges)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	key, err := newCreateKey(plan.ID, true)
	if err != nil {
		addCreateKeyError(&resp.Diagnostics, "subnet", err)
		return
	}
	network := plan.Network.ValueString()
	// This resource does not manage a subnet's tags, and the create token is
	// the only one it sends.
	body := k0rdent.SubnetCreateRequest{
		Id:              &key.id,
		Tags:            key.tagged(nil),
		AddressPool:     plan.AddressPool.ValueString(),
		PrefixLength:    int(plan.PrefixLength.ValueInt64()),
		AllocateGateway: optionalBool(plan.AllocateGateway),
		ExcludeRanges:   excludes,
	}
	md, diags := plan.metadata().create(ctx)
	resp.Diagnostics.Append(diags...)
	body.DisplayName, body.Description = md.DisplayName, md.Description

	create := convergingCreate{
		key: key, kind: "subnet", what: "create the subnet",
		resourceName: ResourceName(subnetResourceSuffix),
		importID:     nestedScopedImportID(target, network),
		send: func(ctx context.Context) createAnswer {
			created, err := r.meta.Client.CreateSubnetWithResponse(ctx, target.region, target.project, network, body)
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
			got, err := r.meta.Client.GetSubnetWithResponse(ctx, target.region, target.project, network, id)
			if err != nil {
				return createLookup{err: err}
			}
			return lookupFrom(got.HTTPResponse, got.Body, got.JSON200 != nil, func() (string, string) {
				return createTokenOf(presentMap(got.JSON200.Tags)), uidOf(got.JSON200.Uid)
			})
		},
		busy: newBusyRetry("createSubnet", r.meta.Wait, r.dependencies(plan.AddressPool.ValueString())...),
	}
	created, found := createFinish[subnetModel]{
		operation: "createSubnet", target: target,
		parentAttr: "network", parentID: network,
		wait: func(ctx context.Context, id string) error {
			return r.waitForSettled(ctx, target, network, id)
		},
		read: func(ctx context.Context, id string) (subnetModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, network, id, plan)
		},
	}.run(ctx, create, req.Plan, &resp.State, resp.Private, &resp.Diagnostics)
	if found {
		resp.Diagnostics.Append(recordUID(ctx, resp.Private, created.uid)...)
	}
}

func (r *subnetResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State, &req.State)
	var prior subnetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, prior.Region, prior.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	state, found, diags := r.read(ctx, target, prior.Network.ValueString(), prior.ID.ValueString(), prior)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	if !refreshedOwnObject(ctx, req.Private, resp, "subnet", prior.ID.ValueString(), state.uid) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update is unreachable: every settable attribute carries RequiresReplace
// because the API has no updateSubnet. It reports rather than silently
// succeeding, because a silent success here writes a state that the platform
// never agreed to.
// Update makes no request: the API has none. What reaches here is a new
// `timeouts` block, or a value adopted after an import, and both only need
// writing to state.
func (r *subnetResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan, state subnetModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, state.Region, state.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	id, network := state.ID.ValueString(), state.Network.ValueString()
	if (resumable[subnetModel]{
		kind: "subnet", resourceName: ResourceName(subnetResourceSuffix),
		timeout: plan.Timeouts.Create, fallback: subnetStates.timeout(r.meta.Wait),
		wait: func(ctx context.Context) error { return r.waitForSettled(ctx, target, network, id) },
		read: func(ctx context.Context) (subnetModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, network, id, plan)
		},
	}).resume(ctx, req, resp) {
		return
	}
	next, found, diags, err := settledRead(ctx, plan.State, subnetStates.settle,
		func(ctx context.Context) (subnetModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, network, id, plan)
		},
		func(m subnetModel) types.String { return m.State },
		func(ctx context.Context) error { return r.waitForSettled(ctx, target, network, id) })
	resp.Diagnostics.Append(diags...)
	if err != nil {
		addWaitError(&resp.Diagnostics, "The subnet did not settle", err)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		// Local: the provider's conclusion from two calls, as in createFinish.
		resp.Diagnostics.AddError(
			"The subnet disappeared during the update",
			fmt.Sprintf("Reading %q after the change answered 404.", id),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

// replaceUnlessUnrecorded is RequiresReplace for a value the API does not
// report. Null in state means an import could not recover it, and the first
// apply records the configured value instead of replacing the subnet.
func replaceUnlessUnrecorded(_ context.Context, req planmodifier.BoolRequest, resp *boolplanmodifier.RequiresReplaceIfFuncResponse) {
	resp.RequiresReplace = !req.StateValue.IsNull()
}

// replaceExcludeRanges replaces the subnet when the excluded addresses change.
// The contract accepts a single IP, a CIDR or a range for the same addresses,
// so a respelling is not a change.
func replaceExcludeRanges(ctx context.Context, req planmodifier.ListRequest, resp *listplanmodifier.RequiresReplaceIfFuncResponse) {
	if req.PlanValue.IsUnknown() || req.ConfigValue.IsNull() {
		return
	}
	var plan, state []string
	resp.Diagnostics.Append(req.PlanValue.ElementsAs(ctx, &plan, false)...)
	resp.Diagnostics.Append(req.StateValue.ElementsAs(ctx, &state, false)...)
	resp.RequiresReplace = !sameAddresses(plan, state)
}

// sameAddresses reports whether two exclude lists cover the same addresses.
// Anything that does not parse is compared as written.
func sameAddresses(a, b []string) bool {
	x, okA := addressSet(a)
	y, okB := addressSet(b)
	if !okA || !okB {
		return slices.Equal(a, b)
	}
	return slices.Equal(x, y)
}

type addrRange struct{ lo, hi netip.Addr }

// addressSet turns an exclude list into sorted, merged ranges.
func addressSet(items []string) ([]addrRange, bool) {
	var out []addrRange
	for _, item := range items {
		var r addrRange
		if lo, hi, ok := strings.Cut(item, "-"); ok {
			var err1, err2 error
			r.lo, err1 = netip.ParseAddr(lo)
			r.hi, err2 = netip.ParseAddr(hi)
			if err1 != nil || err2 != nil {
				return nil, false
			}
		} else if p, err := netip.ParsePrefix(item); err == nil {
			p = p.Masked()
			r.lo, r.hi = p.Addr(), lastAddr(p)
		} else if a, err := netip.ParseAddr(item); err == nil {
			r.lo, r.hi = a, a
		} else {
			return nil, false
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(x, y addrRange) int { return x.lo.Compare(y.lo) })
	var merged []addrRange
	for _, r := range out {
		if n := len(merged); n > 0 && (!merged[n-1].hi.Next().IsValid() || merged[n-1].hi.Next().Compare(r.lo) >= 0) {
			if r.hi.Compare(merged[n-1].hi) > 0 {
				merged[n-1].hi = r.hi
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged, true
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().AsSlice()
	for i := p.Bits(); i < len(b)*8; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	a, _ := netip.AddrFromSlice(b)
	return a
}

func (r *subnetResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state subnetModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if forgetPlatformOwned(ctx, "cluster_id", "", req.State, &resp.Diagnostics) {
		return
	}
	target, ok := resolveScope(r.meta, state.Region, state.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, state.Timeouts.Delete, subnetStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}
	network, id := state.Network.ValueString(), state.ID.ValueString()

	send := func(ctx context.Context) (*http.Response, []byte, error) {
		deleted, err := r.meta.Client.DeleteSubnetWithResponse(ctx, target.region, target.project, network, id)
		if err != nil {
			return nil, nil, err
		}
		return deleted.HTTPResponse, deleted.Body, nil
	}
	subnetStates.destroy(ctx, r.meta.Wait, send, r.reader(target, network, id), &resp.Diagnostics)
}

// ImportState takes the parent network too, because a subnet's id is unique
// only inside its network and every operation needs both.
func (r *subnetResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importScoped(ctx, r.meta, importForm{
		resource: ResourceName(subnetResourceSuffix),
		key:      "id",
		parent:   "network",
		example:  "training-fabric/pods",
	}, req, resp)
}

// IdentitySchema is what an import block's `identity` takes.
func (r *subnetResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = scopedIdentitySchema("id", "network")
}

func (r *subnetResource) read(ctx context.Context, target clusterTarget, network, id string, prior subnetModel) (subnetModel, bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	got, err := r.meta.Client.GetSubnetWithResponse(ctx, target.region, target.project, network, id)
	if err != nil {
		addAPIError(&diags, "read the subnet", nil, nil, err)
		return subnetModel{}, false, diags
	}
	if isNotFound(got.HTTPResponse, got.Body) {
		return subnetModel{}, false, diags
	}
	if got.JSON200 == nil {
		addAPIError(&diags, "read the subnet", got.HTTPResponse, got.Body, nil)
		return subnetModel{}, false, diags
	}

	out, d := subnetStateFrom(ctx, *got.JSON200, network, prior)
	diags.Append(d...)
	if out.AllocateGateway.IsNull() && deref(got.JSON200.Gateway) == "" && r.poolIsRoutable(ctx, target.region, got.JSON200.AddressPool) {
		// A routable pool fills `gateway` whenever the flag was set, so an
		// empty one there proves it wasn't. subnetStateFrom can't tell
		// without the pool, and null would have the plan after an import set
		// the default it already has.
		out.AllocateGateway = types.BoolValue(false)
	}
	out.uid = uidOf(got.JSON200.Uid)
	return out, true, diags
}

// poolIsRoutable reads whether the pool a subnet names is routable, in the
// region its URI names, through the memo the plan-time check uses. Anything
// short of an answer saying so is false: the flag stays unrecorded, as it
// always was, rather than a failed lookup failing the read.
func (r *subnetResource) poolIsRoutable(ctx context.Context, region string, ref *string) bool {
	poolRegion, id, ok := addressPoolRef(deref(ref), types.StringValue(region))
	if !ok {
		return false
	}
	pool, ok := r.meta.addressPool(ctx, poolRegion, id)
	return ok && pool.Routable
}

func (r *subnetResource) waitForSettled(ctx context.Context, target clusterTarget, network, id string) error {
	return subnetStates.awaitSettled(ctx, r.meta.Wait, "the subnet's CIDR to be allocated", r.reader(target, network, id))
}

// reader reads the subnet for a wait.
func (r *subnetResource) reader(target clusterTarget, network, id string) stateRead[k0rdent.Subnet] {
	return func(ctx context.Context) (*k0rdent.Subnet, *http.Response, []byte, error) {
		got, err := r.meta.Client.GetSubnetWithResponse(ctx, target.region, target.project, network, id)
		if err != nil {
			return nil, nil, nil, err
		}
		return got.JSON200, got.HTTPResponse, got.Body, nil
	}
}

// subnetStates is what a subnet's waits make of each state it reports.
//
// A subnet declares no `updating`, so that value arriving here is a newer API
// and stops the wait rather than being assumed transient.
var subnetStates = stateKind[k0rdent.Subnet]{
	name:     "subnet",
	resource: subnetResourceSuffix,
	// "the subnet is not in a deletable state (e.g. still `creating` or
	// already `deleting`)."
	deleting: string(k0rdent.SubnetStateDeleting),
	state: func(n k0rdent.Subnet) (string, bool) {
		return stateString(n.State), stateString(n.State) != ""
	},
	detail: func(n k0rdent.Subnet) string {
		return stateDetail(n.StateMessage, reasonString(n.StateReason))
	},
	settle: stateTable{
		missing: stateRule{verdict: settled},
		states: map[string]stateRule{
			string(k0rdent.SubnetStateActive):   {verdict: settled},
			string(k0rdent.SubnetStateCreating): {verdict: keepWaiting},
			string(k0rdent.SubnetStateDeleting): {verdict: keepWaiting},
			string(k0rdent.SubnetStateFailed):   {verdict: failsTerminally, detail: true},
		},
	},
	delete: stateTable{
		missing: stateRule{verdict: keepWaiting},
		states: map[string]stateRule{
			string(k0rdent.SubnetStateDeleting): {verdict: keepWaiting},
			string(k0rdent.SubnetStateFailed):   {verdict: failsTerminally, detail: true},
			string(k0rdent.SubnetStateActive):   {verdict: failsTerminally, says: afterDelete},
			string(k0rdent.SubnetStateCreating): {verdict: failsTerminally, says: afterDelete},
		},
	},
}

// subnetStateFrom maps a Subnet onto the Terraform model.
func subnetStateFrom(ctx context.Context, s k0rdent.Subnet, network string, prior subnetModel) (subnetModel, diag.Diagnostics) {
	var diags diag.Diagnostics

	md := prior.metadata().read(metadataAnswer{DisplayName: s.DisplayName, Description: s.Description})
	out := subnetModel{

		ID:              types.StringValue(deref(s.Id)),
		Region:          prior.Region,
		Project:         prior.Project,
		Timeouts:        prior.Timeouts,
		Network:         types.StringValue(network),
		PrefixLength:    types.Int64Value(int64(s.PrefixLength)),
		AllocateGateway: prior.AllocateGateway,
		DisplayName:     md.DisplayName,
		Description:     md.Description,
		CIDR:            stringOrNull(s.Cidr),
		Gateway:         stringOrNull(s.Gateway),
		IPVersion:       types.StringNull(),
		State:           types.StringNull(),
		SelfLink:        stringOrNull(s.SelfLink),
		ClusterID:       stringOrNull(s.ClusterId),
	}
	if s.IpVersion != nil {
		out.IPVersion = types.StringValue(string(*s.IpVersion))
	}
	if s.State != nil {
		out.State = types.StringValue(string(*s.State))
	}

	// Never echoed: the request field has no counterpart in the response. An
	// import has no prior value, and only a populated `gateway` proves the flag
	// was set. An empty one fits either value on a pool that is not routable,
	// so it is recorded as null: see replaceUnlessUnrecorded.
	if prior.AllocateGateway.IsNull() || prior.AllocateGateway.IsUnknown() {
		out.AllocateGateway = types.BoolNull()
		if s.Gateway != nil && *s.Gateway != "" {
			out.AllocateGateway = types.BoolValue(true)
		}
	}

	// `addressPool` is absent on a cluster-provisioned subnet, whose CIDR came
	// from the cluster lifecycle rather than a pool the tenant named. Nulling
	// it out of state on that basis would propose replacing a subnet nobody can
	// replace, so the configured value stands.
	out.AddressPool = keepConfigured(prior.AddressPool, s.AddressPool)

	// The read can spell the ranges differently from the request, so a
	// configured list is kept. replaceExcludeRanges compares them as addresses.
	ranges, d := stringListOrNull(ctx, presentList(s.ExcludeRanges))
	diags.Append(d...)
	out.ExcludeRanges = ranges
	if !prior.ExcludeRanges.IsNull() && !prior.ExcludeRanges.IsUnknown() {
		out.ExcludeRanges = prior.ExcludeRanges
	}

	return out, diags
}

// dependencies is the AddressPool a subnet create is refused against while it
// isn't active. A pool `creating` or `updating` settles on its own, so the
// create waits for it. One `deleting` or `failed` doesn't, and its refusal is
// reported. A reference this can't read as a pool's names nothing to wait on.
func (r *subnetResource) dependencies(pool string) []dependency {
	p, ok := addressPoolRoute.params(pool)
	if !ok {
		return nil
	}
	region, id := p["region"], p["id"]
	read := func(ctx context.Context) (*k0rdent.AddressPool, *http.Response, []byte, error) {
		got, err := r.meta.Client.GetAddressPoolWithResponse(ctx, region, id)
		if err != nil {
			return nil, nil, nil, err
		}
		return got.JSON200, got.HTTPResponse, got.Body, nil
	}
	state := func(p k0rdent.AddressPool) (string, bool) {
		return stateString(p.State), stateString(p.State) != ""
	}
	return []dependency{dependsOn(fmt.Sprintf("the AddressPool %q", id), read, state,
		string(k0rdent.AddressPoolStateCreating), string(k0rdent.AddressPoolStateUpdating))}
}

// addressPoolRoute is getAddressPool's route, which an AddressPool's URI is:
// region and id.
var addressPoolRoute = routeOf("getAddressPool")
