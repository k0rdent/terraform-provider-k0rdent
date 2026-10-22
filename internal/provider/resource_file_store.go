package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/mapvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-framework/types/basetypes"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ resource.Resource                   = (*fileStoreResource)(nil)
	_ resource.ResourceWithConfigure      = (*fileStoreResource)(nil)
	_ resource.ResourceWithImportState    = (*fileStoreResource)(nil)
	_ resource.ResourceWithIdentity       = (*fileStoreResource)(nil)
	_ resource.ResourceWithModifyPlan     = (*fileStoreResource)(nil)
	_ resource.ResourceWithValidateConfig = (*fileStoreResource)(nil)
	_ resource.ResourceWithUpgradeState   = (*fileStoreResource)(nil)
)

const fileStoreResourceSuffix = "file_store"

func newFileStoreResource() resource.Resource {
	return guardReplacements(&fileStoreResource{}, fileStoreTenantState)
}

type fileStoreResource struct {
	meta *Meta
}

type fileStoreModel struct {
	ID          types.String `tfsdk:"id"`
	Region      types.String `tfsdk:"region"`
	Project     types.String `tfsdk:"project"`
	ShareType   types.String `tfsdk:"share_type"`
	CapacityGiB types.Int64  `tfsdk:"capacity_gib"`
	DisplayName types.String `tfsdk:"display_name"`
	Description types.String `tfsdk:"description"`

	// Framework collections rather than Go ones: a Go map or slice cannot hold
	// unknown, and `consumers` is unknown whenever configuration builds a key
	// from another resource's attributes. Read it through consumersOf.
	Consumers       types.Map      `tfsdk:"consumers"`
	ConsumerStatus  types.Map      `tfsdk:"consumer_status"`
	ExportLocations types.List     `tfsdk:"export_locations"`
	PreferredExport types.Object   `tfsdk:"preferred_export_location"`
	State           types.String   `tfsdk:"state"`
	Etag            types.String   `tfsdk:"etag"`
	SelfLink        types.String   `tfsdk:"self_link"`
	Timeouts        timeouts.Value `tfsdk:"timeouts"`
}

func (m fileStoreModel) metadata() tenantMetadata {
	return tenantMetadata{
		DisplayName: m.DisplayName,
		Description: m.Description,
	}
}

type fileStoreConsumerModel struct {
	Type        types.String                   `tfsdk:"type"`
	AccessLevel types.String                   `tfsdk:"access_level"`
	MountPath   types.String                   `tfsdk:"mount_path"`
	Network     *fileStoreConsumerNetworkModel `tfsdk:"network"`
}

type fileStoreConsumerNetworkModel struct {
	Type       types.String `tfsdk:"type"`
	MacAddress types.String `tfsdk:"mac_address"`
	GUID       types.String `tfsdk:"guid"`
}

type fileStoreConsumerStatusModel struct {
	State             types.String `tfsdk:"state"`
	StateReason       types.String `tfsdk:"state_reason"`
	ResolvedAddresses types.Set    `tfsdk:"resolved_addresses"`
}

type fileStoreExportLocationModel struct {
	Path      types.String `tfsdk:"path"`
	Server    types.String `tfsdk:"server"`
	Protocol  types.String `tfsdk:"protocol"`
	Options   types.String `tfsdk:"options"`
	Preferred types.Bool   `tfsdk:"preferred"`
	AdminOnly types.Bool   `tfsdk:"admin_only"`
}

// The element types of the three collections, needed to build a null or a
// populated value.
func fileStoreConsumerType() attr.Type {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"type":         types.StringType,
		"access_level": types.StringType,
		"mount_path":   types.StringType,
		"network": types.ObjectType{AttrTypes: map[string]attr.Type{
			"type":        types.StringType,
			"mac_address": types.StringType,
			"guid":        types.StringType,
		}},
	}}
}

// keyedConsumer is one `consumers` entry with the `ref` it is keyed by.
type keyedConsumer struct {
	ref string
	fileStoreConsumerModel
}

// consumersOf decodes the known entries of `consumers`, in key order. An entry
// that is not yet known, which only happens before apply, is left out, and so
// is a null one, which ValidateConfig refuses.
func consumersOf(ctx context.Context, m types.Map, diags *diag.Diagnostics) []keyedConsumer {
	if m.IsNull() || m.IsUnknown() {
		return nil
	}
	elems := m.Elements()
	refs := make([]string, 0, len(elems))
	for ref := range elems {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	out := make([]keyedConsumer, 0, len(refs))
	for _, ref := range refs {
		obj, ok := elems[ref].(types.Object)
		if !ok || obj.IsNull() || obj.IsUnknown() || obj.Attributes()["network"].IsUnknown() {
			continue
		}
		c := keyedConsumer{ref: ref}
		diags.Append(obj.As(ctx, &c.fileStoreConsumerModel, basetypes.ObjectAsOptions{})...)
		out = append(out, c)
	}
	return out
}

func fileStoreConsumerStatusType() attr.Type {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"state":              types.StringType,
		"state_reason":       types.StringType,
		"resolved_addresses": types.SetType{ElemType: types.StringType},
	}}
}

func preferredExportType() types.ObjectType {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"path":     types.StringType,
		"server":   types.StringType,
		"protocol": types.StringType,
		"options":  types.StringType,
	}}
}

func fileStoreExportLocationType() attr.Type {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"path":       types.StringType,
		"server":     types.StringType,
		"protocol":   types.StringType,
		"options":    types.StringType,
		"preferred":  types.BoolType,
		"admin_only": types.BoolType,
	}}
}

func (r *fileStoreResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = ResourceName(fileStoreResourceSuffix)
}

// UpgradeState migrates a state written at an earlier schema version. See
// state_history.go.
func (r *fileStoreResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	return stateHistories[fileStoreResourceSuffix].upgraders()
}

