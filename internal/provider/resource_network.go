package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ resource.Resource                   = (*networkResource)(nil)
	_ resource.ResourceWithConfigure      = (*networkResource)(nil)
	_ resource.ResourceWithImportState    = (*networkResource)(nil)
	_ resource.ResourceWithIdentity       = (*networkResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*networkResource)(nil)
	_ resource.ResourceWithValidateConfig = (*networkResource)(nil)
	_ resource.ResourceWithUpgradeState   = (*networkResource)(nil)
)

const networkResourceSuffix = "network"

func newNetworkResource() resource.Resource {
	return guardReplacements(&networkResource{}, networkTenantState)
}

type networkResource struct {
	meta *Meta
}

type networkModel struct {
	ID          types.String        `tfsdk:"id"`
	Region      types.String        `tfsdk:"region"`
	Project     types.String        `tfsdk:"project"`
	DisplayName types.String        `tfsdk:"display_name"`
	Description types.String        `tfsdk:"description"`
	Tags        types.Map           `tfsdk:"tags"`
	TagsAll     types.Map           `tfsdk:"tags_all"`
	EVPN        *networkEVPNModel   `tfsdk:"evpn"`
	InfiniBand  *networkIBModel     `tfsdk:"infiniband"`
	NVLink      *networkNVLinkModel `tfsdk:"nvlink"`
	Type        types.String        `tfsdk:"type"`
	State       types.String        `tfsdk:"state"`
	SelfLink    types.String        `tfsdk:"self_link"`
	VPC         types.String        `tfsdk:"vpc"`
	ClusterID   types.String        `tfsdk:"cluster_id"`
	Timeouts    timeouts.Value      `tfsdk:"timeouts"`

	// uid is the platform's uid for the network read, which own_object.go
	// keeps in private state. Not an attribute.
	uid string
}

func (m networkModel) metadata() tenantMetadata {
	return tenantMetadata{
		DisplayName: m.DisplayName,
		Description: m.Description,
		Tags:        m.Tags,
		TagsAll:     m.TagsAll,
	}
}

type networkEVPNModel struct {
	ServiceType    types.String `tfsdk:"service_type"`
	MTU            types.Int64  `tfsdk:"mtu"`
	DHCPEnabled    types.Bool   `tfsdk:"dhcp_enabled"`
	DNSServers     types.List   `tfsdk:"dns_servers"`
	VlanID         types.String `tfsdk:"vlan_id"`
	VlanIDResolved types.String `tfsdk:"vlan_id_resolved"`
}

type networkIBModel struct {
	Partition     types.String `tfsdk:"partition"`
	Pkey          types.String `tfsdk:"pkey"`
	PkeyAllocated types.String `tfsdk:"pkey_allocated"`
	Index0        types.Bool   `tfsdk:"index0"`
	IPOverIB      types.Bool   `tfsdk:"ip_over_ib"`
}

type networkNVLinkModel struct {
	Partition types.String `tfsdk:"partition"`
}

func (r *networkResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = ResourceName(networkResourceSuffix)
}

// UpgradeState migrates a state written at an earlier schema version. See
// state_history.go.
func (r *networkResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	return stateHistories[networkResourceSuffix].upgraders()
}

