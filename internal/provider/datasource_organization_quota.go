package provider

import (
	"context"
	"fmt"
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
	_ datasource.DataSource              = (*organizationQuotaDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*organizationQuotaDataSource)(nil)
	_ datasource.DataSource              = (*projectQuotaDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*projectQuotaDataSource)(nil)
)

const (
	organizationQuotaDataSourceSuffix = "organization_quota"
	tenantQuotaDataSourceSuffix       = "tenant_quota"
	projectQuotaDataSourceSuffix      = "project_quota"
)

// The names of the organization's quota scope. The platform called it
// `tenant` until its quota API renamed it `organization`, and a landscape can
// run either. The organization quota reports one name whichever the platform
// sends, and the project quota reports it as sent (asSent), so the rename
// reaches a project quota's users from the platform, not the provider.
const (
	organizationScope = "organization"
	tenantScope       = "tenant"
	asSent            = ""
)

// quotaScopeAs spells a quota scope the platform sent: orgScope when it names
// the organization in either spelling, and otherwise, or when orgScope is
// asSent, as sent.
func quotaScopeAs(scope, orgScope string) string {
	if orgScope != asSent && (scope == organizationScope || scope == tenantScope) {
		return orgScope
	}
	return scope
}

func newOrganizationQuotaDataSource() datasource.DataSource {
	return &organizationQuotaDataSource{suffix: organizationQuotaDataSourceSuffix, orgScope: organizationScope}
}

// newTenantQuotaDataSource is the organization quota under its name from
// before the rename. It keeps reporting the organization's scope as `tenant`,
// so a configuration written for it reads the same on either platform.
func newTenantQuotaDataSource() datasource.DataSource {
	return &organizationQuotaDataSource{suffix: tenantQuotaDataSourceSuffix, orgScope: tenantScope,
		deprecation: fmt.Sprintf("Use `%s` instead, which names the organization's scope `organization`. "+
			"`%s` is removed in v1.0.0.", ResourceName(organizationQuotaDataSourceSuffix),
			ResourceName(tenantQuotaDataSourceSuffix))}
}

func newProjectQuotaDataSource() datasource.DataSource { return &projectQuotaDataSource{} }

type organizationQuotaDataSource struct {
	meta        *Meta
	suffix      string
	orgScope    string
	deprecation string
}
type projectQuotaDataSource struct{ meta *Meta }

type organizationQuotaModel struct {
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

func (d *organizationQuotaDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(d.suffix)
}

func (d *projectQuotaDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(projectQuotaDataSourceSuffix)
}

// quotaAttributes are the attributes both quota views share, with the
// organization's scope named orgScope, or as the platform sends it when
// orgScope is asSent. Read-only because quota profiles and assignments are
// operator-facing.
func quotaAttributes(orgScope string) map[string]schema.Attribute {
	binding := "`" + orgScope + "` or `project`"
	first := "`" + orgScope + "`"
	scope := "`" + orgScope + "` for the organization, `project` for a project."
	if orgScope == asSent {
		binding = "`tenant`, `organization` or `project`"
		first = "the organization"
		scope = "`tenant` or `organization` for the organization, as the platform sends it, and " +
			"`project` for a project."
	}
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
						MarkdownDescription: "The scope `headroom` came from, " + binding + ". " +
							"Null exactly when `headroom` is.",
					},
					"scopes": schema.ListNestedAttribute{
						Computed: true,
						MarkdownDescription: "The scopes that apply, broadest first, in the platform's " +
							"order: " + first + " always, then `project` when the project has an " +
							"assignment of its own.",
						NestedObject: schema.NestedAttributeObject{
							Attributes: map[string]schema.Attribute{
								"scope": schema.StringAttribute{
									Computed:            true,
									MarkdownDescription: scope,
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

func (d *organizationQuotaDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	description := "What an organization is allowed and what it has used, per quota dimension. " +
		"There is no capacity read on the public API, so this is a limit, not availability."
	if d.deprecation != "" {
		description = "~> **Deprecated:** " + d.deprecation + "\n\n" + description
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: description,
		DeprecationMessage:  d.deprecation,
		Attributes:          quotaAttributes(d.orgScope),
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *projectQuotaDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := quotaAttributes(asSent)
	attrs["project_id"] = schema.StringAttribute{
		Required:            true,
		MarkdownDescription: "Project to read, within `organization_id`.",
		Validators:          []validator.String{urlSafeIdentifier{}},
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "A project's quota: the organization's scope and, when the project has " +
			"an assignment of its own, the project's. The two are independent gates, so " +
			"`headroom` is the smaller of them and `binding_scope` names which.\n\n" +
			"The organization's scope reads as the platform sends it: `tenant` before its quota " +
			"API renamed it, `organization` after. `" +
			ResourceName(organizationQuotaDataSourceSuffix) + "` always reports `organization`.",
		Attributes: attrs,
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *organizationQuotaDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *projectQuotaDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *organizationQuotaDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data organizationQuotaModel
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
		addAPIError(&resp.Diagnostics, "read the organization quota", httpResp, body, err)
		return
	}
	data.Profile, data.Dimensions = quotaViewFrom(ctx, *got.JSON200, d.orgScope, &resp.Diagnostics)
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
	data.Profile, data.Dimensions = quotaViewFrom(ctx, *got.JSON200, asSent, &resp.Diagnostics)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// quotaViewFrom maps a quota view, naming the organization's scope orgScope,
// or as sent when orgScope is asSent.
// It reads `scopes`, `headroom` and `enforced`, never the deprecated top-level
// `limit`, `used` and `remaining`, which describe the organization alone and
// read 0 for an unmeasured dimension.
func quotaViewFrom(ctx context.Context, view k0rdent.TenantQuotaView, orgScope string, diags *diag.Diagnostics) (types.String, types.Map) {
	profile := types.StringNull()
	if view.Profile != nil {
		profile = types.StringValue(view.Profile.DisplayName)
	}

	dimensions := map[string]quotaDimensionModel{}
	for _, row := range view.Dimensions {
		scopes := make([]quotaScopeModel, 0, len(row.Scopes))
		for _, s := range row.Scopes {
			scopes = append(scopes, quotaScopeModel{
				Scope:     types.StringValue(quotaScopeAs(string(s.Scope), orgScope)),
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
			binding = types.StringValue(quotaScopeAs(string(*row.BindingScope), orgScope))
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