// Schema declares the resource.
//
// # Why capacity is not RequiresReplace
//
// Capacity is expand-only: the platform answers `409` with
// `CONFLICT_IMMUTABLE` to any value below the current one, because shrinking a
// filesystem that already holds data is not a safe online operation. The
// obvious transcription of "cannot be lowered" is RequiresReplace, and it is
// the wrong one: it turns "I asked for a smaller store" into "Terraform
// destroyed a store holding data and provisioned an empty one". Google's
// `google_sql_database_instance` shipped exactly that on `disk_size` and it is
// the origin of a standing warning in its documentation.
//
// So the shrink is refused in ModifyPlan instead, before the apply, and the
// message names the only safe route: a new store and a migration.
//
// # Why share_type is RequiresReplace and capacity is not
//
// The class genuinely cannot be moved between: the request field exists only so
// that sending it produces a diagnosis rather than a bare rejection. Replacing
// is the only thing Terraform can do about it, and unlike a shrink the user
// asking for a different class is asking for a different store.
func (r *fileStoreResource) Schema(ctx context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Version: stateHistories[fileStoreResourceSuffix].version(),
		MarkdownDescription: "An account-level file store: a shared filesystem provisioned from a " +
			"capability class, exported to the addresses and instance groups named in " +
			"`consumers`.\n\n" +
			"This is storage the platform allocates and the tenant mounts. Storage *inside* a " +
			"cluster is a CSI concern and is driven through the cluster's kubeconfig, not " +
			"here.\n\n" +
			"`capacity_gib` is expand-only and `share_type` is fixed for the life of the store. " +
			"A store that still lists consumers cannot be deleted, so this resource empties them " +
			"first, including any the platform lists that the configuration doesn't, and names " +
			"those in a warning. If the destroy then fails, state records the consumers as detached, " +
			"because they have lost access: running the destroy again carries on, and an apply " +
			"grants them again. See the note on `consumers`.\n\n" +
			"Requires `region` and `project` on the provider.",
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx, timeouts.Opts{
				Create: true, Update: true, Delete: true,
			}),
		},
		Attributes: map[string]schema.Attribute{
			"region":  regionAttribute(),
			"project": projectAttribute(),
			// Optional+Computed, same as every other named object here. The id is
			// the only retry guard the contract offers, with no idempotency key
			// header, so the provider names every create: see create_converge.go.
			"id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Store name, and the id this resource is imported by. " +
					"Omit it and the provider generates one, `tf-` and 16 hex digits. Either way the " +
					"create names the store, so a repeat answers `409` rather than provisioning a " +
					"second one.",
				PlanModifiers: []planmodifier.String{
					stringplanmodifier.RequiresReplace(),
					stringplanmodifier.UseStateForUnknown(),
				},
				Validators: idValidators(),
			},
			"share_type": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Capability class to provision from, as listed by `" +
					ResourceName(shareTypesDataSourceSuffix) + "`. Named by capability, for " +
					"example `general` or `high-speed`, never after a vendor or an array. " +
					"Immutable: moving a store between classes is a data migration, so changing " +
					"this replaces the store.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    idValidators(),
			},
			"capacity_gib": schema.Int64Attribute{
				Required: true,
				MarkdownDescription: "Declared capacity in binary gibibytes. **Expand-only**: " +
					"raising it grows the store, and lowering it is refused at plan time because " +
					"releasing capacity means creating a smaller store and migrating the data.\n\n" +
					"**Not enforced as a quota.** The storage behind this API does not currently " +
					"cap consumption at this figure, so it is a declaration for planning and " +
					"accounting.",
				Validators: []validator.Int64{int64validator.Between(
					k0rdent.FileStoreCreateRequestCapacityGiBMinimum, k0rdent.FileStoreCreateRequestCapacityGiBMaximum)},
			},
			"display_name": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Human-friendly name. Free text, not unique, not used for lookup.",
			},
			"description": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Free-form text describing it, for people to read.",
			},

			// A map keyed by `ref`, the identity the platform enforces as unique
			// and keys `consumerStatus` by. A changed entry plans as that entry
			// changing, and the platform's lack of order cannot cause a diff.
			"consumers": schema.MapNestedAttribute{
				Optional: true,
				MarkdownDescription: "Who may mount the store, keyed by `ref`: the address for an " +
					"`ipAddress` entry, or the instance group's id for an `instanceGroup` entry. This " +
					"is the desired set, replaced wholesale on every change: to attach one, add an " +
					"entry; to detach one, remove it.\n\n" +
					"An empty or absent map is a store mountable by nobody, which is the " +
					"intended default.\n\n" +
					"Entries are resolved before the store is accepted, so an instance group that " +
					"is still provisioning fails the whole create rather than producing a store " +
					"with a partial export policy. Grant progress is in `consumer_status`.\n\n" +
					"An `instanceGroup` entry is resolved to addresses when it is sent, and the " +
					"platform resolves it again by itself when the group's membership moves. When " +
					"that repair is off or does not stick, `consumer_status` reports " +
					"`GROUP_MEMBERSHIP_DRIFTED` and the next plan proposes sending the entry again. " +
					"If the platform refuses that, the rest of the change is applied and plans stop " +
					"proposing the re-send until its report changes. A group reported as " +
					"`GROUP_NO_LONGER_GRANTABLE` is gone or now names a different group, and fails " +
					"the plan until its entry is removed.\n\n" +
					"A key taken from another resource's attribute that is not known until apply " +
					"hides the whole map from the plan. Set the instance group's `id` in " +
					"configuration so a reviewer can see who is granted access.",
				Validators: []validator.Map{mapvalidator.SizeAtMost(k0rdent.FileStoreCreateRequestConsumersMaxItems)},
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						// The default is the one the contract states and the
						// platform applies, so the plan shows the real value.
						"type": schema.StringAttribute{
							Optional: true,
							Computed: true,
							Default:  stringdefault.StaticString(string(k0rdent.ConsumerTypeIpAddress)),
							MarkdownDescription: "`ipAddress` to grant one address directly, or " +
								"`instanceGroup` to grant every member of a group on the interface " +
								"`network` selects. Defaults to `ipAddress`.",
							Validators: []validator.String{oneOfEnum(k0rdent.ConsumerTypeValues())},
						},
						// OneOf holds the contract's enum, `ro` included, so that
						// admitting it later is additive. ValidateConfig refuses
						// `ro` with the reason, since `rw` is the only level
						// granted today.
						"access_level": schema.StringAttribute{
							Optional: true,
							Computed: true,
							Default:  stringdefault.StaticString(string(k0rdent.ConsumerAccessLevelRw)),
							MarkdownDescription: "Mount level for the whole export. Only `rw` can " +
								"be granted. Ask an operator for read-only access.",
							Validators: []validator.String{oneOfEnum(k0rdent.ConsumerAccessLevelValues())},
						},
						"mount_path": schema.StringAttribute{
							Optional: true,
							MarkdownDescription: "Advisory mount point, recorded so configuration " +
								"management on the host has one place to read it from. The " +
								"platform publishes it and never acts on it: it does not mount the " +
								"store, and a host mounted elsewhere is not drift the platform " +
								"reports. Must be a clean absolute POSIX path.",
							Validators: []validator.String{
								stringvalidator.LengthAtMost(k0rdent.ConsumerMountPathMaxLength),
								stringvalidator.RegexMatches(mountPathPattern, "must be an absolute path with no "+
									"empty segment and no control character, such as /mnt/data"),
							},
						},
						"network": schema.SingleNestedAttribute{
							Optional: true,
							MarkdownDescription: "Which of a resolved member's interfaces supplies " +
								"the address to grant. Required on an `instanceGroup` entry and " +
								"refused on an `ipAddress` entry, where `ref` already is the " +
								"address.\n\n" +
								"The operator names the fabric; the platform never infers it, " +
								"because a member machine sits on several at once.",
							Attributes: map[string]schema.Attribute{
								"type": schema.StringAttribute{
									Required:            true,
									MarkdownDescription: enumProse(k0rdent.ConsumerNetworkTypeValues()) + ".",
									Validators:          []validator.String{oneOfEnum(k0rdent.ConsumerNetworkTypeValues())},
								},
								"mac_address": schema.StringAttribute{
									Optional: true,
									MarkdownDescription: "Narrows the selection to one Ethernet " +
										"interface. Only valid with `type = \"ethernet\"`. Read it from " +
										"`network_interfaces.mac_address` on the `" + ResourceName(instancesDataSourceSuffix) +
										"` data source.",
									Validators: []validator.String{stringvalidator.RegexMatches(macAddressPattern,
										"must be six colon-separated pairs of hex digits, such as 0c:42:a1:00:00:01")},
								},
								"guid": schema.StringAttribute{
									Optional: true,
									MarkdownDescription: "Narrows the selection to one InfiniBand " +
										"interface. Only valid with `type = \"infiniband\"`. Read it from " +
										"`network_interfaces.guid` on the `" + ResourceName(instancesDataSourceSuffix) +
										"` data source.",
									Validators: []validator.String{stringvalidator.RegexMatches(guidPattern,
										"must be 0x and 16 hex digits, such as 0x0002c90300a1b2c3")},
								},
							},
						},
					},
				},
			},

			// Keyed by `ref` like `consumers`, of which it is a superset.
			"consumer_status": schema.MapNestedAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.Map{keptGrantStatus{settledBy: fileStoreStates.settle}},
				MarkdownDescription: "Observed state of each grant, keyed by `ref`, as last read. A **superset** of " +
					"`consumers`: an entry being revoked appears here as `deleting` after it has " +
					"left `consumers`.\n\n" +
					"`active` means the grant is enforced on the backing storage, not merely " +
					"recorded, so this is what says whether a host can mount.\n\n" +
					"**A `failed` entry is not necessarily an outage.** " +
					"`GROUP_MEMBERSHIP_DRIFTED` is reported as `failed` while the grant still " +
					"covers the machines it was written against: the group's membership moved and " +
					"the platform's own repair did not happen or did not stick. An entry being " +
					"repaired reads `creating` with no reason. Alert on " +
					"`state == \"failed\" && state_reason != \"GROUP_MEMBERSHIP_DRIFTED\"`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"state": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "`creating`, `active`, `deleting` or `failed`.",
						},
						"state_reason": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "Machine-readable cause, upper-cased. Stable " +
								"enough to branch on.",
						},
						// A Set. The platform normalizes these to the
						// spelling it uses for duplicate detection and declares
						// no order, and two entries that resolve to the same
						// address report it under both, so membership is the
						// only thing an index could have meant.
						"resolved_addresses": schema.SetAttribute{
							Computed:    true,
							ElementType: types.StringType,
							MarkdownDescription: "The addresses this entry actually authored. For " +
								"an `instanceGroup` entry this is the record of which machines the " +
								"grant was derived from. IPv4 only.",
						},
					},
				},
			},

			// Sorted on identity in Read. The platform declares no order, and
			// which location is preferred can move, so neither sets the order.
			// Locations are published as the store becomes active, and after
			// that only a change to who mounts it, or to its size, sets the
			// platform reconciling it, which can republish them. ModifyPlan
			// plans both unknown for a re-send.
			"export_locations": schema.ListNestedAttribute{
				Computed: true,
				PlanModifiers: []planmodifier.List{
					keepUnlessChanged("consumers", "capacity_gib").onceSettled(fileStoreStates.settle),
				},
				MarkdownDescription: "Where the store can be mounted from, sorted by `server` and " +
					"then `path`. To mount, use `preferred_export_location`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"path":      schema.StringAttribute{Computed: true, MarkdownDescription: "Export path on the server."},
						"server":    schema.StringAttribute{Computed: true, MarkdownDescription: "Address of the storage server."},
						"protocol":  schema.StringAttribute{Computed: true, MarkdownDescription: "Protocol the export is served over."},
						"options":   schema.StringAttribute{Computed: true, MarkdownDescription: "Mount options the platform recommends."},
						"preferred": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether this is the location to prefer."},
						"admin_only": schema.BoolAttribute{
							Computed: true,
							MarkdownDescription: "Whether this location is meant for the operator " +
								"rather than for tenant hosts.",
						},
					},
				},
			},

			"preferred_export_location": schema.SingleNestedAttribute{
				Computed: true,
				PlanModifiers: []planmodifier.Object{
					keepUnlessChanged("consumers", "capacity_gib").onceSettled(fileStoreStates.settle),
				},
				MarkdownDescription: "The location a tenant host should mount: the one the platform " +
					"marks preferred and not operator-only. Null until the store is active. The " +
					"mount source is `<server>:<path>`.",
				Attributes: map[string]schema.Attribute{
					"path":     schema.StringAttribute{Computed: true, MarkdownDescription: "Export path on the server."},
					"server":   schema.StringAttribute{Computed: true, MarkdownDescription: "Address of the storage server."},
					"protocol": schema.StringAttribute{Computed: true, MarkdownDescription: "Protocol the export is served over."},
					"options":  schema.StringAttribute{Computed: true, MarkdownDescription: "Mount options the platform recommends."},
				},
			},

			// `state` alone, matching every other resource here. The contract's
			// `stateMessage` and `stateReason` are populated on some transitions
			// and not others with no user write behind them, so storing either
			// turns a refresh into a diff. The detail is not lost: it rides in
			// the diagnostic when a wait fails, which is where someone is
			// actually looking when it matters.
			"state": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Lifecycle state as last read from the platform.",
			},
			// Any write moves it. An update that writes nothing, such as a new
			// `timeouts` block, keeps prior state whole, this included.
			"etag": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The store's current revision. Sent back on every update so " +
					"that a concurrent edit is refused rather than clobbered. A destroy does not " +
					"send it.",
				PlanModifiers: []planmodifier.String{
					keepUnlessChanged("display_name", "description", "capacity_gib", "consumers"),
				},
			},
			"self_link": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Platform-defined reference to this store.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