// Schema declares the resource.
//
// # Why the fabric kind is a block and not a string
//
// `Network` is a discriminated union on `type`, with three branches that share
// almost nothing: an EVPN network carries MTU, DHCP, DNS and a VLAN, an
// InfiniBand network carries a partition and a PKey, an NVLink network carries
// a partition and nothing else. Flattening every branch's fields alongside a
// `type` string would make a wrong combination a runtime 422 instead of a
// plan-time error.
//
// So the branch is the block, `type` is derived from which block is present,
// and the user never writes the discriminator. A configuration that names two
// fabrics, or none, fails validation before a request is made.
func (r *networkResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Version: stateHistories[networkResourceSuffix].version(),
		MarkdownDescription: "A tenant fabric network: an Ethernet (EVPN), InfiniBand or NVLink " +
			"segment that subnets are allocated inside.\n\n" +
			"Exactly one of `evpn`, `infiniband` or `nvlink` must be set, and it is the one that " +
			"decides the network's `type`. Everything except `display_name`, `description` and " +
			"`tags` is immutable: the API offers no way to change a fabric in place, so a change " +
			"anywhere else replaces the network.\n\n" +
			"Networks materialized by a cluster are not managed here. They carry a `cluster_id`, " +
			"and the tenant API answers `409` to any write against them.\n\n" +
			"Requires `region` and `project` on the provider.",
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{
				Create: true, Update: true, Delete: true,
			}),
		},
		Attributes: map[string]schema.Attribute{
			"region":  regionAttribute(),
			"project": projectAttribute(),
			// Optional+Computed: the provider generates a name when one is
			// omitted, and the name is immutable, so there is no drift to lose.
			"id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Network name, and the id this resource is imported by. " +
					"Omit it and the provider generates one, `tf-` and 16 hex digits, so that a " +
					"create whose answer is lost can be read back rather than sent twice. Cannot be " +
					"changed after creation.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
				Validators: idValidators(),
			},
			"display_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Human-friendly name. One of three mutable fields.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form text describing it, for people to read.",
			},
			"tags": schema.MapAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "User-defined labels. The platform keeps its own `systemTags` " +
					"separately and they are not exposed here, so nothing the platform writes can " +
					"collide with a tag set in configuration.",
				Validators: []validator.Map{reservedTagKey{}},
			},
			tagsAllAttribute: tagsAllSchemaAttribute(""),

			"evpn": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "An Ethernet fabric network. Sets `type` to `evpn`.",
				PlanModifiers: []planmodifier.Object{objectplanmodifier.RequiresReplaceIf(evpnRequiresReplace,
					"Replaces the network unless the only difference is a `vlan_id` the allocated id satisfies.",
					"Replaces the network unless the only difference is a `vlan_id` the allocated id satisfies.")},
				Attributes: map[string]schema.Attribute{
					"service_type": schema.StringAttribute{
						Required:            true,
						MarkdownDescription: "`l2vpn` for an L2 EVPN fabric, `l3vpn` for an L3 one.",
						Validators:          []validator.String{oneOfEnum(k0rdent.NetworkCreateRequestEVPNEvpnTypeValues())},
					},
					// Optional+Computed, and so are the rest of this block: each
					// one is immutable and server-defaultable, so an unset value
					// has to hold what the platform chose rather than a null
					// that proposes replacing the network on the next plan.
					"mtu": schema.Int64Attribute{
						Optional:            true,
						Computed:            true,
						MarkdownDescription: "Maximum transmission unit. The platform applies its own when unset.",
						PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
						Validators: []validator.Int64{int64validator.Between(
							k0rdent.NetworkCreateRequestEVPNMtuMinimum, k0rdent.NetworkCreateRequestEVPNMtuMaximum)},
					},
					// Optional+Computed with a default: immutable either way.
					"dhcp_enabled": schema.BoolAttribute{
						Optional:            true,
						Computed:            true,
						Default:             booldefault.StaticBool(false),
						MarkdownDescription: "Whether DHCP serves addresses on this network.",
						PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
					},
					// A List, not a Set: resolver order is what decides which
					// server is asked first, so it is significant and the
					// platform returns it as written.
					"dns_servers": schema.ListAttribute{
						Optional:    true,
						Computed:    true,
						ElementType: types.StringType,
						MarkdownDescription: "Resolver addresses, in order. A List rather than a Set " +
							"because resolver order is significant.",
						PlanModifiers: []planmodifier.List{listplanmodifier.UseStateForUnknown()},
					},
					// Optional+Computed, and the read keeps what was configured:
					// `"auto"` comes back as a concrete id, so taking the
					// response would propose a replacement every plan.
					"vlan_id": schema.StringAttribute{
						Optional: true,
						Computed: true,
						MarkdownDescription: "VLAN id as a string, e.g. `\"100\"`, or `\"auto\"` to let " +
							"the platform allocate one. See `vlan_id_resolved` for what it chose.",
						PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
						Validators: []validator.String{stringvalidator.RegexMatches(vlanIDPattern,
							"must be \"auto\" or a VLAN number from 1 to 9999 with no leading zero")},
					},
					// Allocated while the network is made, so it's kept once the
					// network has settled, and so is pkey_allocated.
					"vlan_id_resolved": schema.StringAttribute{
						Computed: true,
						MarkdownDescription: "The VLAN id the platform actually assigned. Equal to " +
							"`vlan_id` unless that was `\"auto\"`.",
						PlanModifiers: []planmodifier.String{keepUnlessChanged().onceSettled(networkStates.settle)},
					},
				},
			},

			"infiniband": schema.SingleNestedAttribute{
				Optional:            true,
				MarkdownDescription: "An InfiniBand fabric network. Sets `type` to `infiniband`.",
				PlanModifiers: []planmodifier.Object{recordPkey{}, objectplanmodifier.RequiresReplaceIf(infinibandRequiresReplace,
					"Replaces the network, except for a `pkey` an import could not record.",
					"Replaces the network, except for a `pkey` an import could not record.")},
				Attributes: map[string]schema.Attribute{
					// Optional+Computed: required on `nico` only, and
					// the contract does not gate by backend, so the server is
					// the only thing that knows whether it had to be set.
					"partition": schema.StringAttribute{
						Optional: true,
						Computed: true,
						MarkdownDescription: "Name of an operator-defined InfiniBand Partition in the " +
							"region. Required on the `nico` backend. The contract does " +
							"not gate by backend, so the server decides and this attribute cannot." +
							knownGap("No operation in the contract lists partitions, so the name comes from the operator."),
						PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
						Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
					},
					// Optional+Computed, keeping the configured value on read:
					// the platform may allocate one when it is omitted, and `nico` ignores one
					// that is supplied, so the response is not what was asked for.
					"pkey": schema.StringAttribute{
						Optional: true,
						Computed: true,
						MarkdownDescription: "Partition key in hex, e.g. `0x1234`. Omitted, the platform may " +
							"allocate one. The `nico` backend ignores it and uses the partition name. " +
							"See `pkey_allocated`. An import cannot recover the requested value, so the " +
							"first apply after one records the configured value without replacing the " +
							"network.",
						PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
						Validators: []validator.String{stringvalidator.RegexMatches(pkeyPattern,
							"must be 0x and hex digits, such as 0x1234")},
					},
					"pkey_allocated": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "The PKey the platform reports, whether it was asked for or allocated.",
						PlanModifiers:       []planmodifier.String{keepUnlessChanged().onceSettled(networkStates.settle)},
					},
					// Optional+Computed with a default: immutable, and the
					// platform reports its own value when the request omits one.
					"index0": schema.BoolAttribute{
						Optional:            true,
						Computed:            true,
						Default:             booldefault.StaticBool(false),
						MarkdownDescription: "Store the PKey at index 0.",
						PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
					},
					// See index0.
					"ip_over_ib": schema.BoolAttribute{
						Optional:            true,
						Computed:            true,
						Default:             booldefault.StaticBool(false),
						MarkdownDescription: "Whether the partition takes part in IP-over-InfiniBand multicast.",
						PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
					},
				},
			},

			"nvlink": schema.SingleNestedAttribute{
				Optional: true,
				MarkdownDescription: "An NVLink fabric network. Sets `type` to `nvlink`. Write " +
					"`nvlink = {}` to let the fabric bind the partition itself.",
				PlanModifiers: []planmodifier.Object{objectplanmodifier.RequiresReplace()},
				Attributes: map[string]schema.Attribute{
					// Optional+Computed: omitting it asks the fabric to bind a
					// partition, and the one it bound is what a read returns.
					"partition": schema.StringAttribute{
						Optional: true,
						Computed: true,
						MarkdownDescription: "Name of an operator-defined NVLink Logical Partition in " +
							"the region. Omit it and the fabric binds one." +
							knownGap("No operation in the contract lists partitions, so the name comes from the operator."),
						PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
						Validators:    []validator.String{stringvalidator.LengthAtLeast(1)},
					},
				},
			},

			"type": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "`evpn`, `infiniband` or `nvlink`. Derived from which fabric " +
					"block is set, never written in configuration.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown(), networkTypeFromFabric{}},
			},
			"state": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Lifecycle state as last read from the platform.",
			},
			"self_link": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Platform-defined reference to this network.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			// Both are fixed when the network is made, and a network that
			// carries either can't be written here at all.
			"vpc": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The fabric VPC this network attaches to. Reported only for " +
					"cluster-provisioned networks.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"cluster_id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "UID of the cluster that provisioned this network. Present only " +
					"on networks the cluster lifecycle owns, which cannot be written through this " +
					"resource. A value here on an imported network means the import was a mistake.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// ValidateConfig enforces exactly one fabric block.
