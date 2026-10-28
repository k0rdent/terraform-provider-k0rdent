package provider

import (
	"context"
	"fmt"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ datasource.DataSource              = (*vpcsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*vpcsDataSource)(nil)
	_ datasource.DataSource              = (*vpcDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*vpcDataSource)(nil)
)

const (
	vpcsDataSourceSuffix = "vpcs"
	vpcDataSourceSuffix  = "vpc"
)

func newVPCsDataSource() datasource.DataSource { return &vpcsDataSource{} }
func newVPCDataSource() datasource.DataSource  { return &vpcDataSource{} }

type vpcsDataSource struct{ meta *Meta }
type vpcDataSource struct{ meta *Meta }

// Read-only: there is no createVPC, updateVPC or deleteVPC. A VPC is
// created by the cluster or instance-group workflow and goes with its
// owner. The security-group binding is the one tenant-writable part.
type vpcEntryModel struct {
	ID             types.String `tfsdk:"id"`
	DisplayName    types.String `tfsdk:"display_name"`
	Backend        types.String `tfsdk:"backend"`
	OwnerKind      types.String `tfsdk:"owner_kind"`
	OwnerID        types.String `tfsdk:"owner_id"`
	SecurityGroups types.List   `tfsdk:"security_groups"`
	State          types.String `tfsdk:"state"`
	SelfLink       types.String `tfsdk:"self_link"`
	UFOName        types.String `tfsdk:"ufo_cr_name"`
}

type vpcsModel struct {
	Region           types.String    `tfsdk:"region"`
	Project          types.String    `tfsdk:"project"`
	DisplayNameRegex types.String    `tfsdk:"display_name_regex"`
	State            types.String    `tfsdk:"state"`
	VPCs             []vpcEntryModel `tfsdk:"vpcs"`
	IDs              types.List      `tfsdk:"ids"`
}

type vpcModel struct {
	Region  types.String `tfsdk:"region"`
	Project types.String `tfsdk:"project"`
	vpcEntryModel
}

// The scope attributes a project-scoped data source declares.
//
// Optional+Computed: the value falls back to the provider block and is written
// back, so a configuration can read the scope it actually got rather than
// having to know what the provider was set to. Nothing is lost by the pair
// here: a data source has no prior state for drift detection to compare
// against.
func dataSourceRegionAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Optional:            true,
		Computed:            true,
		MarkdownDescription: "Region to read. Defaults to the provider's `region`.",
		Validators:          []validator.String{urlSafeIdentifier{}},
	}
}

// See dataSourceRegionAttribute: Optional+Computed for the same reason.
func dataSourceProjectAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Optional:            true,
		Computed:            true,
		MarkdownDescription: "Project to read. Defaults to the provider's `project`.",
		Validators:          []validator.String{urlSafeIdentifier{}},
	}
}

func vpcEntryAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "The VPC's name.",
		},
		"display_name": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Human-friendly name, for display only.",
		},
		"backend": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "Fabric backend serving the VPC. Only a `nico` VPC can carry " +
				"security groups; on any other backend `security_groups` is always empty and " +
				"attaching one is refused.",
		},
		"owner_kind": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "What created this VPC: a cluster or an instance group. " +
				"Deleting that owner deletes the VPC.",
		},
		"owner_id": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "UID of the owning cluster or instance group.",
		},
		// A List, and ordered. The contract is explicit that order is
		// significant and round-trips what was written, so a Set would discard
		// information the fabric acts on.
		"security_groups": schema.ListAttribute{
			Computed:    true,
			ElementType: types.StringType,
			MarkdownDescription: "Ids of the attached security groups, in the order they were last " +
				"set. Never empty on a `nico` VPC: clearing the binding reinstates the platform's " +
				"DEFAULT group rather than leaving none.",
		},
		"state": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "Lifecycle state. `updating` means a security-group change has " +
				"been accepted and `security_groups` is what the API will converge on, not " +
				"necessarily what the fabric enforces yet.",
		},
		"self_link": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Platform-defined reference to this VPC.",
		},
		"ufo_cr_name": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "The VPC's fabric name. This is the value a " +
				"cluster-provisioned `" + ResourceName(networkResourceSuffix) + "` reports as `vpc`.",
		},
	}
}

func (d *vpcsDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(vpcsDataSourceSuffix)
}