// ValidateConfig catches the consumer rules the platform answers 422 to.
//
// All four are decidable from the configuration alone, and each one is a
// rejection partway through an approved apply if it is left to the server. The
// interlocking pair, where `network` is required on one entry kind and refused
// on the other, is the reason this is hand-written rather than four independent
// attribute validators: an entry that gets it wrong should produce one
// diagnostic naming the entry, not two naming fields.
func (r *fileStoreResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var config fileStoreModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !config.Consumers.IsNull() && !config.Consumers.IsUnknown() {
		for ref, v := range config.Consumers.Elements() {
			if v.IsNull() {
				// Local: configuration validation, no request made.
				resp.Diagnostics.AddAttributeError(path.Root("consumers").AtMapKey(ref),
					"A consumer entry is null",
					fmt.Sprintf("The consumer %q has no value. The platform has no null grant, so the "+
						"entry would never read back. Leave the key out instead, for example with a "+
						"`for` expression that filters on the condition.", ref))
			}
		}
	}

	for _, c := range consumersOf(ctx, config.Consumers, &resp.Diagnostics) {
		where := path.Root("consumers").AtMapKey(c.ref)
		ref := c.ref
		// Unknown until apply: a ref interpolated from another resource. The
		// server will still check it, and guessing here would reject a valid
		// configuration.
		if c.Type.IsUnknown() {
			continue
		}
		if c.Type.ValueString() != string(k0rdent.ConsumerTypeInstanceGroup) && !grantableIPv4(ref) {
			// Local: configuration validation, no request made.
			resp.Diagnostics.AddAttributeError(where,
				"A consumer address is not an IPv4 host address",
				fmt.Sprintf("%q is not an IPv4 host address. An `ipAddress` entry is keyed by one literal "+
					"IPv4 address that names a host: not a prefix, not an IPv6 spelling, and not "+
					"`0.0.0.0`, `255.255.255.255`, a loopback, multicast or link-local address. Set "+
					"`type = \"instanceGroup\"` if this was meant to name a group.", ref))
		}

		kind := c.Type.ValueString()
		if kind == "" {
			kind = string(k0rdent.ConsumerTypeIpAddress) // the documented default
		}

		if lvl := c.AccessLevel; !lvl.IsNull() && !lvl.IsUnknown() &&
			lvl.ValueString() != string(k0rdent.ConsumerAccessLevelRw) {
			// Local: configuration validation, no request made.
			resp.Diagnostics.AddAttributeError(where,
				"Only read-write access can be granted",
				fmt.Sprintf("The consumer %q asks for `access_level = %q`. Only `rw` can be "+
					"granted. Ask an operator for read-only access.", ref, lvl.ValueString()))
		}

		switch kind {
		case string(k0rdent.ConsumerTypeIpAddress):
			if c.Network != nil {
				// Local: configuration validation, no request made.
				resp.Diagnostics.AddAttributeError(where,
					"An address entry has no interface to select",
					fmt.Sprintf("The consumer %q is an address and also sets `network`. ", ref)+
						"`network` selects which of a resolved member's interfaces supplies the "+
						"address to grant. On an `ipAddress` entry `ref` already is the address, "+
						"so there is nothing left to select. Remove `network`, or set "+
						"`type = \"instanceGroup\"` if this was meant to name a group.")
			}
		case string(k0rdent.ConsumerTypeInstanceGroup):
			if c.Network == nil {
				// Local: configuration validation, no request made.
				resp.Diagnostics.AddAttributeError(where,
					"A group entry has to name a fabric",
					fmt.Sprintf("The consumer %q names an instance group and no `network`. ", ref)+
						"A member machine sits on several fabrics at once and only you know which one "+
						"the store should be mounted over, so the platform refuses an "+
						"`instanceGroup` entry that omits `network` rather than choosing an "+
						"interface on your behalf.")
				continue
			}
			r.validateConsumerNetwork(*c.Network, where, ref, &resp.Diagnostics)
		}
	}
}

// validateConsumerNetwork enforces that only the hardware identifier matching
// the fabric is supplied. An InfiniBand interface has no MAC address, which is
// why the two are distinct fields rather than one overloaded one, and sending
// the wrong one is a 422.
func (r *fileStoreResource) validateConsumerNetwork(n fileStoreConsumerNetworkModel, where path.Path, ref string, diags *diag.Diagnostics) {
	mac := !n.MacAddress.IsNull() && !n.MacAddress.IsUnknown()
	guid := !n.GUID.IsNull() && !n.GUID.IsUnknown()

	if mac && guid {
		// Local: configuration validation, no request made.
		diags.AddAttributeError(where,
			"An interface has one hardware identifier, not two",
			fmt.Sprintf("The consumer %q sets both `mac_address` and `guid`. ", ref)+
				"Supply the one that goes with `type`: "+
				"`mac_address` with `ethernet`, `guid` with `infiniband`.")
		return
	}

	switch n.Type.ValueString() {
	case string(k0rdent.ConsumerNetworkTypeEthernet):
		if guid {
			// Local: configuration validation, no request made.
			diags.AddAttributeError(where,
				"A GUID does not identify an Ethernet interface",
				fmt.Sprintf("The consumer %q sets `guid` with `type = \"ethernet\"`. ", ref)+
					"Use `mac_address` to narrow an Ethernet "+
					"selection, or set `type = \"infiniband\"`.")
		}
	case string(k0rdent.ConsumerNetworkTypeInfiniband):
		if mac {
			// Local: configuration validation, no request made.
			diags.AddAttributeError(where,
				"An InfiniBand interface has no MAC address",
				fmt.Sprintf("The consumer %q sets `mac_address` with `type = \"infiniband\"`. ", ref)+
					"Use `guid` to narrow an "+
					"InfiniBand selection, or set `type = \"ethernet\"`.")
		}
	}
}

func (r *fileStoreResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (r *fileStoreResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	modifyScopePlan(ctx, r.meta, req, resp)
	if resp.Diagnostics.HasError() {
		return
	}

	// A destroy. This is the last point at which anybody is told which hosts
	// lose the store, before a prompt that says only "1 to destroy".
	if req.Plan.Raw.IsNull() && !req.State.Raw.IsNull() {
		var prior fileStoreModel
		// Not worth failing a destroy over. If prior state cannot be read the
		// destroy itself will say so far more clearly than a warning would.
		if diags := req.State.Get(ctx, &prior); !diags.HasError() {
			warnOnDestroyingABoundStore(ctx, prior, &resp.Diagnostics)
		}
		return
	}

	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	planResume(ctx, req, resp)

	var plan, state fileStoreModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// The shrink refusal. Deliberately not RequiresReplace: see Schema. An
	// unknown capacity is left to the server, because a value interpolated from
	// somewhere else cannot be compared here and refusing it would reject a
	// configuration that is about to be fine.
	// A replacement is a new store, so a smaller capacity there is no shrink.
	replacing := len(resp.RequiresReplace) > 0 || !plan.ShareType.Equal(state.ShareType) ||
		(!plan.ID.IsUnknown() && !plan.ID.Equal(state.ID))
	if replacing {
		// A replacement deletes this store too.
		warnOnDestroyingABoundStore(ctx, state, &resp.Diagnostics)
	}
	if !replacing && !plan.CapacityGiB.IsUnknown() && !state.CapacityGiB.IsNull() &&
		plan.CapacityGiB.ValueInt64() < state.CapacityGiB.ValueInt64() {
		// Local: compared against prior state, no request made. The upstream
		// 409 CONFLICT_IMMUTABLE this replaces would have carried a code and a
		// request id; the point is that it is never sent.
		resp.Diagnostics.AddAttributeError(path.Root("capacity_gib"),
			"A file store cannot be shrunk",
			fmt.Sprintf("`capacity_gib` goes from %d to %d. Capacity is expand-only: shrinking a "+
				"filesystem that already holds data is not a safe online operation, so the "+
				"platform refuses it.\n\nReleasing capacity means provisioning a smaller store "+
				"and migrating the data. Terraform will not do that for you, and this resource "+
				"deliberately does not replace the store on a shrink, because that would destroy "+
				"the data it holds.",
				state.CapacityGiB.ValueInt64(), plan.CapacityGiB.ValueInt64()))
		return
	}

	verdicts := judgeGrants(grantReportOf(ctx, state.ConsumerStatus, &resp.Diagnostics),
		configuredRefs(plan.Consumers), readRefused(ctx, req.Private, &resp.Diagnostics))
	resp.Diagnostics.Append(verdicts.planDiagnostics()...)
	if len(verdicts.ungrantable) > 0 {
		return
	}
	drifted := verdicts.resend

	if len(drifted) == 0 && req.Plan.Raw.Equal(req.State.Raw) {
		return
	}
	// The update waits for the store to settle, so one that starts settled
	// ends settled. Last, so that it sees the re-send below as a change. The
	// plan modifiers keep what a change can't move.
	defer planSettledState(ctx, fileStoreStates.settle, req, resp)
	if len(drifted) == 0 {
		return
	}
	// A re-send is a write, and a write moves the revision.
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("etag"), types.StringUnknown())...)
	// The grant reaches only the members the group had when it was written,
	// and sending the entry again is the contract's only way to resolve it to
	// the current ones. An unknown `consumer_status` is what makes the plan an
	// update, and Update sends `consumers` when it sees the same verdict.
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("consumer_status"),
		types.MapUnknown(fileStoreConsumerStatusType()))...)
	// The re-send reconciles the store, which can republish its locations.
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("export_locations"),
		types.ListUnknown(fileStoreExportLocationType()))...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("preferred_export_location"),
		types.ObjectUnknown(preferredExportType().AttrTypes))...)
}