//
// Hand-written rather than three `objectvalidator.ExactlyOneOf` declarations:
// those produce three diagnostics naming two blocks each, this produces one
// naming all three and the resource that has to be replaced to change it.
func (r *networkResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	// A fabric from a variable or a conditional is unknown under
	// `terraform validate`, so the count waits for it unless two are already set.
	var set []string
	unknown := false
	for _, f := range networkFabrics {
		name := f.block
		var fabric types.Object
		resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root(name), &fabric)...)
		if resp.Diagnostics.HasError() {
			return
		}
		switch {
		case fabric.IsUnknown():
			unknown = true
		case !fabric.IsNull():
			set = append(set, name)
		}
	}

	switch {
	case len(set) == 1, unknown && len(set) < 2:
		return
	case len(set) == 0:
		// Local: configuration validation, no request made.
		resp.Diagnostics.AddError(
			"A network needs a fabric",
			"Set exactly one of `evpn`, `infiniband` or `nvlink`. The one you set decides the "+
				"network's `type`, which is why there is no `type` to write.",
		)
	default:
		// Local: configuration validation, no request made.
		resp.Diagnostics.AddError(
			"A network has one fabric, not several",
			fmt.Sprintf("`%s` are all set. A network is one of the three, and the platform has no "+
				"request body that carries two.", joinWithAnd(set)),
		)
	}
}

// networkFabrics pairs each fabric block with the `type` it sends, which is
// the discriminator the platform answers with.
var networkFabrics = []struct{ block, kind string }{
	{"evpn", string(k0rdent.NetworkEVPNTypeEvpn)},
	{"infiniband", string(k0rdent.NetworkInfiniBandTypeInfiniband)},
	{"nvlink", string(k0rdent.NetworkNVLinkTypeNvlink)},
}

// networkTypeFromFabric plans `type` from the one fabric block the plan sets,
// so a create shows it rather than "known after apply". The create sends that
// block's discriminator and the read takes `type` from the one the platform
// answers with, so the two can't differ. A block the plan doesn't know yet,
// or two set at once, which ValidateConfig refuses, leaves it as it was.
type networkTypeFromFabric struct{}

func (networkTypeFromFabric) Description(context.Context) string {
	return "Planned from which of `evpn`, `infiniband` or `nvlink` is set."
}

func (m networkTypeFromFabric) MarkdownDescription(ctx context.Context) string {
	return m.Description(ctx)
}

