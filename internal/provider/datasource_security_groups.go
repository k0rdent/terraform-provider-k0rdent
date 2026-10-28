package provider

import (
	"context"
	"net/http"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ datasource.DataSource              = (*securityGroupsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*securityGroupsDataSource)(nil)
	_ datasource.DataSource              = (*effectiveSecurityGroupsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*effectiveSecurityGroupsDataSource)(nil)
	_ datasource.DataSource              = (*vpcPeeringsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*vpcPeeringsDataSource)(nil)
)

const (
	securityGroupsDataSourceSuffix          = "security_groups"
	effectiveSecurityGroupsDataSourceSuffix = "effective_security_groups"
	vpcPeeringsDataSourceSuffix             = "vpc_peerings"
)

func newSecurityGroupsDataSource() datasource.DataSource { return &securityGroupsDataSource{} }
func newEffectiveSecurityGroupsDataSource() datasource.DataSource {
	return &effectiveSecurityGroupsDataSource{}
}
func newVPCPeeringsDataSource() datasource.DataSource { return &vpcPeeringsDataSource{} }

type securityGroupsDataSource struct{ meta *Meta }
type effectiveSecurityGroupsDataSource struct{ meta *Meta }
type vpcPeeringsDataSource struct{ meta *Meta }

type securityGroupSummaryModel struct {
	ID          types.String `tfsdk:"id"`
	DisplayName types.String `tfsdk:"display_name"`
	Tags        types.Map    `tfsdk:"tags"`
	RuleCount   types.Int64  `tfsdk:"rule_count"`
	OwnerKind   types.String `tfsdk:"owner_kind"`
	State       types.String `tfsdk:"state"`
	SelfLink    types.String `tfsdk:"self_link"`
}

type securityGroupsModel struct {
	Region           types.String                `tfsdk:"region"`
	Project          types.String                `tfsdk:"project"`
	OnlyTenantOwned  types.Bool                  `tfsdk:"only_tenant_owned"`
	Tags             types.Map                   `tfsdk:"tags"`
	DisplayNameRegex types.String                `tfsdk:"display_name_regex"`
	State            types.String                `tfsdk:"state"`
	SecurityGroups   []securityGroupSummaryModel `tfsdk:"security_groups"`
	IDs              types.List                  `tfsdk:"ids"`
}

type effectiveGroupModel struct {
	ID       types.String `tfsdk:"id"`
	Source   types.String `tfsdk:"source"`
	SourceID types.String `tfsdk:"source_id"`
	Rules    types.Int64  `tfsdk:"rule_count"`
}

type effectiveVPCModel struct {
	ID             types.String          `tfsdk:"id"`
	SecurityGroups []effectiveGroupModel `tfsdk:"security_groups"`
}

type effectiveSecurityGroupsModel struct {
	Region     types.String        `tfsdk:"region"`
	Project    types.String        `tfsdk:"project"`
	ObjectKind types.String        `tfsdk:"object_kind"`
	ObjectID   types.String        `tfsdk:"object_id"`
	VPCs       []effectiveVPCModel `tfsdk:"vpcs"`
}

type peeringSummaryModel struct {
	ID          types.String `tfsdk:"id"`
	DisplayName types.String `tfsdk:"display_name"`
	Tags        types.Map    `tfsdk:"tags"`
	RemoteVPC   types.String `tfsdk:"remote_vpc"`
	State       types.String `tfsdk:"state"`
	SelfLink    types.String `tfsdk:"self_link"`
}

type vpcPeeringsModel struct {
	Region           types.String          `tfsdk:"region"`
	Project          types.String          `tfsdk:"project"`
	VPC              types.String          `tfsdk:"vpc"`
	Tags             types.Map             `tfsdk:"tags"`
	DisplayNameRegex types.String          `tfsdk:"display_name_regex"`
	State            types.String          `tfsdk:"state"`
	Peerings         []peeringSummaryModel `tfsdk:"peerings"`
}

