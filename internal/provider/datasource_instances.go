package provider

import (
	"context"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ datasource.DataSource              = (*instancesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*instancesDataSource)(nil)
)

const instancesDataSourceSuffix = "instances"

// instancePageSize is listInstances' own ceiling. Every instance on a page is
// read from the infrastructure service in one batch, so the operation caps
// `pageSize` at 200 and answers 422 above it rather than clamping.
const instancePageSize = 200

func newInstancesDataSource() datasource.DataSource { return &instancesDataSource{} }

// instancesDataSource lists the hosts an instance group holds.
//
// # Why it exists
//
// A file store grant to an instance group can narrow the address it grants to
// one interface, by `mac_address` on Ethernet or `guid` on InfiniBand. Those
// are a machine's hardware identifiers, and the platform reports them on an
// instance, in `networkConfiguration`. Without a read over instances the value
// came from outside Terraform. This reads listInstances, which the contract
// marks public, and carries each interface's identifiers under the names the
// file store grant takes.
//
// # What it deliberately does not carry
//
// The host's hardware inventory. No argument anywhere in the provider takes a
// value from it, and its GPU and hardware fields are catalog values, which the
// hardcoded values check would require to be listed on the hardcoded values guide.
type instancesDataSource struct{ meta *Meta }

type instancesModel struct {
	Region        types.String         `tfsdk:"region"`
	Project       types.String         `tfsdk:"project"`
	InstanceGroup types.String         `tfsdk:"instance_group"`
	NodePool      types.String         `tfsdk:"node_pool"`
	State         types.String         `tfsdk:"state"`
	Instances     []instanceEntryModel `tfsdk:"instances"`
	IDs           types.List           `tfsdk:"ids"`
}

// instanceEntryModel is one instance, in the list and, embedded, in the
// singular data source.
type instanceEntryModel struct {
	ID                types.String             `tfsdk:"id"`
	UID               types.String             `tfsdk:"uid"`
	InstanceGroup     types.String             `tfsdk:"instance_group"`
	NodePool          types.String             `tfsdk:"node_pool"`
	ServerSlug        types.String             `tfsdk:"server_slug"`
	Hostname          types.String             `tfsdk:"hostname"`
	State             types.String             `tfsdk:"state"`
	StateMessage      types.String             `tfsdk:"state_message"`
	Health            types.String             `tfsdk:"health"`
	PowerActionError  types.String             `tfsdk:"last_power_action_error"`
	CreatedAt         types.String             `tfsdk:"created_at"`
	SelfLink          types.String             `tfsdk:"self_link"`
	NetworkInterfaces []instanceInterfaceModel `tfsdk:"network_interfaces"`
}

type instanceInterfaceModel struct {
	Type        types.String   `tfsdk:"type"`
	MacAddress  types.String   `tfsdk:"mac_address"`
	GUID        types.String   `tfsdk:"guid"`
	IPAddresses []types.String `tfsdk:"ip_addresses"`
}

