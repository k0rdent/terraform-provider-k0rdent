package provider

import (
	"context"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*networksDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*networksDataSource)(nil)
	_ datasource.DataSource              = (*subnetsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*subnetsDataSource)(nil)
)

const (
	networksDataSourceSuffix = "networks"
	subnetsDataSourceSuffix  = "subnets"
)

func newNetworksDataSource() datasource.DataSource { return &networksDataSource{} }
func newSubnetsDataSource() datasource.DataSource  { return &subnetsDataSource{} }

type networksDataSource struct{ meta *Meta }
type subnetsDataSource struct{ meta *Meta }

// These exist for the half of a project's networking that Terraform does not
// own: the networks and subnets a cluster created from its ClusterType's
// schema. Those carry a `cluster_id`, cannot be written through the tenant API,
// and are invisible to a configuration that only knows about the resources it
// created. A module that has to place a workload next to a cluster's own pod
// subnet has no other way to find it.

type networkSummaryModel struct {
	ID          types.String `tfsdk:"id"`
	Type        types.String `tfsdk:"type"`
	DisplayName types.String `tfsdk:"display_name"`
	Tags        types.Map    `tfsdk:"tags"`
	State       types.String `tfsdk:"state"`
	VPC         types.String `tfsdk:"vpc"`
	ClusterID   types.String `tfsdk:"cluster_id"`
	SelfLink    types.String `tfsdk:"self_link"`
}

type networksModel struct {
	Region           types.String          `tfsdk:"region"`
	Project          types.String          `tfsdk:"project"`
	OnlyTenantOwned  types.Bool            `tfsdk:"only_tenant_owned"`
	Tags             types.Map             `tfsdk:"tags"`
	DisplayNameRegex types.String          `tfsdk:"display_name_regex"`
	State            types.String          `tfsdk:"state"`
	Networks         []networkSummaryModel `tfsdk:"networks"`
	IDs              types.List            `tfsdk:"ids"`
}

type subnetSummaryModel struct {
	ID           types.String `tfsdk:"id"`
	DisplayName  types.String `tfsdk:"display_name"`
	Tags         types.Map    `tfsdk:"tags"`
	CIDR         types.String `tfsdk:"cidr"`
	Gateway      types.String `tfsdk:"gateway"`
	PrefixLength types.Int64  `tfsdk:"prefix_length"`
	AddressPool  types.String `tfsdk:"address_pool"`
	State        types.String `tfsdk:"state"`
	ClusterID    types.String `tfsdk:"cluster_id"`
	SelfLink     types.String `tfsdk:"self_link"`
}

type subnetsModel struct {
	Region           types.String         `tfsdk:"region"`
	Project          types.String         `tfsdk:"project"`
	Network          types.String         `tfsdk:"network"`
	Tags             types.Map            `tfsdk:"tags"`
	DisplayNameRegex types.String         `tfsdk:"display_name_regex"`
	State            types.String         `tfsdk:"state"`
	Subnets          []subnetSummaryModel `tfsdk:"subnets"`
	CIDRs            types.List           `tfsdk:"cidrs"`
}

func (d *networksDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(networksDataSourceSuffix)
}