func (d *securityGroupsDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(securityGroupsDataSourceSuffix)
}

func (d *securityGroupsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every security group in a project, including the platform-owned " +
			"default group of each VPC.\n\n" +
			"A group with `owner_kind` set is the platform's. It cannot be modified or deleted, " +
			"and attaching it anywhere but its own VPC is refused.",
		Attributes: map[string]schema.Attribute{
			"region":  dataSourceRegionAttribute(),
			"project": dataSourceProjectAttribute(),
			"only_tenant_owned": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Return only the groups with no `owner_kind`, which are the " +
					"ones a `" + ResourceName(securityGroupResourceSuffix) + "` can manage. " +
					"Filtered in the provider: the list operation takes no filter for it.",
			},
			"tags":               tagsFilterAttribute("groups"),
			"display_name_regex": displayNameRegexAttribute("groups"),
			"state":              stateFilterAttribute("groups", "state", securityGroupStatesFilter, true),
			// A List because a catalog has to be indexable and a Set is not.
			// The contract promises no order for a list operation, so the
			// provider imposes one.
			"security_groups": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The groups, sorted by `id`, which is the group's name and " +
					"never changes. That order is stable under a reorder upstream but not under " +
					"an insertion: a new group sorting ahead of one you read shifts every later " +
					"index, so drive dependent resources with `for_each` rather than " +
					"`security_groups[0]`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":           schema.StringAttribute{Computed: true, MarkdownDescription: "The group's name."},
						"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-friendly name, for display only."},
						"tags":         listedTagsAttribute(),
						"rule_count":   schema.Int64Attribute{Computed: true, MarkdownDescription: "Rules in the group, ingress plus egress."},
						"owner_kind":   schema.StringAttribute{Computed: true, MarkdownDescription: "`vpc` on a platform-owned default group, null on a tenant's own."},
						"state":        schema.StringAttribute{Computed: true, MarkdownDescription: "Lifecycle state as last read from the platform."},
						"self_link":    schema.StringAttribute{Computed: true, MarkdownDescription: "Platform-defined reference to this object."},
					},
				},
			},
			// Same positions as `security_groups`.
			"ids": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Just the ids, for `for_each` and `contains`. Sorted by " +
					"`id`, the same positions as `security_groups`.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *securityGroupsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *securityGroupsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data securityGroupsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	filter, diags := readObjectFilter(ctx, data.Tags, data.DisplayNameRegex, data.State)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The one filter this operation takes. The rest are applied below.
	var params k0rdent.ListSecurityGroupsParams
	if filter.state != "" {
		state := k0rdent.ListSecurityGroupsParamsState(filter.state)
		params.State = &state
	}

	groups, ok := listSecurityGroups(ctx, d.meta, target, params, &resp.Diagnostics)
	if !ok {
		return
	}

	// Sorted once so `security_groups` and `ids` share positions: a reorder
	// upstream would otherwise read as every element having changed.
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Id < groups[j].Id })

	onlyTenant := !data.OnlyTenantOwned.IsNull() && data.OnlyTenantOwned.ValueBool()

	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)
	data.SecurityGroups = []securityGroupSummaryModel{}
	ids := []string{}
	for _, g := range groups {
		if onlyTenant && g.OwnerKind != nil {
			continue
		}
		tags := tagAnswerOfRequired(g.Tags)
		if !filter.keeps(filterSubject{tags: tags.tags, displayName: g.DisplayName, state: stateString(g.State)}) {
			continue
		}
		entry := securityGroupSummaryModel{
			ID:          types.StringValue(g.Id),
			DisplayName: stringOrNull(g.DisplayName),
			Tags:        tags.forList(),
			RuleCount:   int64OrNull(g.RuleCount),
			OwnerKind:   types.StringNull(),
			State:       types.StringNull(),
			SelfLink:    stringOrNull(g.SelfLink),
		}
		if g.OwnerKind != nil {
			entry.OwnerKind = types.StringValue(string(*g.OwnerKind))
		}
		if g.State != nil {
			entry.State = types.StringValue(string(*g.State))
		}
		data.SecurityGroups = append(data.SecurityGroups, entry)
		ids = append(ids, g.Id)
	}

	list, diags := types.ListValueFrom(ctx, types.StringType, ids)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.IDs = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (d *effectiveSecurityGroupsDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(effectiveSecurityGroupsDataSourceSuffix)
}