// instanceAttributes is every computed attribute of one instance. id is left
// to the caller, because the list computes it and the singular takes it.
func instanceAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"uid": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "The same identity as `id`, in UUID form.",
		},
		"instance_group": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "Id of the instance group that holds this host, the value a " +
				"file store grant's `ref` takes.",
		},
		"node_pool": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "The group's node pool this host was provisioned into.",
		},
		"server_slug": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "The host's registered name.",
		},
		"hostname": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "The host's effective hostname right now. Usually `server_slug`.",
		},
		"state": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "Lifecycle state, read from the host on every request: " +
				"`not_provisioned`, `provisioning`, `running`, `deprovisioning`, `deleting`, " +
				"`degraded` or `unknown`.",
		},
		"state_message": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Why the instance is in `state`. Null while `running`.",
		},
		"health": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "Host health as the infrastructure service observes it: " +
				"`healthy`, `degraded`, `unhealthy` or `unknown`. Separate from `state`: a " +
				"`running` host can be `unhealthy`.",
		},
		"last_power_action_error": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "Why the most recent power action on the host's machine is known to have " +
				"failed, whether it was sent by `" + ResourceName(instancePowerActionSuffix) + "` or by an " +
				"operator on the machine directly. Null when no failure is known, which is also how a power " +
				"action that succeeded reads: the platform doesn't report success. Separate from `state`.",
		},
		"created_at": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "When the instance was created, as RFC 3339.",
		},
		"self_link": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Platform-defined reference to this instance.",
		},
		// A List in the platform's order: an interface has no id of its own to
		// sort by, and the order is the host's.
		"network_interfaces": schema.ListNestedAttribute{
			Computed: true,
			MarkdownDescription: "The host's runtime network interfaces, as the infrastructure " +
				"service reports them now. Empty until the host is provisioned. An Ethernet " +
				"interface carries `mac_address` and an InfiniBand one carries `guid`, the two " +
				"values a `" + ResourceName(fileStoreResourceSuffix) + "` grant's " +
				"`consumers.network` takes to narrow it to one interface.",
			NestedObject: schema.NestedAttributeObject{
				Attributes: map[string]schema.Attribute{
					"type": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "`ethernet` or `infiniband`.",
					},
					"mac_address": schema.StringAttribute{
						Computed: true,
						MarkdownDescription: "Hardware address of an Ethernet interface, in the " +
							"colon-separated form a file store grant's `mac_address` takes. Null " +
							"on InfiniBand.",
					},
					"guid": schema.StringAttribute{
						Computed: true,
						MarkdownDescription: "GUID of an InfiniBand interface, in the `0x`-prefixed " +
							"form a file store grant's `guid` takes. Null on Ethernet.",
					},
					"ip_addresses": schema.ListAttribute{
						Computed:            true,
						ElementType:         types.StringType,
						MarkdownDescription: "Addresses assigned to the interface now.",
					},
				},
			},
		},
	}
}

// instanceEntryOf turns one instance into its state.
func instanceEntryOf(in k0rdent.Instance) instanceEntryModel {
	e := instanceEntryModel{
		ID:                stringOrNull(in.Id),
		UID:               types.StringNull(),
		InstanceGroup:     stringOrNull(in.InstanceGroupId),
		NodePool:          stringOrNull(in.NodePool),
		ServerSlug:        stringOrNull(in.ServerSlug),
		Hostname:          stringOrNull(in.Hostname),
		State:             enumOrNull(in.State),
		StateMessage:      stringOrNull(in.StateMessage),
		Health:            types.StringNull(),
		PowerActionError:  stringOrNull(in.LastPowerActionError),
		CreatedAt:         timeOrNull(in.CreatedAt),
		SelfLink:          stringOrNull(in.SelfLink),
		NetworkInterfaces: []instanceInterfaceModel{},
	}
	if in.Uid != nil {
		e.UID = types.StringValue(in.Uid.String())
	}
	if in.Health != nil {
		e.Health = enumOrNull(in.Health.State)
	}
	for _, nic := range rowsOf(in.NetworkConfiguration) {
		ips := []types.String{}
		for _, ip := range rowsOf(nic.IpAddresses) {
			ips = append(ips, types.StringValue(ip))
		}
		e.NetworkInterfaces = append(e.NetworkInterfaces, instanceInterfaceModel{
			Type:        types.StringValue(string(nic.Type)),
			MacAddress:  stringOrNull(nic.MacAddress),
			GUID:        stringOrNull(nic.Guid),
			IPAddresses: ips,
		})
	}
	return e
}

func (d *instancesDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(instancesDataSourceSuffix)
}

