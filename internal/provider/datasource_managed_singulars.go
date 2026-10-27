package provider

import (
	"context"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// The singular read-only forms of the four networking resources.
//
// The plural data sources beside these answer a different question. They exist
// for discovery: what networks does this project have, which of them did a
// cluster materialize for itself, which security groups are in play. They
// return summaries, and selecting one means filtering a list.
//
// These answer the question a second state boundary asks, which is not a
// question about discovery at all. It already knows the name. What it needs is
// to say that name and get the thing, without adopting it and without the
// configuration that built it. Filtering a list to find something you can
// already name is a workaround for a data source that does not exist.
//
// They are together in one file because they are one idea repeated four times
// and reading them side by side is how you check that they are consistent.

var (
	_ datasource.DataSource              = (*networkDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*networkDataSource)(nil)
	_ datasource.DataSource              = (*subnetDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*subnetDataSource)(nil)
	_ datasource.DataSource              = (*securityGroupDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*securityGroupDataSource)(nil)
	_ datasource.DataSource              = (*vpcPeeringDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*vpcPeeringDataSource)(nil)
)

const (
	networkDataSourceSuffix       = "network"
	subnetDataSourceSuffix        = "subnet"
	securityGroupDataSourceSuffix = "security_group"
	vpcPeeringDataSourceSuffix    = "vpc_peering"
)

func newNetworkDataSource() datasource.DataSource       { return &networkDataSource{} }
func newSubnetDataSource() datasource.DataSource        { return &subnetDataSource{} }
func newSecurityGroupDataSource() datasource.DataSource { return &securityGroupDataSource{} }
func newVPCPeeringDataSource() datasource.DataSource    { return &vpcPeeringDataSource{} }

type networkDataSource struct{ meta *Meta }
type subnetDataSource struct{ meta *Meta }
type securityGroupDataSource struct{ meta *Meta }
type vpcPeeringDataSource struct{ meta *Meta }

// ---------------------------------------------------------------------------
// Network
// ---------------------------------------------------------------------------

type networkDataSourceModel struct {
	ID      types.String `tfsdk:"id"`
	Region  types.String `tfsdk:"region"`
	Project types.String `tfsdk:"project"`

	Type         types.String `tfsdk:"type"`
	DisplayName  types.String `tfsdk:"display_name"`
	Description  types.String `tfsdk:"description"`
	Tags         types.Map    `tfsdk:"tags"`
	VPC          types.String `tfsdk:"vpc"`
	ClusterID    types.String `tfsdk:"cluster_id"`
	State        types.String `tfsdk:"state"`
	StateMessage types.String `tfsdk:"state_message"`
	SelfLink     types.String `tfsdk:"self_link"`

	VlanID    types.String `tfsdk:"vlan_id"`
	MTU       types.Int64  `tfsdk:"mtu"`
	Partition types.String `tfsdk:"partition"`
	Pkey      types.String `tfsdk:"pkey"`
}

func (d *networkDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(networkDataSourceSuffix)
}

func (d *networkDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One network, by name, read rather than managed.\n\n" +
			"Unlike the resource, this reads a cluster-provisioned network as readily as a tenant " +
			"one. Those carry a `cluster_id`, cannot be written through the tenant API at all, and " +
			"are what a module has to find when it needs to place a workload beside a cluster's own " +
			"subnet.\n\n" +
			"The fabric-specific values are flattened rather than nested by kind, because a reader " +
			"is asking what this network resolved to rather than declaring which kind to build. " +
			"`vlan_id` here is always what the platform allocated.",
		Attributes: map[string]schema.Attribute{
			"id":      schema.StringAttribute{Required: true, MarkdownDescription: "The network's name.", Validators: []validator.String{urlSafeIdentifier{}}},
			"region":  dataSourceRegionAttribute(),
			"project": dataSourceProjectAttribute(),
			"type": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "`evpn`, `infiniband` or `nvlink`. The other attributes depend on it.",
			},
			"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-friendly name, for display only."},
			"description":  schema.StringAttribute{Computed: true, MarkdownDescription: "Free-form text describing it, for people to read."},
			"tags": schema.MapAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "User-defined labels, as key-value pairs.",
			},
			"vpc": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The fabric VPC this network attaches to, on a cluster-provisioned network.",
			},
			"cluster_id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "UID of the cluster that provisioned it. Null on a network a " +
					"tenant created, set on one the cluster lifecycle owns. A network with this set " +
					"cannot be managed by the resource.",
			},
			"state":         schema.StringAttribute{Computed: true, MarkdownDescription: "Lifecycle state as last read from the platform."},
			"state_message": schema.StringAttribute{Computed: true, MarkdownDescription: "The platform's own explanation of the state."},
			"self_link":     schema.StringAttribute{Computed: true, MarkdownDescription: "Platform-defined reference to this object."},
			"vlan_id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The VLAN the platform allocated, on an EVPN network. Never " +
					"`auto`: this is the resolved value, not the request.",
			},
			"mtu":       schema.Int64Attribute{Computed: true, MarkdownDescription: "MTU, on an EVPN network."},
			"partition": schema.StringAttribute{Computed: true, MarkdownDescription: "Partition, on an InfiniBand network."},
			"pkey": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The partition key the platform allocated, on an InfiniBand network.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *networkDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *networkDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data networkDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	got, err := d.meta.Client.GetNetworkWithResponse(ctx, target.region, target.project, data.ID.ValueString())
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(&resp.Diagnostics, "read the network", httpResp, body, err)
		return
	}

	// The same union decoder the resource uses, rather than a second one. A
	// data source and a resource disagreeing about what a network's `type` is
	// would be a very confusing bug to find.
	view, err := networkViewOf(*got.JSON200)
	if err != nil {
		// Local: the response decoded but its union did not, so there is no
		// upstream code to report.
		resp.Diagnostics.AddError("The network could not be read", redactDiagnosticText(err.Error()))
		return
	}

	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)
	data.Type = types.StringValue(view.kind)
	data.ID = types.StringValue(view.id)
	data.DisplayName = stringOrNull(view.displayName)
	data.Description = stringOrNull(view.description)
	data.VPC = stringOrNull(view.vpc)
	data.ClusterID = stringOrNull(view.clusterID)
	data.SelfLink = stringOrNull(view.selfLink)
	data.StateMessage = stringOrNull(view.stateMessage)
	data.State = types.StringNull()
	if view.state != "" {
		data.State = types.StringValue(view.state)
	}

	data.Tags = tagAnswerOf(view.tags).forDataSource()

	data.VlanID, data.MTU = types.StringNull(), types.Int64Null()
	data.Partition, data.Pkey = types.StringNull(), types.StringNull()
	switch {
	case view.evpn != nil:
		data.MTU = int64OrNull(view.evpn.Mtu)
		if view.evpn.Vlan != nil {
			data.VlanID = stringOrNull(view.evpn.Vlan.Id)
		}
	case view.ib != nil:
		data.Partition = stringOrNull(view.ib.Infiniband.Partition)
		data.Pkey = stringOrNull(view.ib.Infiniband.Pkey)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// ---------------------------------------------------------------------------
// Subnet
// ---------------------------------------------------------------------------

type subnetDataSourceModel struct {
	ID      types.String `tfsdk:"id"`
	Network types.String `tfsdk:"network"`
	Region  types.String `tfsdk:"region"`
	Project types.String `tfsdk:"project"`

	CIDR          types.String   `tfsdk:"cidr"`
	Gateway       types.String   `tfsdk:"gateway"`
	PrefixLength  types.Int64    `tfsdk:"prefix_length"`
	IPVersion     types.String   `tfsdk:"ip_version"`
	AddressPool   types.String   `tfsdk:"address_pool"`
	ExcludeRanges []types.String `tfsdk:"exclude_ranges"`
	DisplayName   types.String   `tfsdk:"display_name"`
	Description   types.String   `tfsdk:"description"`
	ClusterID     types.String   `tfsdk:"cluster_id"`
	Tags          types.Map      `tfsdk:"tags"`
	State         types.String   `tfsdk:"state"`
	StateMessage  types.String   `tfsdk:"state_message"`
	StateReason   types.String   `tfsdk:"state_reason"`
	SelfLink      types.String   `tfsdk:"self_link"`
}

func (d *subnetDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(subnetDataSourceSuffix)
}

func (d *subnetDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One subnet, by name, read rather than managed.\n\n" +
			"The CIDR is what this is usually read for: the block is chosen by the platform from " +
			"an address pool, so a configuration that has to route to a subnet somebody else " +
			"created cannot know it any other way.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Required: true, MarkdownDescription: "The subnet's name.", Validators: []validator.String{urlSafeIdentifier{}}},
			"network": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The network the subnet belongs to. Subnet names are unique within a network, not within a project.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"region":        dataSourceRegionAttribute(),
			"project":       dataSourceProjectAttribute(),
			"cidr":          schema.StringAttribute{Computed: true, MarkdownDescription: "The block the platform allocated."},
			"gateway":       schema.StringAttribute{Computed: true, MarkdownDescription: "Gateway address, where the pool is routable and one was allocated."},
			"prefix_length": schema.Int64Attribute{Computed: true, MarkdownDescription: "Prefix length of `cidr`."},
			"ip_version":    schema.StringAttribute{Computed: true, MarkdownDescription: "IP family of the block."},
			"address_pool":  schema.StringAttribute{Computed: true, MarkdownDescription: "The pool the block came from."},
			"exclude_ranges": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Addresses inside the block that are not allocatable.",
			},
			"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-friendly name, for display only."},
			"description":  schema.StringAttribute{Computed: true, MarkdownDescription: "Free-form text describing it, for people to read."},
			"cluster_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "UID of the cluster that provisioned it, on a subnet the cluster lifecycle owns.",
			},
			"tags": schema.MapAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "User-defined labels, as key-value pairs.",
			},
			"state":         schema.StringAttribute{Computed: true, MarkdownDescription: "Lifecycle state as last read from the platform."},
			"state_message": schema.StringAttribute{Computed: true, MarkdownDescription: "The platform's own explanation of the state. Read it, do not branch on it."},
			"state_reason": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Machine-readable cause of a `failed` state, such as " +
					"`INVALID_CONFIGURATION`. Stable enough to branch on. Null when no failure is recorded.",
			},
			"self_link": schema.StringAttribute{Computed: true, MarkdownDescription: "Platform-defined reference to this object."},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *subnetDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *subnetDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data subnetDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	got, err := d.meta.Client.GetSubnetWithResponse(ctx, target.region, target.project,
		data.Network.ValueString(), data.ID.ValueString())
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(&resp.Diagnostics, "read the subnet", httpResp, body, err)
		return
	}
	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)
	resp.Diagnostics.Append(data.fill(ctx, *got.JSON200)...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (data *subnetDataSourceModel) fill(ctx context.Context, s k0rdent.Subnet) diag.Diagnostics {
	var diags diag.Diagnostics
	data.CIDR = stringOrNull(s.Cidr)
	data.Gateway = stringOrNull(s.Gateway)
	data.PrefixLength = types.Int64Value(int64(s.PrefixLength))
	data.AddressPool = stringOrNull(s.AddressPool)
	data.DisplayName = stringOrNull(s.DisplayName)
	data.Description = stringOrNull(s.Description)
	data.ClusterID = stringOrNull(s.ClusterId)
	data.SelfLink = stringOrNull(s.SelfLink)
	data.StateMessage = stringOrNull(s.StateMessage)
	data.StateReason = enumOrNull(s.StateReason)

	data.Tags = tagAnswerOfRequired(s.Tags).forDataSource()

	data.IPVersion = types.StringNull()
	if s.IpVersion != nil {
		data.IPVersion = types.StringValue(string(*s.IpVersion))
	}
	data.State = types.StringNull()
	if s.State != nil {
		data.State = types.StringValue(string(*s.State))
	}

	data.ExcludeRanges = []types.String{}
	for _, r := range s.ExcludeRanges {
		data.ExcludeRanges = append(data.ExcludeRanges, types.StringValue(r))
	}

	return diags
}

// ---------------------------------------------------------------------------
// Security group
// ---------------------------------------------------------------------------

type securityGroupDataSourceModel struct {
	ID      types.String `tfsdk:"id"`
	Region  types.String `tfsdk:"region"`
	Project types.String `tfsdk:"project"`

	DisplayName  types.String `tfsdk:"display_name"`
	Description  types.String `tfsdk:"description"`
	RuleCount    types.Int64  `tfsdk:"rule_count"`
	OwnerKind    types.String `tfsdk:"owner_kind"`
	OwnerID      types.String `tfsdk:"owner_id"`
	State        types.String `tfsdk:"state"`
	StatusReason types.String `tfsdk:"status_reason"`
	SelfLink     types.String `tfsdk:"self_link"`

	Ingress []ruleModel `tfsdk:"ingress"`
	Egress  []ruleModel `tfsdk:"egress"`
}

func (d *securityGroupDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(securityGroupDataSourceSuffix)
}

func (d *securityGroupDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One security group, by name, read rather than managed.\n\n" +
			"`owner_kind` is the attribute to read before doing anything with the result. `vpc` " +
			"marks a VPC's default group, which the platform owns: it cannot be changed or " +
			"deleted, and it can be bound only to the VPC named by `owner_id`. Binding it back to " +
			"that VPC is how to keep it alongside your own groups; binding it anywhere else is " +
			"refused at apply rather than at plan.\n\n" +
			"`ingress` and `egress` are the group's own rules, in the order they're matched. " +
			"A VPC's default group can't be managed, so this is how to read what it allows. " +
			"A group is rarely all that filters an interface: the `" +
			ResourceName("effective_security_groups") + "` data source says which groups reach " +
			"one, and in what order.",
		Attributes: map[string]schema.Attribute{
			"id":           schema.StringAttribute{Required: true, MarkdownDescription: "The group's name.", Validators: []validator.String{urlSafeIdentifier{}}},
			"region":       dataSourceRegionAttribute(),
			"project":      dataSourceProjectAttribute(),
			"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-friendly name, for display only."},
			"description":  schema.StringAttribute{Computed: true, MarkdownDescription: "Free-form text describing it, for people to read."},
			"rule_count":   schema.Int64Attribute{Computed: true, MarkdownDescription: "How many rules the group holds."},
			"owner_kind": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "`vpc` on a VPC's default group, which the platform owns and " +
					"a tenant can't modify through the API. Null on a group a tenant created.",
			},
			"owner_id": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "UID of the owning resource: the VPC's `uid` when `owner_kind` " +
					"is `vpc`. Null on a group a tenant created.",
			},
			"state":         schema.StringAttribute{Computed: true, MarkdownDescription: "Lifecycle state as last read from the platform."},
			"status_reason": schema.StringAttribute{Computed: true, MarkdownDescription: "Why the group is in this state, where the platform gives a reason."},
			"self_link":     schema.StringAttribute{Computed: true, MarkdownDescription: "Platform-defined reference to this object."},
			"ingress": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: ingressRulesDescription,
				NestedObject:        schema.NestedAttributeObject{Attributes: ruleDataSourceAttributes()},
			},
			"egress": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: egressRulesDescription,
				NestedObject:        schema.NestedAttributeObject{Attributes: ruleDataSourceAttributes()},
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