func (d *networksDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every network in a project, including the ones a cluster created " +
			"for itself.\n\n" +
			"A cluster-provisioned network carries a `cluster_id` and cannot be written through " +
			"the tenant API. This is the only way a configuration can see one.",
		Attributes: map[string]schema.Attribute{
			"region":  dataSourceRegionAttribute(),
			"project": dataSourceProjectAttribute(),
			"only_tenant_owned": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Return only the networks with no `cluster_id`, which are the " +
					"ones a `" + ResourceName(networkResourceSuffix) + "` can manage. Filtered in " +
					"the provider: the list operation takes no filter.",
			},
			"tags":               tagsFilterAttribute("networks"),
			"display_name_regex": displayNameRegexAttribute("networks"),
			"state":              stateFilterAttribute("networks", "state", networkStatesFilter, false),
			// A List because a catalog has to be indexable and a Set is not.
			// The contract promises no order for any list operation, so Read sorts
			// before writing to state.
			"networks": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The networks, sorted by `id`, which is the network's name. " +
					"That order is stable when the platform reorders them but not when one is added, " +
					"so reach an element with `for_each` rather than `networks[0]`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":           schema.StringAttribute{Computed: true, MarkdownDescription: "The network's name."},
						"type":         schema.StringAttribute{Computed: true, MarkdownDescription: "`evpn`, `infiniband` or `nvlink`."},
						"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-friendly name, for display only."},
						"tags":         listedTagsAttribute(),
						"state":        schema.StringAttribute{Computed: true, MarkdownDescription: "Lifecycle state as last read from the platform."},
						"vpc":          schema.StringAttribute{Computed: true, MarkdownDescription: "The fabric VPC this network attaches to, on a cluster-provisioned network."},
						"cluster_id": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "UID of the cluster that provisioned it. Null on a " +
								"network a tenant created, set on one the cluster lifecycle owns.",
						},
						"self_link": schema.StringAttribute{Computed: true, MarkdownDescription: "Platform-defined reference to this object."},
					},
				},
			},
			// Same positions as `networks`: both are built from one sorted slice.
			"ids": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Just the ids, for `for_each` and `contains`. Sorted by `id`, at the same positions as `networks`.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *networksDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *networksDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data networksModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	networks, ok := listNetworks(ctx, d.meta, target, &resp.Diagnostics)
	if !ok {
		return
	}

	onlyTenant := !data.OnlyTenantOwned.IsNull() && data.OnlyTenantOwned.ValueBool()
	filter, diags := readObjectFilter(ctx, data.Tags, data.DisplayNameRegex, data.State)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)
	data.Networks = []networkSummaryModel{}
	for _, n := range networks {
		view, err := networkViewOf(n)
		if err != nil {
			// A type this provider does not recognise is reported rather than
			// skipped: a silently shorter list is a wrong answer, not a partial
			// one. Same reasoning as networkViewOf's own default branch.
			resp.Diagnostics.AddError("A network in this project could not be read", redactDiagnosticText(err.Error()))
			return
		}
		if onlyTenant && view.clusterID != nil {
			continue
		}
		tags := tagAnswerOf(view.tags)
		if !filter.keeps(filterSubject{tags: tags.tags, displayName: view.displayName, state: view.state}) {
			continue
		}
		entry := networkSummaryModel{
			ID:          types.StringValue(view.id),
			Type:        types.StringValue(view.kind),
			DisplayName: stringOrNull(view.displayName),
			Tags:        tags.forList(),
			State:       types.StringNull(),
			VPC:         stringOrNull(view.vpc),
			ClusterID:   stringOrNull(view.clusterID),
			SelfLink:    stringOrNull(view.selfLink),
		}
		if view.state != "" {
			entry.State = types.StringValue(view.state)
		}
		data.Networks = append(data.Networks, entry)
	}

	// A fixed order, so that a dependent resource reading `networks[i]` gets the
	// same network on every read: the list operation defines no order of its own.
	sort.SliceStable(data.Networks, func(i, j int) bool {
		return data.Networks[i].ID.ValueString() < data.Networks[j].ID.ValueString()
	})

	ids := make([]string, 0, len(data.Networks))
	for _, n := range data.Networks {
		ids = append(ids, n.ID.ValueString())
	}

	list, diags := types.ListValueFrom(ctx, types.StringType, ids)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.IDs = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (d *subnetsDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(subnetsDataSourceSuffix)
}