func (d *instancesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	entry := instanceAttributes()
	entry["id"] = schema.StringAttribute{
		Computed:            true,
		MarkdownDescription: "The instance id.",
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every instance in a project: the hosts its instance groups hold.\n\n" +
			"An instance is materialized when its group is created or scaled, so there is no " +
			"resource for one. This is how a configuration reads a host's interfaces, whose " +
			"`mac_address` or `guid` narrows a `" + ResourceName(fileStoreResourceSuffix) + "` " +
			"grant to one interface.",
		Attributes: map[string]schema.Attribute{
			"region":  dataSourceRegionAttribute(),
			"project": dataSourceProjectAttribute(),
			"instance_group": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Only return the hosts of the instance group with this id. Filtered by the platform.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"node_pool": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Only return the hosts provisioned into a node pool with this name. " +
					"Pool names come from the ClusterType, so on its own this matches that pool in " +
					"every group: set `instance_group` too for one group's pool. Filtered by the platform.",
				Validators: []validator.String{urlSafeIdentifier{}},
			},
			"state": func() schema.StringAttribute {
				a := stateFilterAttribute("hosts", "state", instanceStatesFilter, true)
				a.MarkdownDescription += " `deleted` returns the instances the platform keeps a record " +
					"of after they're gone, which an ordinary read leaves out: the platform turns its " +
					"`showDeleted` on for that state. Those records are never reaped, so a long-lived " +
					"group can return many."
				return a
			}(),
			"instances": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The instances, sorted by `id`. That order is stable across reads " +
					"but not across a scale, so reach an element with `for_each` rather than " +
					"`instances[0]`.",
				NestedObject: schema.NestedAttributeObject{Attributes: entry},
			},
			// Same positions as `instances`.
			"ids": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Just the ids, for `for_each` and `contains`. Sorted by `id`, at the same positions as `instances`.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *instancesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *instancesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data instancesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	var params k0rdent.ListInstancesParams
	if !data.InstanceGroup.IsNull() && !data.InstanceGroup.IsUnknown() {
		v := data.InstanceGroup.ValueString()
		params.InstanceGroupId = &v
	}
	if !data.NodePool.IsNull() && !data.NodePool.IsUnknown() {
		v := data.NodePool.ValueString()
		params.NodePool = &v
	}
	filter, diags := readObjectFilter(ctx, types.MapNull(types.StringType), types.StringNull(), data.State)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// `deleted` needs no `showDeleted` beside it: the contract turns that on
	// for `inStates=deleted`, since only a deleted instance is in that state.
	if filter.state != "" {
		params.InStates = &[]k0rdent.ListInstancesParamsInStates{k0rdent.ListInstancesParamsInStates(filter.state)}
	}

	// This operation documents only the cursor. A page can be shorter than the
	// size asked for while a token is still set, which byToken already
	// follows: only the token says the list is over.
	instances, ok := listByTokenAt(&resp.Diagnostics, cappedPager(instancePageSize), "instances",
		func(token *string, size *int) (page[k0rdent.Instance], error) {
			params.PageToken, params.PageSize = token, size
			got, err := d.meta.Client.ListInstancesWithResponse(ctx, target.region, target.project, &params)
			if err != nil {
				return page[k0rdent.Instance]{}, err
			}
			if got.JSON200 == nil {
				return page[k0rdent.Instance]{}, unreadPage(got.HTTPResponse, got.Body)
			}
			return page[k0rdent.Instance]{items: got.JSON200.Instances, next: got.JSON200.NextPageToken}, nil
		})
	if !ok {
		return
	}

	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)
	data.Instances = make([]instanceEntryModel, 0, len(instances))
	for _, in := range instances {
		if !filter.keeps(filterSubject{state: stateString(in.State)}) {
			continue
		}
		data.Instances = append(data.Instances, instanceEntryOf(in))
	}

	// The operation promises no order, so a dependent resource reading
	// `instances[i]` would otherwise see a different host on a reread.
	sort.SliceStable(data.Instances, func(i, j int) bool {
		return data.Instances[i].ID.ValueString() < data.Instances[j].ID.ValueString()
	})

	ids := make([]string, 0, len(data.Instances))
	for _, in := range data.Instances {
		if !in.ID.IsNull() {
			ids = append(ids, in.ID.ValueString())
		}
	}
	list, diags := types.ListValueFrom(ctx, types.StringType, ids)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.IDs = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