func (networkTypeFromFabric) PlanModifyString(ctx context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	kind := ""
	for _, f := range networkFabrics {
		var fabric types.Object
		resp.Diagnostics.Append(req.Plan.GetAttribute(ctx, path.Root(f.block), &fabric)...)
		if resp.Diagnostics.HasError() || fabric.IsUnknown() {
			return
		}
		if fabric.IsNull() {
			continue
		}
		if kind != "" {
			return
		}
		kind = f.kind
	}
	if kind != "" {
		resp.PlanValue = types.StringValue(kind)
	}
}

// evpnRequiresReplace replaces the network when the block changes, except that
// `vlan_id = "auto"` is satisfied by whatever id the platform allocated, and a
// configured id by the same id reached through `auto`. An import records the
// allocated id, so without this an imported `auto` network plans a replacement.
func evpnRequiresReplace(ctx context.Context, req planmodifier.ObjectRequest, resp *objectplanmodifier.RequiresReplaceIfFuncResponse) {
	if fabricChanged(req) {
		resp.RequiresReplace = true
		return
	}
	var plan, state networkEVPNModel
	resp.Diagnostics.Append(req.PlanValue.As(ctx, &plan, basetypes.ObjectAsOptions{})...)
	resp.Diagnostics.Append(req.StateValue.As(ctx, &state, basetypes.ObjectAsOptions{})...)
	if resp.Diagnostics.HasError() {
		return
	}
	if v := plan.VlanID; !v.IsUnknown() && (v.ValueString() == "auto" || v.Equal(state.VlanIDResolved)) {
		plan.VlanID = state.VlanID
	}
	resp.RequiresReplace = !sameObject(ctx, plan, req, &resp.Diagnostics)
}

// infinibandRequiresReplace replaces the network when the block changes, except
// for a `pkey` state does not know. An import records none, because NICo ignores
// a requested PKey and the response cannot say what was asked for, so the first
// apply takes it from configuration. After that a change replaces as usual.
func infinibandRequiresReplace(ctx context.Context, req planmodifier.ObjectRequest, resp *objectplanmodifier.RequiresReplaceIfFuncResponse) {
	if fabricChanged(req) {
		resp.RequiresReplace = true
		return
	}
	var plan, state networkIBModel
	resp.Diagnostics.Append(req.PlanValue.As(ctx, &plan, basetypes.ObjectAsOptions{})...)
	resp.Diagnostics.Append(req.StateValue.As(ctx, &state, basetypes.ObjectAsOptions{})...)
	if resp.Diagnostics.HasError() {
		return
	}
	if state.Pkey.IsNull() && !plan.Pkey.IsNull() && !plan.Pkey.IsUnknown() {
		if !plan.Pkey.Equal(state.PkeyAllocated) {
			resp.Diagnostics.AddAttributeWarning(req.Path.AtName("pkey"),
				"Recording the configured `pkey` without replacing the network",
				fmt.Sprintf("The configuration says %s and the platform reports %s. The `nico` backend "+
					"ignores a requested PKey, so the provider cannot tell that from a network created with a "+
					"different one. If it really differs, replace the network with "+
					"`terraform apply -replace`.", plan.Pkey, state.PkeyAllocated))
		}
		plan.Pkey = state.Pkey
	}
	resp.RequiresReplace = !sameObject(ctx, plan, req, &resp.Diagnostics)
}

// recordPkey fills a `pkey` an import left null with the one the platform
// reports, when configuration does not set one. The first apply then leaves
// state knowing it, and a later change to `pkey` replaces as usual.
type recordPkey struct{}

func (recordPkey) Description(context.Context) string {
	return "Records the reported PKey after an import when configuration sets none."
}

func (m recordPkey) MarkdownDescription(ctx context.Context) string { return m.Description(ctx) }

func (recordPkey) PlanModifyObject(ctx context.Context, req planmodifier.ObjectRequest, resp *planmodifier.ObjectResponse) {
	if req.StateValue.IsNull() || req.PlanValue.IsNull() || req.PlanValue.IsUnknown() || req.ConfigValue.IsNull() {
		return
	}
	var plan, state, config networkIBModel
	resp.Diagnostics.Append(req.PlanValue.As(ctx, &plan, basetypes.ObjectAsOptions{})...)
	resp.Diagnostics.Append(req.StateValue.As(ctx, &state, basetypes.ObjectAsOptions{})...)
	resp.Diagnostics.Append(req.ConfigValue.As(ctx, &config, basetypes.ObjectAsOptions{})...)
	if resp.Diagnostics.HasError() || !state.Pkey.IsNull() || !config.Pkey.IsNull() {
		return
	}
	plan.Pkey = state.PkeyAllocated
	obj, d := types.ObjectValueFrom(ctx, req.PlanValue.AttributeTypes(ctx), plan)
	resp.Diagnostics.Append(d...)
	resp.PlanValue = obj
}

// fabricChanged reports a switch to or from this fabric: the block is null on
// one side, so there is nothing to compare.
func fabricChanged(req planmodifier.ObjectRequest) bool {
	return req.PlanValue.IsNull() || req.StateValue.IsNull() || req.PlanValue.IsUnknown()
}