func (d *vpcsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every VPC in a project.\n\n" +
			"A VPC comes with a cluster or an instance group and is never created directly. To " +
			"name a cluster's VPC in a resource, use the cluster's `vpc` attribute rather than " +
			"this data source: a data source that depends on the cluster is read at apply " +
			"whenever the cluster has a pending change, which makes the VPC id unknown and " +
			"replaces everything that names it.",
		Attributes: map[string]schema.Attribute{
			"region":             dataSourceRegionAttribute(),
			"project":            dataSourceProjectAttribute(),
			"display_name_regex": displayNameRegexAttribute("VPCs"),
			"state":              stateFilterAttribute("VPCs", "state", vpcStatesFilter, false),
			// A List because a catalog has to be indexable and a Set is not.
			// The contract promises no order for any list operation, so the
			// provider imposes one.
			"vpcs": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The VPCs. Sorted by `id`. That order is stable when the " +
					"platform reorders its response but not when a VPC is added, since a new " +
					"`id` sorting first shifts every index: prefer `for_each` over `ids` to " +
					"`vpcs[0]`.",
				NestedObject: schema.NestedAttributeObject{Attributes: vpcEntryAttributes()},
			},
			"ids": schema.ListAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Just the ids, at the same positions as `vpcs`. Sorted by `id`.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *vpcsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *vpcsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data vpcsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	vpcs, ok := listVPCs(ctx, d.meta, target, k0rdent.ListVPCsParams{}, &resp.Diagnostics)
	if !ok {
		return
	}
	filter, filterDiags := readObjectFilter(ctx, types.MapNull(types.StringType), data.DisplayNameRegex, data.State)
	resp.Diagnostics.Append(filterDiags...)
	if resp.Diagnostics.HasError() {
		return
	}

	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)
	data.VPCs = []vpcEntryModel{}
	ids := []string{}
	// An unordered response would read to a dependent resource as every element
	// having changed. `ids` is built from the same slice, so positions agree.
	sort.SliceStable(vpcs, func(i, j int) bool { return vpcs[i].Id < vpcs[j].Id })
	for _, v := range vpcs {
		if !filter.keeps(filterSubject{displayName: v.DisplayName, state: stateString(v.State)}) {
			continue
		}
		entry, diags := vpcEntryFrom(ctx, v)
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		data.VPCs = append(data.VPCs, entry)
		ids = append(ids, v.Id)
	}

	list, diags := types.ListValueFrom(ctx, types.StringType, ids)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.IDs = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// ownedVPC is the id of the VPC an owner brought with it, found by the owner's
// uid, or null before the workflow has made one.
func ownedVPC(ctx context.Context, meta *Meta, target clusterTarget, params k0rdent.ListVPCsParams) (types.String, diag.Diagnostics) {
	var diags diag.Diagnostics
	vpcs, ok := listVPCs(ctx, meta, target, params, &diags)
	if !ok || len(vpcs) == 0 {
		return types.StringNull(), diags
	}
	sort.Slice(vpcs, func(i, j int) bool { return vpcs[i].Id < vpcs[j].Id })
	return types.StringValue(vpcs[0].Id), diags
}

func (d *vpcDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(vpcDataSourceSuffix)
}

func (d *vpcDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := vpcEntryAttributes()
	attrs["id"] = schema.StringAttribute{
		Required:            true,
		MarkdownDescription: "The VPC's name.",
		Validators:          []validator.String{urlSafeIdentifier{}},
	}
	attrs["region"] = dataSourceRegionAttribute()
	attrs["project"] = dataSourceProjectAttribute()

	resp.Schema = schema.Schema{
		MarkdownDescription: "One VPC, by name.\n\n" +
			"Use `" + ResourceName(vpcsDataSourceSuffix) + "` when the name is not known ahead of time.",
		Attributes: attrs,
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *vpcDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *vpcDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data vpcModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	id := data.ID.ValueString()

	got, err := d.meta.Client.GetVPCWithResponse(ctx, target.region, target.project, id)
	if err != nil {
		addAPIError(&resp.Diagnostics, "read the VPC", nil, nil, err)
		return
	}
	if isNotFound(got.HTTPResponse, got.Body) {
		// A data source that cannot find what it was asked for is an error, not
		// an empty result: the configuration named this VPC and everything
		// downstream of it is about to be planned against nothing.
		resp.Diagnostics.AddError(
			"No such VPC",
			fmt.Sprintf("No VPC named %q in project %q of region %q. A VPC comes with a "+
				"cluster or an instance group, so the owning resource may not have been "+
				"applied yet.", id, target.project, target.region),
		)
		return
	}
	if got.JSON200 == nil {
		addAPIError(&resp.Diagnostics, "read the VPC", got.HTTPResponse, got.Body, nil)
		return
	}

	entry, diags := vpcEntryFrom(ctx, *got.JSON200)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.vpcEntryModel = entry
	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func vpcEntryFrom(ctx context.Context, v k0rdent.VPC) (vpcEntryModel, diag.Diagnostics) {
	groups, diags := types.ListValueFrom(ctx, types.StringType, v.SecurityGroups)

	out := vpcEntryModel{
		ID:             types.StringValue(v.Id),
		DisplayName:    stringOrNull(v.DisplayName),
		Backend:        stringOrNull(v.Backend),
		OwnerKind:      types.StringNull(),
		OwnerID:        types.StringNull(),
		SecurityGroups: groups,
		State:          types.StringNull(),
		SelfLink:       stringOrNull(v.SelfLink),
		UFOName:        stringOrNull(v.UfoCrName),
	}
	if v.OwnerKind != nil {
		out.OwnerKind = types.StringValue(string(*v.OwnerKind))
	}
	if v.OwnerId != nil {
		out.OwnerID = types.StringValue(v.OwnerId.String())
	}
	if v.State != nil {
		out.State = types.StringValue(string(*v.State))
	}
	return out, diags
}
