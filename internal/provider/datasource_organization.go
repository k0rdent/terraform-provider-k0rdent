package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*organizationDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*organizationDataSource)(nil)
)

const organizationDataSourceSuffix = "organization"

func newOrganizationDataSource() datasource.DataSource { return &organizationDataSource{} }

// organizationDataSource reads one organization by id, through getOrganization.
//
// It is the singular beside `organizations` for the same reason `project` sits
// beside `projects`: a configuration that already knows the id wants to check
// it names a real organization, and whether that organization is `active`,
// before creating a project in it, without filtering a list for one row.
//
// It does not carry the contact email or the member and invitation counts.
// The email is a person's address, and the counts move with people joining,
// which nothing in a configuration acts on.
type organizationDataSource struct{ meta *Meta }

type organizationDataSourceModel struct {
	ID types.String `tfsdk:"id"`

	DisplayName types.String `tfsdk:"display_name"`
	Description types.String `tfsdk:"description"`
	Status      types.String `tfsdk:"status"`
	CreatedAt   types.String `tfsdk:"created_at"`
	SelfLink    types.String `tfsdk:"self_link"`
}

func (d *organizationDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(organizationDataSourceSuffix)
}

func (d *organizationDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One organization, read by id.\n\n" +
			"Use it to check that the `organization_id` a `" + ResourceName(projectResourceSuffix) +
			"` names is a real organization, and that it is `active`, before a project is created " +
			"in it. `" + ResourceName(organizationsDataSourceSuffix) + "` is where to find the id.\n\n" +
			"Like `" + ResourceName(projectDataSourceSuffix) + "` it takes no `region` and no " +
			"`project`: an organization lives on the global control-plane tier.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The organization's id.",
				Validators:          idValidators(),
			},
			"display_name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Human-friendly name, if the organization has one.",
			},
			"description": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Free-form text describing it, for people to read. Null for an " +
					"organization created before the platform required a description.",
			},
			"status": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Lifecycle status. `active` is usable. `suspended` and `disabled` " +
					"are states the platform holds an organization in, `provisioning` and `failed` " +
					"belong to its creation, and `deleting` means a teardown has been accepted.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the organization was created, as RFC 3339.",
			},
			"self_link": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Platform-defined reference to this organization.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *organizationDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *organizationDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data organizationDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !providerConfigured(d.meta, &resp.Diagnostics) {
		return
	}

	got, err := d.meta.Client.GetOrganizationWithResponse(ctx, data.ID.ValueString())
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		// A 404 is an error here: a data source naming something absent is a
		// broken configuration, not drift to reconcile.
		addAPIError(&resp.Diagnostics, "read the organization", httpResp, body, err)
		return
	}
	o := *got.JSON200

	data.DisplayName = stringOrNull(o.DisplayName)
	data.Description = textOrNull(o.Description)
	data.Status = enumOrNull(o.Status)
	data.CreatedAt = timeOrNull(o.CreatedAt)
	data.SelfLink = stringOrNull(o.SelfLink)

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