// sameObject compares a planned block with the prior one. An attribute the
// configuration leaves unset is still unknown here, because the block's own
// modifiers run before its attributes' UseStateForUnknown, so it counts as
// unchanged. An unknown the configuration set is a real change.
func sameObject(ctx context.Context, model any, req planmodifier.ObjectRequest, diags *diag.Diagnostics) bool {
	got, d := types.ObjectValueFrom(ctx, req.StateValue.AttributeTypes(ctx), model)
	diags.Append(d...)
	if diags.HasError() {
		return false
	}
	prior, config := req.StateValue.Attributes(), req.ConfigValue.Attributes()
	for name, v := range got.Attributes() {
		if v.IsUnknown() && config[name] != nil && config[name].IsNull() {
			continue
		}
		if !v.Equal(prior[name]) {
			return false
		}
	}
	return true
}

func (r *networkResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (r *networkResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	modifyScopePlan(ctx, r.meta, req, resp)
	planTagsAll(ctx, r.meta, req, resp)
	refusePlatformOwned(ctx, "cluster_id", "", req, resp)
	if resp.Diagnostics.HasError() {
		return
	}

	// It compares resp.Plan, not req.Plan: a change to the provider's
	// default_tags is in tags_all alone, and it's a PATCH like any other tag
	// change.
	planResume(ctx, req, resp)
	planSettledState(ctx, networkStates.settle, req, resp)
}

func (r *networkResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan networkModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, plan.Region, plan.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, plan.Timeouts.Create, networkStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}

	key, err := newCreateKey(plan.ID, true)
	if err != nil {
		addCreateKeyError(&resp.Diagnostics, "network", err)
		return
	}
	body, diags := networkCreateRequest(ctx, plan, key)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	create := convergingCreate{
		key: key, kind: "network", what: "create the network",
		resourceName: ResourceName(networkResourceSuffix),
		importID:     scopedImportID(target),
		send: func(ctx context.Context) createAnswer {
			created, err := r.meta.Client.CreateNetworkWithResponse(ctx, target.region, target.project, body)
			if err != nil {
				return createAnswer{err: err}
			}
			a := createAnswer{resp: created.HTTPResponse, body: created.Body}
			if created.JSON201 != nil {
				// A 201 whose union does not decode still made the network
				// it was sent, so its id is the one in the request.
				if view, err := networkViewOf(*created.JSON201); err == nil {
					a.id = view.id
				}
			}
			return a
		},
		lookup: func(ctx context.Context, id string) createLookup {
			got, err := r.meta.Client.GetNetworkWithResponse(ctx, target.region, target.project, id)
			if err != nil {
				return createLookup{err: err}
			}
			return lookupFrom(got.HTTPResponse, got.Body, got.JSON200 != nil, func() string {
				view, err := networkViewOf(*got.JSON200)
				if err != nil {
					return ""
				}
				return createTokenOf(view.tags)
			})
		},
	}
	created, found := createFinish[networkModel]{
		operation: "createNetwork", target: target,
		wait: func(ctx context.Context, id string) error {
			return r.waitForSettled(ctx, target, id, "the network to finish provisioning")
		},
		read: func(ctx context.Context, id string) (networkModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, id, plan)
		},
	}.run(ctx, create, req.Plan, &resp.State, &resp.Diagnostics)
	if found {
		resp.Diagnostics.Append(recordUID(ctx, resp.Private, created.uid)...)
	}
}

