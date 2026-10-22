package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ datasource.DataSource              = (*tenantQuotaDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*tenantQuotaDataSource)(nil)
	_ datasource.DataSource              = (*projectQuotaDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*projectQuotaDataSource)(nil)
)

const (
	tenantQuotaDataSourceSuffix  = "tenant_quota"
	projectQuotaDataSourceSuffix = "project_quota"
)

func newTenantQuotaDataSource() datasource.DataSource  { return &tenantQuotaDataSource{} }
func newProjectQuotaDataSource() datasource.DataSource { return &projectQuotaDataSource{} }

type tenantQuotaDataSource struct{ meta *Meta }
type projectQuotaDataSource struct{ meta *Meta }

type tenantQuotaModel struct {
	OrganizationID types.String `tfsdk:"organization_id"`
	Profile        types.String `tfsdk:"profile"`
	Dimensions     types.Map    `tfsdk:"dimensions"`
}

type projectQuotaModel struct {
	OrganizationID types.String `tfsdk:"organization_id"`
	ProjectID      types.String `tfsdk:"project_id"`
	Profile        types.String `tfsdk:"profile"`
	Dimensions     types.Map    `tfsdk:"dimensions"`
}

type quotaDimensionModel struct {
	Unit         types.String `tfsdk:"unit"`
	Enforced     types.Bool   `tfsdk:"enforced"`
	OverLimit    types.Bool   `tfsdk:"over_limit"`
	Headroom     types.Int64  `tfsdk:"headroom"`
	BindingScope types.String `tfsdk:"binding_scope"`
	Scopes       types.List   `tfsdk:"scopes"`
}

type quotaScopeModel struct {
	Scope     types.String `tfsdk:"scope"`
	TargetID  types.String `tfsdk:"target_id"`
	Limit     types.Int64  `tfsdk:"limit"`
	Used      types.Int64  `tfsdk:"used"`
	Headroom  types.Int64  `tfsdk:"headroom"`
	OverLimit types.Bool   `tfsdk:"over_limit"`
}

func quotaScopeType() attr.Type {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"scope":      types.StringType,
		"target_id":  types.StringType,
		"limit":      types.Int64Type,
		"used":       types.Int64Type,
		"headroom":   types.Int64Type,
		"over_limit": types.BoolType,
	}}
}

func quotaDimensionType() attr.Type {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"unit":          types.StringType,
		"enforced":      types.BoolType,
		"over_limit":    types.BoolType,
		"headroom":      types.Int64Type,
		"binding_scope": types.StringType,
		"scopes":        types.ListType{ElemType: quotaScopeType()},
	}}
}

func (d *tenantQuotaDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(tenantQuotaDataSourceSuffix)
}

func (d *projectQuotaDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(projectQuotaDataSourceSuffix)
}

// quotaAttributes are the attributes both quota views share. Read-only because
// quota profiles and assignments are operator-facing.
func quotaAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"organization_id": schema.StringAttribute{
			Required: true,
			MarkdownDescription: "Organization to read. The `" +
				ResourceName(projectsDataSourceSuffix) + "` data source reports it per project.",
			Validators: []validator.String{urlSafeIdentifier{}},
		},
		"profile": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "Name of the quota profile assigned at this view's own target. Null " +
				"when that target has no active assignment.",
		},
		"dimensions": schema.MapNestedAttribute{
			Computed: true,
			MarkdownDescription: "One entry per quota dimension that some scope limits, or that is " +
				"enforced and has recorded usage, keyed by dimension name, for example " +
				"`dimensions[\"node_count\"].headroom`.",
			NestedObject: schema.NestedAttributeObject{
				Attributes: map[string]schema.Attribute{
					"unit": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "Unit of every limit, usage and headroom figure in this entry.",
					},
					"enforced": schema.BoolAttribute{
						Computed: true,
						MarkdownDescription: "Whether admission refuses past this dimension's limits. When " +
							"false the limits are recorded but nothing refuses past them, and `used` and " +
							"`headroom` are null at every scope: unmeasured is not zero.",
					},
					"over_limit": schema.BoolAttribute{
						Computed:            true,
						MarkdownDescription: "Whether usage exceeds at least one scope's limit.",
					},
					"headroom": schema.Int64Attribute{
						Computed: true,
						MarkdownDescription: "How much more fits: the smallest headroom across the scopes " +
							"that cap this dimension. Null when no scope caps it or it is not enforced. " +
							"This is a limit, not availability: headroom does not mean the hardware is free.",
					},
					"binding_scope": schema.StringAttribute{
						Computed: true,
						MarkdownDescription: "The scope `headroom` came from, `tenant` or `project`. Null " +
							"exactly when `headroom` is.",
					},
					"scopes": schema.ListNestedAttribute{
						Computed: true,
						MarkdownDescription: "The scopes that apply, broadest first, in the platform's " +
							"order: the organization (`tenant`) always, then the project when it has an " +
							"assignment of its own.",
						NestedObject: schema.NestedAttributeObject{
							Attributes: map[string]schema.Attribute{
								"scope": schema.StringAttribute{
									Computed:            true,
									MarkdownDescription: "`tenant` for the organization, `project` for a project.",
								},
								"target_id": schema.StringAttribute{
									Computed:            true,
									MarkdownDescription: "The organization or project the scope belongs to.",
								},
								"limit": schema.Int64Attribute{
									Computed:            true,
									MarkdownDescription: "The scope's cap. Null when the scope does not cap this dimension.",
								},
								"used": schema.Int64Attribute{
									Computed:            true,
									MarkdownDescription: "Usage recorded at this scope, summed across regions. Null when the dimension is not enforced.",
								},
								"headroom": schema.Int64Attribute{
									Computed:            true,
									MarkdownDescription: "Remaining under this scope's limit. Null when there is no limit or the dimension is not enforced.",
								},
								"over_limit": schema.BoolAttribute{
									Computed:            true,
									MarkdownDescription: "Whether usage exceeds this scope's limit, which happens when a limit is lowered below current usage.",
								},
							},
						},
					},
				},
			},
		},
	}
}