func (d *subnetsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every subnet inside one network, including the ones a cluster " +
			"created.\n\n" +
			"This is how a configuration learns the CIDR a cluster was given, which is the value " +
			"a firewall rule or a peering has to be written against.",
		Attributes: map[string]schema.Attribute{
			"region":  dataSourceRegionAttribute(),
			"project": dataSourceProjectAttribute(),
			"network": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the network to read.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"tags":               tagsFilterAttribute("subnets"),
			"display_name_regex": displayNameRegexAttribute("subnets"),
			"state":              stateFilterAttribute("subnets", "state", subnetStatesFilter, false),
			// A List for the same reason as every other catalog here.
			"subnets": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The subnets, sorted by `id`, which is the subnet's name. " +
					"That order is stable when the platform reorders them but not when one is added, " +
					"so reach an element with `for_each` rather than `subnets[0]`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":            schema.StringAttribute{Computed: true, MarkdownDescription: "The subnet's name."},
						"display_name":  schema.StringAttribute{Computed: true, MarkdownDescription: "Human-friendly name, for display only."},
						"tags":          listedTagsAttribute(),
						"cidr":          schema.StringAttribute{Computed: true, MarkdownDescription: "The allocated block. Empty until the subnet is `active`."},
						"gateway":       schema.StringAttribute{Computed: true, MarkdownDescription: "Resolved gateway address, where one was allocated."},
						"prefix_length": schema.Int64Attribute{Computed: true, MarkdownDescription: "Allocation prefix length."},
						"address_pool": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "The pool the CIDR came from. Null on a " +
								"cluster-provisioned subnet, whose CIDR came from the cluster " +
								"lifecycle rather than a pool the tenant named.",
						},
						"state":      schema.StringAttribute{Computed: true, MarkdownDescription: "Lifecycle state as last read from the platform."},
						"cluster_id": schema.StringAttribute{Computed: true, MarkdownDescription: "UID of the cluster that provisioned it, if any."},
						"self_link":  schema.StringAttribute{Computed: true, MarkdownDescription: "Platform-defined reference to this object."},
					},
				},
			},
			// Same positions as `subnets`. The one value a caller almost always
			// wants, lifted so a firewall rule can be written without a `for`.
			"cidrs": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Just the allocated blocks, sorted by the subnet's `id`, at the same positions as `subnets`.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *subnetsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *subnetsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data subnetsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	network := data.Network.ValueString()

	subnets, ok := listSubnets(ctx, d.meta, target, network, &resp.Diagnostics)
	if !ok {
		return
	}

	filter, diags := readObjectFilter(ctx, data.Tags, data.DisplayNameRegex, data.State)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)
	data.Subnets = []subnetSummaryModel{}
	for _, s := range subnets {
		tags := tagAnswerOfRequired(s.Tags)
		if !filter.keeps(filterSubject{tags: tags.tags, displayName: s.DisplayName, state: stateString(s.State)}) {
			continue
		}
		entry := subnetSummaryModel{
			ID:           types.StringValue(deref(s.Id)),
			DisplayName:  stringOrNull(s.DisplayName),
			Tags:         tags.forList(),
			CIDR:         stringOrNull(s.Cidr),
			Gateway:      stringOrNull(s.Gateway),
			PrefixLength: types.Int64Value(int64(s.PrefixLength)),
			AddressPool:  stringOrNull(s.AddressPool),
			State:        types.StringNull(),
			ClusterID:    stringOrNull(s.ClusterId),
			SelfLink:     stringOrNull(s.SelfLink),
		}
		if s.State != nil {
			entry.State = types.StringValue(string(*s.State))
		}
		data.Subnets = append(data.Subnets, entry)
	}

	// A fixed order, so that `subnets[i]` and `cidrs[i]` keep naming the same
	// subnet on every read: the list operation defines no order of its own.
	sort.SliceStable(data.Subnets, func(i, j int) bool {
		return data.Subnets[i].ID.ValueString() < data.Subnets[j].ID.ValueString()
	})

	cidrs := make([]string, 0, len(data.Subnets))
	for _, s := range data.Subnets {
		cidrs = append(cidrs, s.CIDR.ValueString())
	}

	list, diags := types.ListValueFrom(ctx, types.StringType, cidrs)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.CIDRs = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