// This is the only read that answers "what actually filters this thing".
//
// Every other surface reports one binding: the group's own rules, a VPC's
// floor, a cluster's own list. What reaches an interface is the merge of the
// owner's groups and each of its VPCs' floors, deduplicated, in fabric order.
// Reconstructing that from the parts is where a reader gets it wrong, and the
// order is precedence rather than presentation, so getting it wrong changes
// what a person believes is permitted.
func (d *effectiveSecurityGroupsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The security groups that actually filter one object's interfaces, " +
			"merged and in the order the fabric applies them.\n\n" +
			"A `cluster` or `instance_group` read returns the merged set: the object's own " +
			"binding plus the floor of every VPC created with it. A `vpc` read returns only that " +
			"VPC's own binding, because the owner's groups filter traffic there too and are read " +
			"from the owner.\n\n" +
			"An empty list for a VPC means nothing filters interfaces in it. That is not an " +
			"error, and it is always the case on a fabric backend with no security-group " +
			"construct behind it, where neither binding can be enforced.",
		Attributes: map[string]schema.Attribute{
			"region":  dataSourceRegionAttribute(),
			"project": dataSourceProjectAttribute(),
			"object_kind": schema.StringAttribute{
				Required: true,
				MarkdownDescription: enumProse(k0rdent.ListSecurityGroupsEffectiveParamsObjectKindValues()) +
					". Both this and `object_id` are required: the answer is only meaningful for one named object.",
				Validators: []validator.String{oneOfEnum(k0rdent.ListSecurityGroupsEffectiveParamsObjectKindValues())},
			},
			"object_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the object to report on.",
				Validators:          idValidators(),
			},
			// `vpcs` carries no precedence, so it is sorted. The groups inside
			// it do, so they are left as the fabric ordered them.
			"vpcs": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "One block per VPC the object's interfaces sit in, sorted by `id`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{Computed: true, MarkdownDescription: "The VPC's id."},
						"security_groups": schema.ListNestedAttribute{
							Computed: true,
							MarkdownDescription: "The groups reaching interfaces in this VPC, " +
								"owner-bound first and then the VPC's own, deduplicated. Order is " +
								"precedence, so this is the one list here the provider does not " +
								"sort: an earlier group wins over a later one.",
							NestedObject: schema.NestedAttributeObject{
								Attributes: map[string]schema.Attribute{
									"id": schema.StringAttribute{Computed: true, MarkdownDescription: "The group's id."},
									"source": schema.StringAttribute{
										Computed: true,
										MarkdownDescription: "What bound it: `cluster`, `instance_group` or `vpc`. " +
											"This is how you tell a group you attached to the VPC from one the owner carries.",
									},
									"source_id":  schema.StringAttribute{Computed: true, MarkdownDescription: "Id of the object that bound it."},
									"rule_count": schema.Int64Attribute{Computed: true, MarkdownDescription: "Rules in the group."},
								},
							},
						},
					},
				},
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *effectiveSecurityGroupsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *effectiveSecurityGroupsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data effectiveSecurityGroupsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	params := k0rdent.ListSecurityGroupsEffectiveParams{
		ObjectKind: k0rdent.ListSecurityGroupsEffectiveParamsObjectKind(data.ObjectKind.ValueString()),
		ObjectId:   data.ObjectID.ValueString(),
	}
	got, err := d.meta.Client.ListSecurityGroupsEffectiveWithResponse(ctx, target.region, target.project, &params)
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(&resp.Diagnostics, "read the effective security groups", httpResp, body, err)
		return
	}

	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)
	data.VPCs = []effectiveVPCModel{}
	// Sorted on identity because a VPC block's position means nothing, unlike
	// the position of a group inside it.
	sort.SliceStable(got.JSON200.Vpcs, func(i, j int) bool {
		return got.JSON200.Vpcs[i].Id < got.JSON200.Vpcs[j].Id
	})
	for _, v := range got.JSON200.Vpcs {
		entry := effectiveVPCModel{
			ID:             types.StringValue(v.Id),
			SecurityGroups: []effectiveGroupModel{},
		}
		for _, g := range v.SecurityGroups {
			entry.SecurityGroups = append(entry.SecurityGroups, effectiveGroupModel{
				ID:       types.StringValue(g.Id),
				Source:   types.StringValue(string(g.Source)),
				SourceID: types.StringValue(g.SourceId),
				Rules:    types.Int64Value(int64(g.RuleCount)),
			})
		}
		data.VPCs = append(data.VPCs, entry)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (d *vpcPeeringsDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(vpcPeeringsDataSourceSuffix)
}

func (d *vpcPeeringsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every peering declared under one VPC.\n\n" +
			"**Only the ones this project created.** A counterpart's peering naming your VPC as " +
			"its remote is that project's own resource and never appears here, so this cannot " +
			"tell you whether a peering is half-open.",
		Attributes: map[string]schema.Attribute{
			"region":  dataSourceRegionAttribute(),
			"project": dataSourceProjectAttribute(),
			"vpc": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Id of the local VPC whose peerings to read.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"tags":               tagsFilterAttribute("peerings"),
			"display_name_regex": displayNameRegexAttribute("peerings"),
			"state":              stateFilterAttribute("peerings", "state", peeringStatesFilter, false),
			// A List for the same reason as every other catalog here.
			"peerings": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The peerings, sorted by `id`, then `remote_vpc`. Stable " +
					"under a reorder upstream but not under an insertion, so reach a peering " +
					"with `for_each` rather than `peerings[0]`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":           schema.StringAttribute{Computed: true, MarkdownDescription: "The peering's name."},
						"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-friendly name, for display only."},
						"tags":         listedTagsAttribute(),
						"remote_vpc":   schema.StringAttribute{Computed: true, MarkdownDescription: "Reference to the counterpart VPC."},
						"state": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "State of this side only. `active` does not mean traffic flows.",
						},
						"self_link": schema.StringAttribute{Computed: true, MarkdownDescription: "Platform-defined reference to this object."},
					},
				},
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *vpcPeeringsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *vpcPeeringsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data vpcPeeringsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	vpc := data.VPC.ValueString()

	peerings, ok := listVPCPeerings(ctx, d.meta, target, vpc, &resp.Diagnostics)
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
	data.Peerings = []peeringSummaryModel{}
	// `id` is optional in the response, so `remote_vpc` makes the order total.
	sort.SliceStable(peerings, func(i, j int) bool {
		if a, b := deref(peerings[i].Id), deref(peerings[j].Id); a != b {
			return a < b
		}
		return deref(peerings[i].RemoteVpc) < deref(peerings[j].RemoteVpc)
	})
	for _, p := range peerings {
		tags := tagAnswerOf(p.Tags)
		if !filter.keeps(filterSubject{tags: tags.tags, displayName: p.DisplayName, state: stateString(p.State)}) {
			continue
		}
		entry := peeringSummaryModel{
			ID:          types.StringValue(deref(p.Id)),
			DisplayName: stringOrNull(p.DisplayName),
			Tags:        tags.forList(),
			RemoteVPC:   stringOrNull(p.RemoteVpc),
			State:       types.StringNull(),
			SelfLink:    stringOrNull(p.SelfLink),
		}
		if p.State != nil {
			entry.State = types.StringValue(string(*p.State))
		}
		data.Peerings = append(data.Peerings, entry)
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
