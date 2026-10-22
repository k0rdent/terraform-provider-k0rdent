package provider

import (
	"context"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*projectDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*projectDataSource)(nil)
)

const projectDataSourceSuffix = "project"

func newProjectDataSource() datasource.DataSource { return &projectDataSource{} }

// projectDataSource reads one project this configuration does not manage.
//
// # Why the plural data source beside it is not enough
//
// `projects` answers a discovery question: what can this token reach. Selecting
// one from it means filtering a list by an attribute you already know, which is
// a workaround for a singular form rather than a use of a plural one, and it
// reads every page of the catalog to return one row.
//
// The question this answers is the other half of the read-only-form
// requirement, the half about crossing a state boundary. A configuration that
// creates clusters inside a project somebody else's configuration owns has to
// name that project and get back something it can assert on. Passing the slug
// across as a bare string works and tells the reader nothing, least of all
// whether the project is still `active`.
//
// # Why `organization_id` is optional
//
// The id alone names one project while only one organization holds it, and
// the lookup refuses an id that more than one does. Given the organization,
// the read is one GET. Without it, the organization is found by searching the
// project list for that exact id.
//
// # What it carries that the resource does not
//
// `created_at`. A read-only form is re-read every run and compared against
// nothing, so it can carry a field that would only ever be noise in a resource
// schema. It is also the field an operator looking at an inherited estate
// actually asks for first.
type projectDataSource struct{ meta *Meta }

type projectDataSourceModel struct {
	ID             types.String `tfsdk:"id"`
	OrganizationID types.String `tfsdk:"organization_id"`
	Region         types.String `tfsdk:"region"`

	DisplayName types.String `tfsdk:"display_name"`
	Description types.String `tfsdk:"description"`
	Status      types.String `tfsdk:"status"`
	CreatedAt   types.String `tfsdk:"created_at"`
	SelfLink    types.String `tfsdk:"self_link"`
}

func (d *projectDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(projectDataSourceSuffix)
}

func (d *projectDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One project, read rather than managed.\n\n" +
			"Use it to name a project another configuration owns, from the state boundary that " +
			"holds the clusters and networks inside it. `status` is what to read on that side: " +
			"a project at `deleting` still answers reads and still holds its name, and a cluster " +
			"created in one is refused.\n\n" +
			"Unlike every other data source here it takes no `region` and no `project`. A project " +
			"lives on the global control-plane tier and is addressed inside an organization. A " +
			"project's `id` is normally held by one organization, so `organization_id` can be left " +
			"out and is then looked up. If more than one organization holds the id, the read is " +
			"refused and `organization_id` has to be set.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Project id. This is the value the provider block's `project` takes.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"organization_id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The organization the project is in. Looked up from `id` when " +
					"left out, which costs a list request instead of one read.",
				Validators: []validator.String{urlSafeIdentifier{}},
			},
			"region": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The region the project is locked to. Every regional resource in " +
					"the project has to be in it, so it's the value their `region`, or the provider's, " +
					"takes. Null for a project made before regions were required.",
			},
			"display_name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Human-friendly name, if the project has one.",
			},
			"description": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Free-form text describing it, for people to read.",
			},
			"status": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Lifecycle status. `active` is usable. `suspended` and `disabled` " +
					"are steady states the platform can hold a project in. `deleting` means a " +
					"teardown has been accepted and the project takes no new work, so a " +
					"configuration that is about to build inside it should check for it rather " +
					"than discover it as a 409 part way through an apply.",
			},
			"created_at": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "When the project was created, as RFC 3339. The resource does not " +
					"carry this and does not need to: it is read every run here and compared " +
					"against nothing.",
			},
			"self_link": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Platform-defined reference to this project.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *projectDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *projectDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data projectDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !providerConfigured(d.meta, &resp.Diagnostics) {
		return
	}

	if data.OrganizationID.IsNull() {
		org, found := findProjectOrganization(ctx, d.meta, data.ID.ValueString(), &resp.Diagnostics)
		if !found {
			return
		}
		data.OrganizationID = types.StringValue(org)
	}

	got, err := d.meta.Client.GetProjectInOrgWithResponse(ctx,
		data.OrganizationID.ValueString(), data.ID.ValueString())
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		// A 404 is an error here, where it removes the resource from state in
		// the resource's Read. A data source naming something absent is a
		// broken configuration, not drift to reconcile.
		addAPIError(&resp.Diagnostics, "read the project", httpResp, body, err)
		return
	}
	p := *got.JSON200

	data.DisplayName = stringOrNull(p.DisplayName)
	data.Description = stringOrNull(p.Description)
	data.SelfLink = stringOrNull(p.SelfLink)

	// `organizationId` is taken from the request where the response omits it,
	// for the reason projectStateFrom gives: every field on Project is a
	// pointer because the whole type is readOnly, and this is the one the
	// caller definitely knows because it was in the URL.
	if p.OrganizationId != nil {
		data.OrganizationID = types.StringValue(*p.OrganizationId)
	}

	data.Status = types.StringNull()
	if p.Status != nil {
		data.Status = types.StringValue(string(*p.Status))
	}
	data.Region = stringOrNull(p.Region)

	data.CreatedAt = types.StringNull()
	if p.CreatedAt != nil {
		data.CreatedAt = types.StringValue(p.CreatedAt.Format(time.RFC3339))
	}

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