func (d *tenantQuotaDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "What an organization is allowed and what it has used, per quota " +
			"dimension. There is no capacity read on the public API, so this is a limit, not " +
			"availability.",
		Attributes: quotaAttributes(),
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *projectQuotaDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := quotaAttributes()
	attrs["project_id"] = schema.StringAttribute{
		Required:            true,
		MarkdownDescription: "Project to read, within `organization_id`.",
		Validators:          []validator.String{urlSafeIdentifier{}},
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A project's quota: the organization's scope and, when the project has " +
			"an assignment of its own, the project's. The two are independent gates, so " +
			"`headroom` is the smaller of them and `binding_scope` names which.",
		Attributes: attrs,
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *tenantQuotaDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *projectQuotaDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *tenantQuotaDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data tenantQuotaModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() || !providerConfigured(d.meta, &resp.Diagnostics) {
		return
	}
	got, err := d.meta.Client.GetTenantQuotaWithResponse(ctx, data.OrganizationID.ValueString())
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(&resp.Diagnostics, "read the tenant quota", httpResp, body, err)
		return
	}
	data.Profile, data.Dimensions = quotaViewFrom(ctx, *got.JSON200, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (d *projectQuotaDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data projectQuotaModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() || !providerConfigured(d.meta, &resp.Diagnostics) {
		return
	}
	got, err := d.meta.Client.GetProjectQuotaWithResponse(ctx, data.OrganizationID.ValueString(),
		data.ProjectID.ValueString())
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(&resp.Diagnostics, "read the project quota", httpResp, body, err)
		return
	}
	data.Profile, data.Dimensions = quotaViewFrom(ctx, *got.JSON200, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// quotaViewFrom maps a quota view. It reads `scopes`, `headroom` and
// `enforced`, never the deprecated top-level `limit`, `used` and `remaining`,
// which describe the organization alone and read 0 for an unmeasured dimension.
func quotaViewFrom(ctx context.Context, view k0rdent.TenantQuotaView, diags *diag.Diagnostics) (types.String, types.Map) {
	profile := types.StringNull()
	if view.Profile != nil {
		profile = types.StringValue(view.Profile.DisplayName)
	}

	dimensions := map[string]quotaDimensionModel{}
	for _, row := range view.Dimensions {
		scopes := make([]quotaScopeModel, 0, len(row.Scopes))
		for _, s := range row.Scopes {
			scopes = append(scopes, quotaScopeModel{
				Scope:     types.StringValue(string(s.Scope)),
				TargetID:  types.StringValue(s.TargetId),
				Limit:     int64OrNull(s.Limit),
				Used:      int64OrNull(s.Used),
				Headroom:  int64OrNull(s.Headroom),
				OverLimit: types.BoolValue(s.OverLimit),
			})
		}
		list, d := types.ListValueFrom(ctx, quotaScopeType(), scopes)
		diags.Append(d...)

		binding := types.StringNull()
		if row.BindingScope != nil {
			binding = types.StringValue(string(*row.BindingScope))
		}
		dimensions[row.Dimension] = quotaDimensionModel{
			Unit:         types.StringValue(row.Unit),
			Enforced:     types.BoolValue(row.Enforced),
			OverLimit:    types.BoolValue(row.OverLimit),
			Headroom:     int64OrNull(row.Headroom),
			BindingScope: binding,
			Scopes:       list,
		}
	}
	m, d := types.MapValueFrom(ctx, quotaDimensionType(), dimensions)
	diags.Append(d...)
	return profile, m
}