// ruleDataSourceAttributes is the resource's rule shape, read only. It's built
// from ruleAttributes so the names and descriptions can't drift apart.
func ruleDataSourceAttributes() map[string]schema.Attribute {
	out := map[string]schema.Attribute{}
	for name, attr := range ruleAttributes() {
		s, ok := attr.(rschema.StringAttribute)
		if !ok {
			// Every rule field is a string today. Another type needs its own
			// case here, and every schema test fails until it has one.
			panic(fmt.Sprintf("rule attribute %q is a %T, which the data source can't mirror", name, attr))
		}
		out[name] = schema.StringAttribute{Computed: true, MarkdownDescription: s.MarkdownDescription}
	}
	return out
}

func (d *securityGroupDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *securityGroupDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data securityGroupDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	got, err := d.meta.Client.GetSecurityGroupWithResponse(ctx, target.region, target.project, data.ID.ValueString())
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(&resp.Diagnostics, "read the security group", httpResp, body, err)
		return
	}
	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)
	data.fill(*got.JSON200)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (data *securityGroupDataSourceModel) fill(g k0rdent.SecurityGroup) {
	data.DisplayName = stringOrNull(g.DisplayName)
	data.Description = stringOrNull(g.Description)
	data.RuleCount = int64OrNull(g.RuleCount)
	data.SelfLink = stringOrNull(g.SelfLink)
	data.StatusReason = stringOrNull(g.StatusReason)

	data.OwnerKind = enumOrNull(g.OwnerKind)
	data.OwnerID = types.StringNull()
	if g.OwnerId != nil {
		data.OwnerID = types.StringValue(g.OwnerId.String())
	}
	data.State = types.StringNull()
	if g.State != nil {
		data.State = types.StringValue(string(*g.State))
	}

	// The resource's conversion, with no configuration to keep a port spelling
	// from. Never nil: an empty group reads as [], so length() works on it.
	data.Ingress = append([]ruleModel{}, rulesFrom(g.Rules.Ingress, nil)...)
	data.Egress = append([]ruleModel{}, rulesFrom(g.Rules.Egress, nil)...)
}