func (r *fileStoreResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan fileStoreModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, plan.Region, plan.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, plan.Timeouts.Create, fileStoreStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}

	// A file store has no tags, so it carries no create token: only a
	// generated id proves a store found after a lost answer is this one.
	key, err := newCreateKey(plan.ID, false)
	if err != nil {
		addCreateKeyError(&resp.Diagnostics, "file store", err)
		return
	}
	body := k0rdent.FileStoreCreateRequest{
		Id:          &key.id,
		ShareType:   plan.ShareType.ValueString(),
		CapacityGiB: plan.CapacityGiB.ValueInt64(),
		Consumers:   consumersRequest(consumersOf(ctx, plan.Consumers, &resp.Diagnostics)),
	}
	md, diags := plan.metadata().create(ctx)
	resp.Diagnostics.Append(diags...)
	body.DisplayName, body.Description = md.DisplayName, md.Description

	// A 504 can follow a committed create, and then names the store in
	// `details[].metadata.fileStoreId`. That store is recorded and waited out,
	// because a retry would answer CONFLICT_DUPLICATE and nothing would own it.
	create := convergingCreate{
		key: key, kind: "file store", what: "create the file store",
		resourceName: ResourceName(fileStoreResourceSuffix),
		importID:     scopedImportID(target),
		send: func(ctx context.Context) createAnswer {
			created, err := r.meta.Client.CreateFileStoreWithResponse(ctx, target.region, target.project, body)
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
			got, err := r.meta.Client.GetFileStoreWithResponse(ctx, target.region, target.project, id)
			if err != nil {
				return createLookup{err: err}
			}
			return lookupFrom(got.HTTPResponse, got.Body, got.JSON200 != nil, func() string { return "" })
		},
		timeoutID: timeoutMetadataID("fileStoreId"),
	}
	// A store is finite physical capacity, so an orphan costs more here than
	// anywhere: createFinish keeps it in state rather than deleting it.
	state, ok := createFinish[fileStoreModel]{
		operation: "createFileStore", target: target,
		wait: func(ctx context.Context, id string) error {
			return r.waitForSettled(ctx, target, id, "the file store to finish provisioning")
		},
		read: func(ctx context.Context, id string) (fileStoreModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, id, plan)
		},
	}.run(ctx, create, req.Plan, &resp.State, &resp.Diagnostics)
	if !ok {
		return
	}
	warnOnConsumerFailures(ctx, state, &resp.Diagnostics)
}