func (r *networkResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State, &req.State)
	var prior networkModel
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
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	if !refreshedOwnObject(ctx, req.Private, resp, "network", prior.ID.ValueString(), state.uid) {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update covers the three mutable fields. A fabric change replaces the network,
// except for the equivalent values its replace rules let through, which only
// need writing to state.
func (r *networkResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan, state networkModel
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
	if (resumable[networkModel]{
		kind: "network", resourceName: ResourceName(networkResourceSuffix),
		timeout: plan.Timeouts.Create, fallback: networkStates.timeout(r.meta.Wait),
		wait: func(ctx context.Context) error {
			return r.waitForSettled(ctx, target, id, "the network to finish provisioning")
		},
		read: func(ctx context.Context) (networkModel, bool, diag.Diagnostics) { return r.read(ctx, target, id, plan) },
	}).resume(ctx, req, resp) {
		return
	}
	ctx, cancel := withTimeout(ctx, plan.Timeouts.Update, r.meta.Wait.Timeout, &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}

	// An adopted fabric value or a new `timeouts` block reaches here with the
	// metadata unchanged, and needs no request.
	md, changed, diags := plan.metadata().patch(ctx, state.metadata())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if changed {
		updated, err := r.meta.Client.UpdateNetworkWithResponse(ctx, target.region, target.project, id,
			k0rdent.NetworkUpdateRequest{
				DisplayName: md.DisplayName,
				Description: md.Description,
				Tags:        md.Tags,
			})
		if err != nil || updated.JSON200 == nil {
			var respBody []byte
			var httpResp *http.Response
			if updated != nil {
				respBody, httpResp = updated.Body, updated.HTTPResponse
			}
			addAPIError(&resp.Diagnostics, "update the network", httpResp, respBody, err)
			return
		}

		// A metadata change settles without a fabric operation, but `updating`
		// is a declared state on all three branches, so the wait is not skipped
		// on the assumption that it cannot happen.
		if err := r.waitForSettled(ctx, target, id, "the network to settle"); err != nil {
			addWaitError(&resp.Diagnostics, "The network did not settle after the change", err)
			return
		}
	}

	next, found, diags, err := settledRead(ctx, plan.State, networkStates.settle,
		func(ctx context.Context) (networkModel, bool, diag.Diagnostics) { return r.read(ctx, target, id, plan) },
		func(m networkModel) types.String { return m.State },
		func(ctx context.Context) error { return r.waitForSettled(ctx, target, id, "the network to settle") })
	resp.Diagnostics.Append(diags...)
	if err != nil {
		addWaitError(&resp.Diagnostics, "The network did not settle after the change", err)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		// Local: the provider's conclusion from two calls, as in createFinish.
		resp.Diagnostics.AddError(
			"The network disappeared during the update",
			fmt.Sprintf("Reading %q after the change answered 404.", id),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

func (r *networkResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state networkModel
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
	ctx, cancel := withTimeout(ctx, state.Timeouts.Delete, networkStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	send := func(ctx context.Context) (*http.Response, []byte, error) {
		deleted, err := r.meta.Client.DeleteNetworkWithResponse(ctx, target.region, target.project, id)
		if err != nil {
			return nil, nil, err
		}
		return deleted.HTTPResponse, deleted.Body, nil
	}
	networkStates.destroy(ctx, r.meta.Wait, send, r.reader(target, id), &resp.Diagnostics)
}

func (r *networkResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importScoped(ctx, r.meta, importForm{
		resource: ResourceName(networkResourceSuffix),
		key:      "id",
	}, req, resp)
}

// IdentitySchema is what an import block's `identity` takes.
func (r *networkResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = scopedIdentitySchema("id", "")
}

func (r *networkResource) read(ctx context.Context, target clusterTarget, id string, prior networkModel) (networkModel, bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	got, err := r.meta.Client.GetNetworkWithResponse(ctx, target.region, target.project, id)
	if err != nil {
		addAPIError(&diags, "read the network", nil, nil, err)
		return networkModel{}, false, diags
	}
	if isNotFound(got.HTTPResponse, got.Body) {
		return networkModel{}, false, diags
	}
	if got.JSON200 == nil {
		addAPIError(&diags, "read the network", got.HTTPResponse, got.Body, nil)
		return networkModel{}, false, diags
	}

	out, d := networkStateFrom(ctx, *got.JSON200, prior, r.meta.defaultTags())
	diags.Append(d...)
	return out, true, diags
}

func (r *networkResource) waitForSettled(ctx context.Context, target clusterTarget, id, what string) error {
	return networkStates.awaitSettled(ctx, r.meta.Wait, what, r.reader(target, id))
}

// reader reads the network for a wait, as the part every branch shares.
func (r *networkResource) reader(target clusterTarget, id string) stateRead[networkView] {
	return func(ctx context.Context) (*networkView, *http.Response, []byte, error) {
		got, err := r.meta.Client.GetNetworkWithResponse(ctx, target.region, target.project, id)
		if err != nil {
			return nil, nil, nil, err
		}
		if got.JSON200 == nil {
			return nil, got.HTTPResponse, got.Body, nil
		}
		view, err := networkViewOf(*got.JSON200)
		if err != nil {
			return nil, nil, nil, err
		}
		return &view, got.HTTPResponse, got.Body, nil
	}
}

// networkStates is what a network's waits make of each state it reports.
var networkStates = stateKind[networkView]{
	name:     "network",
	resource: networkResourceSuffix,
	// The contract's 409s on a network delete are a read-only network and
	// one with subnets. Neither is a delete running, but the network reads
	// `deleting` while one is, and only then is a 409 waited out.
	deleting: string(k0rdent.NetworkEVPNStateDeleting),
	state:    func(v networkView) (string, bool) { return v.state, v.state != "" },
	detail: func(v networkView) string {
		return stateDetail(v.stateMessage, reasonString(v.stateReason))
	},
	settle: stateTable{
		missing: stateRule{verdict: settled},
		states: map[string]stateRule{
			string(k0rdent.NetworkEVPNStateActive):   {verdict: settled},
			string(k0rdent.NetworkEVPNStateCreating): {verdict: keepWaiting},
			string(k0rdent.NetworkEVPNStateUpdating): {verdict: keepWaiting},
			string(k0rdent.NetworkEVPNStateDeleting): {verdict: keepWaiting},
			string(k0rdent.NetworkEVPNStateFailed):   {verdict: failsTerminally, detail: true},
		},
	},
	// A network is the exception: the contract does not promise it reads
	// `deleting` as soon as the delete is accepted, so its prior state is
	// tolerated for a while.
	delete: stateTable{
		missing: stateRule{verdict: keepWaiting},
		states: map[string]stateRule{
			string(k0rdent.NetworkEVPNStateDeleting): {verdict: keepWaiting},
			string(k0rdent.NetworkEVPNStateFailed):   {verdict: failsProvisionally, detail: true},
			string(k0rdent.NetworkEVPNStateActive):   {verdict: failsProvisionally, says: afterDelete},
			string(k0rdent.NetworkEVPNStateCreating): {verdict: failsProvisionally, says: afterDelete},
			string(k0rdent.NetworkEVPNStateUpdating): {verdict: failsProvisionally, says: afterDelete},
		},
	},
}

// networkView is the part of a Network that does not depend on which branch of
// the union it is. Reading it once keeps every caller from repeating the
// three-way switch.
type networkView struct {
	kind         string
	id           string
	displayName  *string
	description  *string
	tags         *map[string]string
	selfLink     *string
	uid          string
	vpc          *string
	clusterID    *string
	state        string
	stateMessage *string
	stateReason  *k0rdent.StateReason

	evpn *k0rdent.NetworkEVPN
	ib   *k0rdent.NetworkInfiniBand
	nv   *k0rdent.NetworkNVLink
}

// networkKey is a network's id for listByOffset, or "" for one whose type this
// provider can't read, which the caller then reports.
func networkKey(n k0rdent.Network) string {
	view, err := networkViewOf(n)
	if err != nil {
		return ""
	}
	return view.id
}

func networkViewOf(n k0rdent.Network) (networkView, error) {
	kind, err := n.Discriminator()
	if err != nil {
		return networkView{}, fmt.Errorf("the network's `type` could not be read: %w", err)
	}

	switch kind {
	case string(k0rdent.NetworkEVPNTypeEvpn):
		v, err := n.AsNetworkEVPN()
		if err != nil {
			return networkView{}, err
		}
		return networkView{
			kind: kind, id: deref(v.Id), displayName: v.DisplayName, description: v.Description,
			tags: v.Tags, selfLink: v.SelfLink, uid: uidOf(v.Uid), vpc: v.Vpc, clusterID: v.ClusterId,
			state: stateString(v.State), stateMessage: v.StateMessage, stateReason: v.StateReason,
			evpn: &v,
		}, nil
	case string(k0rdent.NetworkInfiniBandTypeInfiniband):
		v, err := n.AsNetworkInfiniBand()
		if err != nil {
			return networkView{}, err
		}
		return networkView{
			kind: kind, id: deref(v.Id), displayName: v.DisplayName, description: v.Description,
			tags: v.Tags, selfLink: v.SelfLink, uid: uidOf(v.Uid), vpc: v.Vpc, clusterID: v.ClusterId,
			state: stateString(v.State), stateMessage: v.StateMessage, stateReason: v.StateReason,
			ib: &v,
		}, nil
	case string(k0rdent.NetworkNVLinkTypeNvlink):
		v, err := n.AsNetworkNVLink()
		if err != nil {
			return networkView{}, err
		}
		return networkView{
			kind: kind, id: deref(v.Id), displayName: v.DisplayName, description: v.Description,
			tags: v.Tags, selfLink: v.SelfLink, uid: uidOf(v.Uid), vpc: v.Vpc, clusterID: v.ClusterId,
			state: stateString(v.State), stateMessage: v.StateMessage, stateReason: v.StateReason,
			nv: &v,
		}, nil
	default:
		// The same reasoning as unknownStateError: a fourth branch upstream is
		// a newer API, and guessing which of the three it resembles would put
		// invented values in state.
		return networkView{}, fmt.Errorf(
			"the platform reports a network of type %q, which this provider does not recognise. "+
				"Either the landscape is running a newer API than this provider was built against, "+
				"or the value is a typo upstream", kind)
	}
}

// networkCreateRequest builds the branch of the union the configuration chose.
// ValidateConfig has already established that exactly one is set.
func networkCreateRequest(ctx context.Context, plan networkModel, key createKey) (k0rdent.NetworkCreateRequest, diag.Diagnostics) {
	var (
		body  k0rdent.NetworkCreateRequest
		diags diag.Diagnostics
	)

	md, d := plan.metadata().create(ctx)
	diags.Append(d...)
	if diags.HasError() {
		return body, diags
	}
	tags := key.tagged(md.Tags)

	switch {
	case plan.EVPN != nil:
		req := k0rdent.NetworkCreateRequestEVPN{
			Id:          &key.id,
			DisplayName: md.DisplayName,
			Description: md.Description,
			Tags:        tags,
			Type:        k0rdent.NetworkCreateRequestEVPNTypeEvpn,
			DhcpEnabled: optionalBool(plan.EVPN.DHCPEnabled),
		}
		req.Evpn.Type = k0rdent.NetworkCreateRequestEVPNEvpnType(plan.EVPN.ServiceType.ValueString())
		if mtu := optionalInt(plan.EVPN.MTU); mtu != nil {
			req.Mtu = mtu
		}
		if servers, d := optionalStringList(ctx, plan.EVPN.DNSServers); d.HasError() {
			diags.Append(d...)
			return body, diags
		} else if servers != nil {
			req.DnsServers = servers
		}
		if vlan := optionalString(plan.EVPN.VlanID); vlan != nil {
			req.Vlan = zeroPtr(req.Vlan)
			req.Vlan.Id = vlan
		}
		if err := body.FromNetworkCreateRequestEVPN(req); err != nil {
			// Local: the provider could not encode its own request body, no call made.
			diags.AddError("The EVPN network request could not be encoded", err.Error())
		}

	case plan.InfiniBand != nil:
		req := k0rdent.NetworkCreateRequestInfiniBand{
			Id:          &key.id,
			DisplayName: md.DisplayName,
			Description: md.Description,
			Tags:        tags,
			Type:        k0rdent.NetworkCreateRequestInfiniBandTypeInfiniband,
		}
		req.Infiniband.Partition = optionalString(plan.InfiniBand.Partition)
		req.Infiniband.Pkey = optionalString(plan.InfiniBand.Pkey)
		req.Infiniband.Index0 = optionalBool(plan.InfiniBand.Index0)
		req.Infiniband.IpOverIb = optionalBool(plan.InfiniBand.IPOverIB)
		if err := body.FromNetworkCreateRequestInfiniBand(req); err != nil {
			// Local: the provider could not encode its own request body, no call made.
			diags.AddError("The InfiniBand network request could not be encoded", err.Error())
		}

	case plan.NVLink != nil:
		req := k0rdent.NetworkCreateRequestNVLink{
			Id:          &key.id,
			DisplayName: md.DisplayName,
			Description: md.Description,
			Tags:        tags,
			Type:        k0rdent.NetworkCreateRequestNVLinkTypeNvlink,
		}
		// `nvlink = {}` and no partition are the same request: the block is
		// omitted so the fabric binds one, which is what the contract asks for.
		if partition := optionalString(plan.NVLink.Partition); partition != nil {
			req.Nvlink = zeroPtr(req.Nvlink)
			req.Nvlink.Partition = partition
		}
		if err := body.FromNetworkCreateRequestNVLink(req); err != nil {
			// Local: the provider could not encode its own request body, no call made.
			diags.AddError("The NVLink network request could not be encoded", err.Error())
		}

	default:
		// Local: unreachable given ValidateConfig, and said out loud rather
		// than silently sending an empty union.
		diags.AddError(
			"A network reached create with no fabric",
			"This is always a bug in the provider: ValidateConfig should have rejected it.",
		)
	}

	return body, diags
}

// networkStateFrom maps a Network onto the Terraform model.
func networkStateFrom(ctx context.Context, n k0rdent.Network, prior networkModel, defaults defaultTags) (networkModel, diag.Diagnostics) {
	var diags diag.Diagnostics

	view, err := networkViewOf(n)
	if err != nil {
		// Local: the provider could not decode a body it received.
		diags.AddError("The network could not be read", redactDiagnosticText(err.Error()))
		return networkModel{}, diags
	}
	tags, d := tagAnswerOf(view.tags).forResource(prior.Tags)
	diags.Append(d...)
	md := prior.metadata().read(metadataAnswer{DisplayName: view.displayName, Description: view.description, Tags: tags, Defaults: defaults})

	out := networkModel{

		ID:          types.StringValue(view.id),
		Region:      prior.Region,
		Project:     prior.Project,
		Timeouts:    prior.Timeouts,
		DisplayName: md.DisplayName,
		Description: md.Description,
		Tags:        md.Tags,
		TagsAll:     md.TagsAll,
		SelfLink:    stringOrNull(view.selfLink),
		uid:         view.uid,
		VPC:         stringOrNull(view.vpc),
		ClusterID:   stringOrNull(view.clusterID),
		Type:        types.StringValue(view.kind),
		State:       types.StringNull(),
	}
	if view.state != "" {
		out.State = types.StringValue(view.state)
	}

	switch {
	case view.evpn != nil:
		e := &networkEVPNModel{
			ServiceType:    types.StringValue(string(view.evpn.Evpn.Type)),
			MTU:            int64OrNull(view.evpn.Mtu),
			DHCPEnabled:    boolOrNull(view.evpn.DhcpEnabled),
			VlanIDResolved: types.StringNull(),
		}
		if view.evpn.Vlan != nil {
			e.VlanIDResolved = stringOrNull(view.evpn.Vlan.Id)
		}
		e.VlanID = e.VlanIDResolved
		if prior.EVPN != nil {
			e.VlanID = keepConfigured(prior.EVPN.VlanID, e.VlanIDResolved.ValueStringPointer())
		}
		servers, d := stringListOrNull(ctx, view.evpn.DnsServers)
		diags.Append(d...)
		e.DNSServers = servers
		out.EVPN = e

	case view.ib != nil:
		i := &networkIBModel{
			Partition:     stringOrNull(view.ib.Infiniband.Partition),
			PkeyAllocated: stringOrNull(view.ib.Infiniband.Pkey),
			Index0:        boolOrNull(view.ib.Infiniband.Index0),
			IPOverIB:      boolOrNull(view.ib.Infiniband.IpOverIb),
		}
		// NICo ignores a requested PKey and answers with the partition's own,
		// so a configured value has to survive the read or every plan against a
		// NICo landscape proposes replacing the network. An import has nothing
		// to keep and records none: see infinibandRequiresReplace.
		i.Pkey = types.StringNull()
		if prior.InfiniBand != nil {
			i.Pkey = prior.InfiniBand.Pkey
			if i.Pkey.IsUnknown() {
				i.Pkey = i.PkeyAllocated
			}
		}
		out.InfiniBand = i

	case view.nv != nil:
		nv := &networkNVLinkModel{Partition: types.StringNull()}
		if view.nv.Nvlink != nil {
			nv.Partition = stringOrNull(view.nv.Nvlink.Partition)
		}
		out.NVLink = nv
	}

	return out, diags
}