// ---------------------------------------------------------------------------
// VPC peering
// ---------------------------------------------------------------------------

type vpcPeeringDataSourceModel struct {
	ID      types.String `tfsdk:"id"`
	VPC     types.String `tfsdk:"vpc"`
	Region  types.String `tfsdk:"region"`
	Project types.String `tfsdk:"project"`

	LocalVPC     types.String `tfsdk:"local_vpc"`
	RemoteVPC    types.String `tfsdk:"remote_vpc"`
	DisplayName  types.String `tfsdk:"display_name"`
	Description  types.String `tfsdk:"description"`
	Tags         types.Map    `tfsdk:"tags"`
	State        types.String `tfsdk:"state"`
	StateMessage types.String `tfsdk:"state_message"`
	SelfLink     types.String `tfsdk:"self_link"`
}

func (d *vpcPeeringDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(vpcPeeringDataSourceSuffix)
}

func (d *vpcPeeringDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One VPC peering, by name, read rather than managed.\n\n" +
			"A peering joins two VPCs, and a VPC comes with a cluster or an instance group " +
			"rather than being created directly, so the two sides of a peering routinely belong " +
			"to different configurations. This is how the side that did not create it sees whether " +
			"it is established.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{Required: true, MarkdownDescription: "The peering's name.", Validators: []validator.String{urlSafeIdentifier{}}},
			"vpc": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The VPC the peering is read from. A peering is addressed under one of its two VPCs.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"region":       dataSourceRegionAttribute(),
			"project":      dataSourceProjectAttribute(),
			"local_vpc":    schema.StringAttribute{Computed: true, MarkdownDescription: "The VPC this peering is defined on."},
			"remote_vpc":   schema.StringAttribute{Computed: true, MarkdownDescription: "The VPC on the other side."},
			"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-friendly name, for display only."},
			"description":  schema.StringAttribute{Computed: true, MarkdownDescription: "Free-form text describing it, for people to read."},
			"tags": schema.MapAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "User-defined labels, as key-value pairs.",
			},
			"state":         schema.StringAttribute{Computed: true, MarkdownDescription: "Lifecycle state. `active` means traffic is carried."},
			"state_message": schema.StringAttribute{Computed: true, MarkdownDescription: "The platform's own explanation of the state."},
			"self_link":     schema.StringAttribute{Computed: true, MarkdownDescription: "Platform-defined reference to this object."},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *vpcPeeringDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *vpcPeeringDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data vpcPeeringDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	got, err := d.meta.Client.GetVPCPeeringWithResponse(ctx, target.region, target.project,
		data.VPC.ValueString(), data.ID.ValueString())
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(&resp.Diagnostics, "read the VPC peering", httpResp, body, err)
		return
	}
	p := *got.JSON200

	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)
	data.LocalVPC = stringOrNull(p.LocalVpc)
	data.RemoteVPC = stringOrNull(p.RemoteVpc)
	data.DisplayName = stringOrNull(p.DisplayName)
	data.Description = stringOrNull(p.Description)
	data.SelfLink = stringOrNull(p.SelfLink)
	data.StateMessage = stringOrNull(p.StateMessage)

	data.State = types.StringNull()
	if p.State != nil {
		data.State = types.StringValue(string(*p.State))
	}

	data.Tags = tagAnswerOf(p.Tags).forDataSource()

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