func (r *fileStoreResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State, &req.State)
	var prior fileStoreModel
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
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// Update covers the four updatable fields. `share_type` and `id` carry
// RequiresReplace, so neither ever reaches here, and `share_type` is never put
// in the body: it is present on the update type only so that sending it yields
// a diagnosed 409 rather than a bare rejection, which is a diagnosis nobody
// needs to provoke on purpose.
func (r *fileStoreResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan, state fileStoreModel
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
	if (resumable[fileStoreModel]{
		kind: "file store", resourceName: ResourceName(fileStoreResourceSuffix),
		timeout: plan.Timeouts.Create, fallback: fileStoreStates.timeout(r.meta.Wait),
		wait: func(ctx context.Context) error {
			return r.waitForSettled(ctx, target, id, "the file store to finish provisioning")
		},
		read: func(ctx context.Context) (fileStoreModel, bool, diag.Diagnostics) {
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

	md, metadataChanged, diags := plan.metadata().patch(ctx, state.metadata())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	grants, ok := planGrantChange(ctx, plan, state, req.Private, &resp.Diagnostics)
	if !ok {
		return
	}
	// A group recreated under its id since the refresh would still read as
	// granted, and sending `consumers` would grant whatever group holds that
	// id now. So the grants are judged again against the live store.
	if grants.sends() && !r.grantsStillAllowed(ctx, target, id, plan, grants, &resp.Diagnostics) {
		return
	}
	restChanged := metadataChanged || !plan.CapacityGiB.Equal(state.CapacityGiB)

	// Nothing the platform holds is changing: a new `timeouts` block on its
	// own. A PATCH here would be a write nobody asked for, so prior state is
	// kept with the timeouts taken from the plan. `allow_replace` alone never
	// reaches here (replace_guard_resource.go).
	if !grants.sends() && !restChanged {
		state.Timeouts = plan.Timeouts
		resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
		return
	}

	body := updateRequest(ctx, plan, state, md, grants.sends(), &resp.Diagnostics)
	applied := r.patch(ctx, target, id, body, grants, restChanged, state, resp.Private, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	r.recordUpdate(ctx, target, id, plan, applied, resp)
}

// updateRequest is the PATCH body for an update.
func updateRequest(ctx context.Context, plan, state fileStoreModel, md metadataFields, sendConsumers bool, diags *diag.Diagnostics) k0rdent.FileStoreUpdateRequest {
	capacity := plan.CapacityGiB.ValueInt64()
	body := k0rdent.FileStoreUpdateRequest{
		DisplayName: md.DisplayName,
		Description: md.Description,
		CapacityGiB: &capacity,
		// Optimistic concurrency is an `etag` in the body, not an `If-Match`
		// header, so it cannot ride on a transport option. Sending it is the
		// difference between refusing a concurrent edit and clobbering it, and
		// a Terraform apply is exactly the long gap between read and write
		// where a concurrent edit happens.
		Etag: optionalString(state.Etag),
	}
	// Sent only when it changed or a group drifted. Re-sending an
	// instance-group entry resolves the group again, so a capacity change must
	// not quietly change who can mount. An empty list goes on the wire when it
	// is sent, because an absent field means "leave the consumers alone".
	if !sendConsumers {
		return body
	}
	body.Consumers = consumersRequest(consumersOf(ctx, plan.Consumers, diags))
	if body.Consumers == nil {
		body.Consumers = &[]k0rdent.Consumer{}
	}
	return body
}

// recordUpdate waits out what the update applied and writes the store as it
// then stands. applied is false when the platform took nothing, so there is
// nothing to wait for and only the read is left.
func (r *fileStoreResource) recordUpdate(ctx context.Context, target clusterTarget, id string, plan fileStoreModel, applied bool, resp *resource.UpdateResponse) {
	if applied {
		if err := r.waitForSettled(ctx, target, id, "the file store to settle"); err != nil {
			addWaitError(&resp.Diagnostics, "The file store did not settle after the change", err)
			return
		}
	}
	next, found, diags, err := settledRead(ctx, plan.State, fileStoreStates.settle,
		func(ctx context.Context) (fileStoreModel, bool, diag.Diagnostics) {
			return r.read(ctx, target, id, plan)
		},
		func(m fileStoreModel) types.String { return m.State },
		func(ctx context.Context) error { return r.waitForSettled(ctx, target, id, "the file store to settle") })
	resp.Diagnostics.Append(diags...)
	if err != nil {
		addWaitError(&resp.Diagnostics, "The file store did not settle after the change", err)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		// Local: the provider's conclusion from two calls, as in createFinish.
		resp.Diagnostics.AddError(
			"The file store disappeared during the update",
			fmt.Sprintf("Reading %q after the change answered 404.", id),
		)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
	warnOnConsumerFailures(ctx, next, &resp.Diagnostics)
}

// grantChange is what an update does with `consumers`.
type grantChange struct {
	report   grantReport
	refusals refusedResends
	// drifted are the groups whose entries go again because their membership
	// moved.
	drifted []string
	// changed says the configuration changed `consumers` itself.
	changed bool
	// configured are the refs `consumers` names.
	configured []string
}

// planGrantChange reads the grants the way the plan did. It reports false
// when an ungrantable group stops the update before anything is sent.
func planGrantChange(ctx context.Context, plan, state fileStoreModel, private privateState, diags *diag.Diagnostics) (grantChange, bool) {
	g := grantChange{
		report:     grantReportOf(ctx, state.ConsumerStatus, diags),
		refusals:   readRefused(ctx, private, diags),
		changed:    !plan.Consumers.Equal(state.Consumers),
		configured: configuredRefs(plan.Consumers),
	}
	// The plan already warned about each held re-send, so only the verdicts
	// are read here.
	verdicts := judgeGrants(g.report, g.configured, g.refusals)
	if refusal := verdicts.ungrantableErrors(); refusal.HasError() {
		diags.Append(refusal...)
		return g, false
	}
	g.drifted = verdicts.resend
	return g, true
}

// grantsStillAllowed judges the configured consumers against the live store.
// A store that reads as gone is left to the PATCH to report.
func (r *fileStoreResource) grantsStillAllowed(ctx context.Context, target clusterTarget, id string, plan fileStoreModel, g grantChange, diags *diag.Diagnostics) bool {
	live, found, d := r.read(ctx, target, id, plan)
	diags.Append(d...)
	if diags.HasError() {
		return false
	}
	if !found {
		return true
	}
	report := grantReportOf(ctx, live.ConsumerStatus, diags)
	diags.Append(judgeGrants(report, configuredRefs(plan.Consumers), g.refusals).ungrantableErrors()...)
	return !diags.HasError()
}

// sends says whether the update carries `consumers` at all.
func (g grantChange) sends() bool { return g.changed || len(g.drifted) > 0 }

// driftOnly says the entries go only because a group drifted.
func (g grantChange) driftOnly() bool { return !g.changed && len(g.drifted) > 0 }

// patch sends the change and reports whether the platform applied any of it.
//
// A store that isn't yet updatable is waited out first: see sendUpdate. A 422
// to a re-send that only drift asked for is the one refusal it settles
// itself, when a member is one the platform cannot vouch for. The rest of the
// change still goes, and the refusal is remembered so the next plan doesn't
// propose the same re-send forever. prior is the store as state holds it.
func (r *fileStoreResource) patch(ctx context.Context, target clusterTarget, id string, body k0rdent.FileStoreUpdateRequest, g grantChange, restChanged bool, prior fileStoreModel, private privateWriter, diags *diag.Diagnostics) bool {
	updated, sent, err := r.sendUpdate(ctx, target, id, &body, g, prior, diags)
	if !sent {
		return false
	}
	if g.driftOnly() && err == nil && updated.StatusCode() == http.StatusUnprocessableEntity {
		return r.patchWithoutResend(ctx, target, id, body, g, restChanged, updated, private, diags)
	}
	if len(g.drifted) > 0 {
		g.refusals.forget(g.drifted)
		diags.Append(writeRefused(ctx, private, g.refusals)...)
	}
	return updateAccepted(updated, err, diags)
}

// sendUpdate sends the PATCH, and sends it again for as long as the platform
// refuses it with CONFLICT_IN_USE because the store isn't yet updatable. It
// returns the last answer, or false when it gave up waiting, with the refusal
// and the wait in diags.
//
// The contract names the causes: the store is `creating`, `deleting` or
// `failed`, a previous capacity change is still settling its quota
// accounting, or the reconcile sweep is charging the store's quota, and says
// to retry shortly. A store `creating`, which one whose create stopped
// waiting or created by another run can be, is waited out until it leaves
// that state, within the update timeout. One that reads `active` is sent once
// more after a pause, which covers the quota causes. `deleting` and `failed`
// don't clear by waiting, so they're reported at once. See busyRetry.
//
// The body carries the etag state holds, read before the wait. That one is
// stale once a `creating` store settles, so an update resent with it would
// be refused as a concurrent edit nobody made. Each resend takes the etag
// from the read that ended the wait instead, unless that read shows the
// store changed from what state holds, which is a concurrent edit. Then the
// etag stays, and the platform refuses the update with CONFLICT_VERSION,
// which is what it's for.
func (r *fileStoreResource) sendUpdate(ctx context.Context, target clusterTarget, id string, body *k0rdent.FileStoreUpdateRequest, g grantChange, prior fileStoreModel, diags *diag.Diagnostics) (*k0rdent.UpdateFileStoreResponse, bool, error) {
	// last is the store as the newest read since the last PATCH found it.
	var last *k0rdent.FileStore
	read := r.reader(target, id)
	self := dependsOn(fmt.Sprintf("the file store %q", id),
		func(ctx context.Context) (*k0rdent.FileStore, *http.Response, []byte, error) {
			fs, resp, raw, err := read(ctx)
			if err == nil && fs != nil {
				last = fs
			}
			return fs, resp, raw, err
		},
		fileStoreStates.state, string(k0rdent.FileStoreStateCreating))
	busy := newBusyRetry("updateFileStore", r.meta.Wait, self)

	var updated *k0rdent.UpdateFileStoreResponse
	var sendErr error
	resend, refused := false, false
	waitErr := busy.sendUntilSettled(ctx, func(ctx context.Context) (*http.Response, []byte, error) {
		if resend {
			// A group recreated under its id during the wait would be granted
			// by the resend, so the grants are judged again first.
			if refusal := grantsRefusedBy(ctx, last, prior, g); body.Consumers != nil && refusal.HasError() {
				diags.Append(refusal...)
				refused = true
				return nil, nil, errGrantsRefused
			}
			body.Etag = etagAfterWait(ctx, last, prior, body.Etag)
		}
		resend, last = true, nil
		updated, sendErr = r.meta.Client.UpdateFileStoreWithResponse(ctx, target.region, target.project, id, *body)
		if sendErr != nil {
			return nil, nil, sendErr
		}
		return updated.HTTPResponse, updated.Body, nil
	})
	if waitErr != nil {
		busy.report(diags, "update the file store", updated.HTTPResponse, updated.Body, waitErr)
		return nil, false, nil
	}
	if refused {
		return nil, false, nil
	}
	return updated, true, sendErr
}

// errGrantsRefused ends a resend whose grants the newest read refuses.
var errGrantsRefused = errors.New("a consumer group can no longer be granted")

// grantsRefusedBy judges the configured consumers against last, if there was
// a read.
func grantsRefusedBy(ctx context.Context, last *k0rdent.FileStore, prior fileStoreModel, g grantChange) diag.Diagnostics {
	var diags diag.Diagnostics
	if last == nil {
		return diags
	}
	now, d := fileStoreStateFrom(ctx, *last, prior)
	if d.HasError() {
		return diags
	}
	return judgeGrants(grantReportOf(ctx, now.ConsumerStatus, &d), g.configured, g.refusals).ungrantableErrors()
}

// etagAfterWait is the etag to send again with, after a wait: the one fresh
// carries, when fresh still holds what prior does in every field an update
// writes, and sent otherwise. An update sent without an etag stays without.
func etagAfterWait(ctx context.Context, fresh *k0rdent.FileStore, prior fileStoreModel, sent *string) *string {
	if fresh == nil || fresh.Etag == nil || sent == nil {
		return sent
	}
	now, diags := fileStoreStateFrom(ctx, *fresh, prior)
	if diags.HasError() {
		return sent
	}
	if !now.DisplayName.Equal(prior.DisplayName) || !now.Description.Equal(prior.Description) ||
		!now.CapacityGiB.Equal(prior.CapacityGiB) || !now.Consumers.Equal(prior.Consumers) {
		tflog.Info(ctx, "the file store changed while the update waited for it, so the update keeps the "+
			"etag state holds and the platform can refuse it as a concurrent edit", map[string]any{"store": fresh.Id})
		return sent
	}
	return fresh.Etag
}

// patchWithoutResend answers a 422 to a re-send that only drift asked for.
//
// When the re-send was the whole change, the 422 can only be about it, and a
// second PATCH would carry nothing but the unchanged capacity and the etag,
// so none is sent. Otherwise the 422 is read as the re-send's only if the same
// change without `consumers` is accepted. If that is refused too, its answer
// is the one about the user's change, so it's what gets reported, and nothing
// is remembered.
func (r *fileStoreResource) patchWithoutResend(ctx context.Context, target clusterTarget, id string, body k0rdent.FileStoreUpdateRequest, g grantChange, restChanged bool, refused *k0rdent.UpdateFileStoreResponse, private privateWriter, diags *diag.Diagnostics) bool {
	if restChanged {
		body.Consumers = nil
		retried, err := r.meta.Client.UpdateFileStoreWithResponse(ctx, target.region, target.project, id, body)
		if !updateAccepted(retried, err, diags) {
			return false
		}
	}
	g.refusals.remember(g.report, g.drifted)
	diags.Append(writeRefused(ctx, private, g.refusals)...)
	diags.Append(refusedResendWarning(g.drifted, refusalMessage(refused.HTTPResponse, refused.Body))...)
	return restChanged
}

// updateAccepted reports whether an update was accepted, and the platform's
// refusal when it was not.
func updateAccepted(updated *k0rdent.UpdateFileStoreResponse, err error, diags *diag.Diagnostics) bool {
	if err == nil && updated.JSON200 != nil {
		return true
	}
	var respBody []byte
	var httpResp *http.Response
	if updated != nil {
		respBody, httpResp = updated.Body, updated.HTTPResponse
	}
	addAPIError(diags, "update the file store", httpResp, respBody, err)
	addConcurrentEditHint(httpResp, respBody, diags)
	return false
}

// Delete detaches every consumer and then deletes.
//
// The detach takes access away the moment the platform accepts it, so from
// then on state follows the store, and a destroy that fails afterwards says
// what it already did. See recordDetach.
//
// The extra mutation is not tidiness. An `active` store that still lists
// consumers is refused with 409, so a plain delete of a store anyone can mount
// fails, and no other resource in this provider has that shape. The alternative
// the contract offers is `force=true`, which is an operator escape hatch that
// removes the record without contacting the storage backend and leaves the
// backing object and its data in place for manual cleanup. A provider that
// reached for it would turn `terraform destroy` into silent orphaning of
// finite physical capacity, so it is not used here and is not exposed.
func (r *fileStoreResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state fileStoreModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(r.meta, state.Region, state.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, state.Timeouts.Delete, fileStoreStates.timeout(r.meta.Wait), &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	// The store as it is now, not as state recorded it: a store still
	// `creating` refuses both the detach and the delete, and `-refresh=false`
	// leaves state blind to a consumer attached out of band.
	live, err := r.awaitDeletable(ctx, target, id)
	if err != nil {
		addWaitError(&resp.Diagnostics, "The file store could not be deleted", err)
		return
	}
	if live == nil {
		return
	}
	// A destroy that stopped waiting left the store `deleting`. That delete
	// is waited out: there's nothing left to detach, and nothing to send.
	if stateString(live.State) == fileStoreStates.deleting {
		tflog.Info(ctx, "the file store is already being deleted, so that delete is waited out rather than sent again",
			map[string]any{"file_store": id})
		fileStoreStates.reportDeleteWait(&resp.Diagnostics,
			fileStoreStates.awaitDeleted(ctx, r.meta.Wait, "the file store to finish deleting", r.reader(target, id)))
		return
	}

	detached := consumersBlockDelete(*live)
	if detached {
		warnOnUnlistedConsumers(ctx, state, *live, &resp.Diagnostics)
		record := func(fs k0rdent.FileStore) { recordDetach(ctx, fs, state, &resp.State, &resp.Diagnostics) }
		if !r.detachConsumers(ctx, target, id, live.CapacityGiB, record, &resp.Diagnostics) {
			return
		}
	}

	gone, err := r.deleteOnceIdle(ctx, target, id)
	if err != nil {
		addWaitError(&resp.Diagnostics, "Could not delete the file store", err)
		if detached {
			addDetachedNote(id, &resp.Diagnostics)
		}
		return
	}
	if gone {
		return
	}

	fileStoreStates.reportDeleteWait(&resp.Diagnostics,
		fileStoreStates.awaitDeleted(ctx, r.meta.Wait, "the file store to finish deleting", r.reader(target, id)))
}

// consumersBlockDelete says whether the store's consumers have to be
// detached before it can be deleted. The platform checks consumers on an
// active store alone. A failed store's grants were never enforced, and a
// detach there is only refused.
func consumersBlockDelete(fs k0rdent.FileStore) bool {
	return stateString(fs.State) == string(k0rdent.FileStoreStateActive) &&
		fs.Consumers != nil && len(*fs.Consumers) > 0
}

// warnOnUnlistedConsumers names the consumers the platform lists that state
// doesn't. The plan's warning was built from state, so these are the ones
// nobody was told about: attached after the refresh, or skipped by
// `-refresh=false`. The destroy cuts them off all the same.
func warnOnUnlistedConsumers(ctx context.Context, state fileStoreModel, fs k0rdent.FileStore, diags *diag.Diagnostics) {
	var unlisted []string
	known := state.Consumers.Elements()
	for _, c := range *fs.Consumers {
		if _, ok := known[c.Ref]; !ok {
			unlisted = append(unlisted, c.Ref)
		}
	}
	if len(unlisted) == 0 {
		return
	}
	sort.Strings(unlisted)
	tflog.Info(ctx, "detaching consumers state did not list", map[string]any{"store": state.ID.ValueString()})
	// Local: the comparison is the provider's. The refs are the platform's
	// words, so they're escaped.
	diags.AddWarning("Detaching consumers this configuration did not list", redactDiagnosticText(fmt.Sprintf(
		"The platform lists consumers on %s that state did not, so the plan could not name them. "+
			"They lose access to the store along with the rest:\n\n  %s",
		state.ID.ValueString(), strings.Join(unlisted, "\n  "))))
}

// recordDetach writes the store as the detach left it. From the moment the
// platform accepts the detach, its consumers have lost access, so a destroy
// that fails after that point must not leave state claiming they still have
// it: the next plan then shows the grants going back on, which is the truth.
//
// It's bookkeeping, so it only ever warns. An error here would fail a destroy
// whose DELETE then goes through, and keep a store that's gone in state.
func recordDetach(ctx context.Context, fs k0rdent.FileStore, prior fileStoreModel, st *tfsdk.State, diags *diag.Diagnostics) {
	next, d := fileStoreStateFrom(ctx, fs, prior)
	if !d.HasError() {
		d.Append(st.Set(ctx, &next)...)
	}
	if !d.HasError() {
		diags.Append(d...)
		return
	}
	var reasons []string
	for _, e := range d.Errors() {
		reasons = append(reasons, e.Summary()+": "+e.Detail())
	}
	tflog.Warn(ctx, "could not record the file store's detach in state", map[string]any{"store": fs.Id})
	// Local: the provider failed to write its own state. The id is the
	// platform's word, so it's escaped.
	diags.AddWarning("Could not record the detach in state", redactDiagnosticText(fmt.Sprintf(
		"The platform accepted the detach of every consumer from %s, but writing that to state "+
			"failed, so if the destroy stops before the store is gone, state may still list them. "+
			"The destroy carries on.\n\n%s", fs.Id, strings.Join(reasons, "\n"))))
}

// addDetachedNote says what a destroy that failed after its detach has
// already done, and the two ways on from there.
func addDetachedNote(id string, diags *diag.Diagnostics) {
	// Local: the provider's account of its own requests.
	diags.AddWarning("The file store's consumers were detached", redactDiagnosticText(fmt.Sprintf(
		"The destroy detached every consumer from %s before the step that failed, so they have "+
			"lost access to it, and state records that. Running the destroy again carries on from "+
			"here. Running `terraform apply` instead grants the configured consumers again.", id)))
}

// deleteOnceIdle sends the DELETE until the store accepts it, and reports
// whether the store was already gone.
//
// A refusal is retried only while the store is active with nothing
// attached, where the contract's causes are a change still being applied
// or a missing export being re-checked. One permanent cause, an active store
// with no recorded namespace, looks the same and is ended by the timeout.
// Every other cause needs a person, so it is reported at once.
func (r *fileStoreResource) deleteOnceIdle(ctx context.Context, target clusterTarget, id string) (bool, error) {
	var gone bool
	err := awaitAdmitted(ctx, r.meta.Wait, "the file store to accept the delete",
		func(ctx context.Context) (*http.Response, []byte, error) {
			deleted, err := r.meta.Client.DeleteFileStoreWithResponse(ctx, target.region, target.project, id,
				&k0rdent.DeleteFileStoreParams{})
			if err != nil {
				return nil, nil, err
			}
			return deleted.HTTPResponse, deleted.Body, nil
		},
		func(ctx context.Context, resp *http.Response, body []byte) (bool, *k0rdent.APIError, error) {
			switch {
			case isNotFound(resp, body):
				gone = true
				return true, nil, nil
			case resp != nil && resp.StatusCode < 300:
				return true, nil, nil
			}
			checked := k0rdent.CheckResponse(resp, body)
			var refusal *k0rdent.APIError
			if !errors.As(checked, &refusal) || !refusal.HasReason("CONFLICT_IN_USE") {
				return false, nil, checked
			}
			got, readErr := r.meta.Client.GetFileStoreWithResponse(ctx, target.region, target.project, id)
			if readErr != nil {
				return false, nil, checked
			}
			if isNotFound(got.HTTPResponse, got.Body) {
				gone = true
				return true, nil, nil
			}
			if got.JSON200 == nil {
				return false, nil, checked
			}
			// The provider's own DELETE, resent after a 504 or a dropped
			// connection, finds the store already `deleting`. The contract
			// answers that with 204, and a 409 is read the same way: as the
			// delete under way that it is. See startDelete.
			if stateString(got.JSON200.State) == fileStoreStates.deleting {
				return true, nil, nil
			}
			idle := got.JSON200.Consumers == nil || len(*got.JSON200.Consumers) == 0
			if stateString(got.JSON200.State) == string(k0rdent.FileStoreStateActive) && idle {
				return false, refusal, nil
			}
			return false, nil, checked
		})
	return gone, err
}

// awaitDeletable waits out a store that is still `creating` and returns it as
// it then stands, or nil once it is gone.
func (r *fileStoreResource) awaitDeletable(ctx context.Context, target clusterTarget, id string) (*k0rdent.FileStore, error) {
	var live *k0rdent.FileStore
	err := waitFor(ctx, r.meta.Wait, "the file store to finish provisioning before it is deleted", func(ctx context.Context) (bool, error) {
		got, err := r.meta.Client.GetFileStoreWithResponse(ctx, target.region, target.project, id)
		if err != nil {
			return false, err
		}
		if isNotFound(got.HTTPResponse, got.Body) {
			live = nil
			return true, nil
		}
		if got.JSON200 == nil {
			return false, k0rdent.CheckResponse(got.HTTPResponse, got.Body)
		}
		live = got.JSON200
		// Any other state, recognised or not, is left to the DELETE to judge.
		return stateString(live.State) != string(k0rdent.FileStoreStateCreating), nil
	})
	return live, err
}

// detachConsumers empties the grant list and waits for the store to report it
// gone. It reports whether the delete may proceed. record, when set, is
// handed the store each time the detach is seen to have moved it, so state
// follows. Once the PATCH is accepted, any failure also says the consumers
// were detached.
func (r *fileStoreResource) detachConsumers(ctx context.Context, target clusterTarget, id string, capacity int64, record func(k0rdent.FileStore), diags *diag.Diagnostics) bool {
	if record == nil {
		record = func(k0rdent.FileStore) {}
	}
	// Capacity goes in the body unchanged because the contract requires at
	// least one updatable field and an etag-only body is a 400. No etag: on the
	// way to a delete there is nothing left to protect, and a stale one would
	// only turn an approved destroy into a 409.
	detached, err := r.meta.Client.UpdateFileStoreWithResponse(ctx, target.region, target.project, id,
		k0rdent.FileStoreUpdateRequest{
			CapacityGiB: &capacity,
			Consumers:   &[]k0rdent.Consumer{},
		})
	switch {
	case err != nil:
		addAPIError(diags, "detach the file store's consumers before deleting it", nil, nil, err)
		return false
	case isNotFound(detached.HTTPResponse, detached.Body):
		// Proceed: the DELETE answers 404 too if the store has gone, and that
		// is handled as success. Stopping here would drop the resource from
		// state without ever sending the DELETE.
		return true
	case detached.JSON200 == nil:
		addAPIError(diags, "detach the file store's consumers before deleting it",
			detached.HTTPResponse, detached.Body, nil)
		addConcurrentEditHint(detached.HTTPResponse, detached.Body, diags)
		return false
	}
	record(*detached.JSON200)

	// The delete is refused while a change is still being applied, so this
	// waits for the revocations too, not only for the desired list to empty.
	err = waitFor(ctx, r.meta.Wait, "the file store's consumers to be detached", func(ctx context.Context) (bool, error) {
		got, readErr := r.meta.Client.GetFileStoreWithResponse(ctx, target.region, target.project, id)
		if readErr != nil {
			return false, readErr
		}
		if isNotFound(got.HTTPResponse, got.Body) {
			return true, nil
		}
		if got.JSON200 == nil {
			return false, k0rdent.CheckResponse(got.HTTPResponse, got.Body)
		}
		record(*got.JSON200)
		return detachSettled(*got.JSON200), nil
	})
	if err != nil {
		addWaitError(diags, "The file store's consumers did not finish detaching before the delete", err)
		addDetachedNote(id, diags)
		return false
	}
	return true
}

// detachSettled says whether a store has finished detaching its consumers.
// A failed store, or an active one whose last change failed, keeps
// `reconciling` true for good, so either counts as settled once no
// revocation is still in flight.
func detachSettled(fs k0rdent.FileStore) bool {
	if fs.Consumers != nil && len(*fs.Consumers) > 0 {
		return false
	}
	if fs.Reconciling == nil || !*fs.Reconciling {
		return true
	}
	return lastChangeFailed(fs) && !revoking(fs)
}

// lastChangeFailed says whether the store, or the last change made to it,
// failed, which is what leaves `reconciling` true for good.
func lastChangeFailed(fs k0rdent.FileStore) bool {
	return stateString(fs.State) == string(k0rdent.FileStoreStateFailed) ||
		stateString(fs.StateReason) != "" || deref(fs.StateMessage) != "" || deref(fs.StatusReason) != ""
}

// revoking says whether any consumer's grant is still being withdrawn.
func revoking(fs k0rdent.FileStore) bool {
	if fs.ConsumerStatus == nil {
		return false
	}
	for _, cs := range *fs.ConsumerStatus {
		if stateString(cs.State) == string(k0rdent.FileStoreConsumerStatusStateDeleting) {
			return true
		}
	}
	return false
}

func (r *fileStoreResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	importScoped(ctx, r.meta, importForm{
		resource: ResourceName(fileStoreResourceSuffix),
		key:      "id",
	}, req, resp)
}

// IdentitySchema is what an import block's `identity` takes.
func (r *fileStoreResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = scopedIdentitySchema("id", "")
}

func (r *fileStoreResource) read(ctx context.Context, target clusterTarget, id string, prior fileStoreModel) (fileStoreModel, bool, diag.Diagnostics) {
	var diags diag.Diagnostics

	got, err := r.meta.Client.GetFileStoreWithResponse(ctx, target.region, target.project, id)
	if err != nil {
		addAPIError(&diags, "read the file store", nil, nil, err)
		return fileStoreModel{}, false, diags
	}
	if isNotFound(got.HTTPResponse, got.Body) {
		return fileStoreModel{}, false, diags
	}
	if got.JSON200 == nil {
		addAPIError(&diags, "read the file store", got.HTTPResponse, got.Body, nil)
		return fileStoreModel{}, false, diags
	}

	out, d := fileStoreStateFrom(ctx, *got.JSON200, prior)
	diags.Append(d...)
	return out, true, diags
}

// waitForSettled polls until the store reaches a terminal state.
func (r *fileStoreResource) waitForSettled(ctx context.Context, target clusterTarget, id, what string) error {
	return fileStoreStates.awaitSettled(ctx, r.meta.Wait, what, r.reader(target, id))
}

// reader reads the file store for a wait.
func (r *fileStoreResource) reader(target clusterTarget, id string) stateRead[k0rdent.FileStore] {
	return func(ctx context.Context) (*k0rdent.FileStore, *http.Response, []byte, error) {
		got, err := r.meta.Client.GetFileStoreWithResponse(ctx, target.region, target.project, id)
		if err != nil {
			return nil, nil, nil, err
		}
		return got.JSON200, got.HTTPResponse, got.Body, nil
	}
}

// fileStoreStates is what a file store's waits make of each state it reports.
var fileStoreStates = stateKind[k0rdent.FileStore]{
	name:     "file store",
	resource: fileStoreResourceSuffix,
	deleting: string(k0rdent.FileStoreStateDeleting),
	state: func(fs k0rdent.FileStore) (string, bool) {
		return stateString(fs.State), stateString(fs.State) != ""
	},
	detail: func(fs k0rdent.FileStore) string {
		return stateDetail(fs.StateMessage, stateString(fs.StateReason))
	},
	ready: fileStoreApplied,
	settle: stateTable{
		missing: stateRule{verdict: unrecognised},
		states: map[string]stateRule{
			string(k0rdent.FileStoreStateActive):   {verdict: settledOnceReady},
			string(k0rdent.FileStoreStateCreating): {verdict: keepWaiting},
			string(k0rdent.FileStoreStateFailed):   {verdict: failsTerminally, detail: true},
			// `deleting` is terminal here rather than pending, which is the one
			// place this differs from the other resources' waiters. A store
			// that is being torn down while the provider waits for it to become
			// active will never become active, so reporting it now beats
			// burning the timeout to say the same thing later.
			string(k0rdent.FileStoreStateDeleting): {
				verdict: failsTerminally, detail: true,
				says: "the platform reports %s as %s, so it is being torn down and will not become active",
			},
		},
	},
	delete: stateTable{
		missing: stateRule{verdict: unrecognised},
		states: map[string]stateRule{
			string(k0rdent.FileStoreStateDeleting): {verdict: keepWaiting},
			string(k0rdent.FileStoreStateFailed):   {verdict: failsTerminally, detail: true},
			string(k0rdent.FileStoreStateActive):   {verdict: failsTerminally, says: afterDelete},
			string(k0rdent.FileStoreStateCreating): {verdict: failsTerminally, says: afterDelete},
		},
	},
}

// fileStoreApplied reports whether an `active` store has finished applying its
// last change.
//
// # Why `state` comes before `reconciling`
//
// A `failed` store keeps reporting `reconciling: true`, so a waiter gated on it
// alone never returns. The lifecycle verdict is `state`, and `reconciling` is
// read only on an `active` store.
//
// It is not published as an attribute either. An attribute whose documentation
// has to say "do not act on this" has not earned a place in a public schema,
// and `state` already answers the question a reader would reach for it to ask.
//
// An `active` store can still be applying a change: a grant shows as a
// consumer in `creating` or `deleting`, and an expansion only as `reconciling`.
// The wait holds for both. A day-2 change that failed leaves the store `active`
// with a reason, which the contract says to treat as terminal.
//
// # Why a `failed` consumer is not a failure here
//
// A grant that resolved to no address settles the *consumer* `failed` while the
// store itself is `active`, and `GROUP_MEMBERSHIP_DRIFTED` is reported the same
// way while the grant is still enforced. Failing the apply on either would fail
// on the routine case, an instance group scaling up, and the store is usable
// in both. So the wait is on the store, and per-consumer trouble is published
// in `consumer_status` and raised as a warning. See warnOnConsumerFailures.
func fileStoreApplied(fs k0rdent.FileStore) (bool, error) {
	if reason := stateString(fs.StateReason); reason != "" || deref(fs.StateMessage) != "" ||
		deref(fs.StatusReason) != "" {
		return false, fmt.Errorf("a change to the file store failed, and the store stays active%s",
			stateDetail(fs.StateMessage, reason))
	}
	// A failed consumer can hold `reconciling` true for good, and is
	// reported as a warning rather than waited on.
	grantFailed := false
	if fs.ConsumerStatus != nil {
		for _, cs := range *fs.ConsumerStatus {
			switch stateString(cs.State) {
			case string(k0rdent.FileStoreConsumerStatusStateCreating),
				string(k0rdent.FileStoreConsumerStatusStateDeleting):
				return false, nil
			case string(k0rdent.FileStoreConsumerStatusStateFailed):
				grantFailed = true
			}
		}
	}
	return grantFailed || fs.Reconciling == nil || !*fs.Reconciling, nil
}

// addConcurrentEditHint says what a CONFLICT_VERSION actually means, because
// the bare diagnostic reads like a transient failure and it is not one.
//
// The class travels in `error.details[].reason` rather than at the top level,
// so it is read through HasReason, which checks both places.
func addConcurrentEditHint(resp *http.Response, body []byte, diags *diag.Diagnostics) {
	var apiErr *k0rdent.APIError
	if !errors.As(k0rdent.CheckResponse(resp, body), &apiErr) {
		return
	}
	if !apiErr.HasReason("CONFLICT_VERSION") {
		return
	}
	// Local: the provider's reading of a code the API already reported.
	diags.AddWarning(
		"The file store was changed by something else",
		"The update carried the revision this provider last read and the platform has a newer "+
			"one, so the change was refused rather than clobbering whatever else wrote to the "+
			"store. Run `terraform refresh`, check what changed, then apply again.",
	)
}

// fileStoreStateFrom maps a FileStore onto the Terraform model.
//
// Every server-reported field is taken from the response rather than kept from
// the configuration. The keepConfigured pattern used on `vlan_id` and `pkey`
// exists for attributes whose request value and response value are different by
// design, and nothing here is like that: a consumer's `type` and `access_level`
// come back as the concrete value the platform applied, so taking the response
// is both quiet and honest. Keeping the configured value instead would silence
// a permanent diff by disabling drift detection on the grant list, which is the
// one thing on this resource worth detecting drift on.
func fileStoreStateFrom(ctx context.Context, fs k0rdent.FileStore, prior fileStoreModel) (fileStoreModel, diag.Diagnostics) {
	var diags diag.Diagnostics

	md := prior.metadata().read(metadataAnswer{DisplayName: fs.DisplayName, Description: fs.Description})
	out := fileStoreModel{

		ID:          types.StringValue(fs.Id),
		Region:      prior.Region,
		Project:     prior.Project,
		Timeouts:    prior.Timeouts,
		ShareType:   types.StringValue(fs.ShareType),
		CapacityGiB: types.Int64Value(fs.CapacityGiB),
		DisplayName: md.DisplayName,
		Description: md.Description,
		State:       types.StringNull(),
		Etag:        stringOrNull(fs.Etag),
		SelfLink:    stringOrNull(fs.SelfLink),
	}
	if fs.State != nil {
		out.State = types.StringValue(string(*fs.State))
	}

	// `consumers` is Optional and not Computed, so state has to match the
	// configuration exactly. The platform answers an absent list and an empty
	// one the same way, with `consumers: []`, and turning that into an empty
	// list where the configuration wrote nothing is "produced an unexpected new
	// value: was null, but now []". So the null-ness of what was planned is
	// what decides, and only when there is nothing to report either way.
	//
	// This does not weaken drift detection: a consumer attached out of band
	// makes the list non-empty, and a non-empty list is always reported.
	out.Consumers = types.MapNull(fileStoreConsumerType())
	var listed []k0rdent.Consumer
	if fs.Consumers != nil {
		listed = *fs.Consumers
	}
	if len(listed) > 0 || !prior.Consumers.IsNull() {
		consumers := make(map[string]fileStoreConsumerModel, len(listed))
		for _, c := range listed {
			// Both fields are decoded rather than copied, because the
			// platform preserves a body as it was written: a request that
			// omitted `type` is read back without it. Reporting null would
			// publish "no kind" for an entry that has one, and would leave a
			// Computed attribute empty for anyone reading `consumers[*].type`.
			//
			// The values are not invented. The contract states both meanings:
			// an absent `type` is `ipAddress` ("keeps the historical meaning"),
			// and an absent `accessLevel` is `rw`, which is the only level that
			// can be granted at all. Decoding them is stable whether or not the
			// platform starts echoing its own defaults.
			entry := fileStoreConsumerModel{
				Type:        types.StringValue(string(k0rdent.ConsumerTypeIpAddress)),
				AccessLevel: types.StringValue(string(k0rdent.ConsumerAccessLevelRw)),
				MountPath:   stringOrNull(c.MountPath),
			}
			if c.Type != nil {
				entry.Type = types.StringValue(string(*c.Type))
			}
			if c.AccessLevel != nil {
				entry.AccessLevel = types.StringValue(string(*c.AccessLevel))
			}
			if c.Network != nil {
				entry.Network = &fileStoreConsumerNetworkModel{
					Type:       types.StringValue(string(c.Network.Type)),
					MacAddress: stringOrNull(c.Network.MacAddress),
					GUID:       stringOrNull(c.Network.Guid),
				}
			}
			consumers[c.Ref] = entry
		}
		m, d := types.MapValueFrom(ctx, fileStoreConsumerType(), consumers)
		diags.Append(d...)
		out.Consumers = m
	}

	statuses := map[string]fileStoreConsumerStatusModel{}
	if fs.ConsumerStatus != nil {
		for _, cs := range *fs.ConsumerStatus {
			// Keyed by ref, so an entry without one has nowhere to go.
			if deref(cs.Ref) == "" {
				continue
			}
			addresses, d := stringSetOrNull(ctx, cs.ResolvedAddresses)
			diags.Append(d...)

			entry := fileStoreConsumerStatusModel{
				State:             types.StringNull(),
				StateReason:       types.StringNull(),
				ResolvedAddresses: addresses,
			}
			if cs.State != nil {
				entry.State = types.StringValue(string(*cs.State))
			}
			if cs.StateReason != nil {
				entry.StateReason = types.StringValue(string(*cs.StateReason))
			}
			statuses[deref(cs.Ref)] = entry
		}
	}
	statusMap, d := types.MapValueFrom(ctx, fileStoreConsumerStatusType(), statuses)
	diags.Append(d...)
	out.ConsumerStatus = statusMap

	locations := []fileStoreExportLocationModel{}
	if fs.ExportLocations != nil {
		for _, e := range *fs.ExportLocations {
			entry := fileStoreExportLocationModel{
				Path:      stringOrNull(e.Path),
				Server:    stringOrNull(e.Server),
				Protocol:  types.StringNull(),
				Options:   stringOrNull(e.Options),
				Preferred: boolOrNull(e.Preferred),
				AdminOnly: boolOrNull(e.AdminOnly),
			}
			if e.Protocol != nil {
				entry.Protocol = types.StringValue(string(*e.Protocol))
			}
			locations = append(locations, entry)
		}
	}
	sort.SliceStable(locations, func(i, j int) bool {
		a, b := locations[i], locations[j]
		if a.Server.ValueString() != b.Server.ValueString() {
			return a.Server.ValueString() < b.Server.ValueString()
		}
		return a.Path.ValueString() < b.Path.ValueString()
	})
	locationList, d := types.ListValueFrom(ctx, fileStoreExportLocationType(), locations)
	diags.Append(d...)
	out.ExportLocations = locationList

	out.PreferredExport = types.ObjectNull(preferredExportType().AttrTypes)
	for _, l := range locations {
		if l.Preferred.ValueBool() && !l.AdminOnly.ValueBool() {
			preferred, d := types.ObjectValue(preferredExportType().AttrTypes, map[string]attr.Value{
				"path": l.Path, "server": l.Server, "protocol": l.Protocol, "options": l.Options,
			})
			diags.Append(d...)
			out.PreferredExport = preferred
			break
		}
	}

	return out, diags
}

// grantableIPv4 reports whether ref is an address the platform can grant: one
// literal IPv4 address naming a host. The parser refuses leading zeros, so a
// valid key is already in the only spelling the platform reads back.
func grantableIPv4(ref string) bool {
	addr, err := netip.ParseAddr(ref)
	if err != nil || !addr.Is4() {
		return false
	}
	return !addr.IsUnspecified() && !addr.IsLoopback() && !addr.IsMulticast() &&
		!addr.IsLinkLocalUnicast() && addr != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}
