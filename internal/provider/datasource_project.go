package provider

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
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
// the lookup refuses an id that more than one does. Without the organization,
// the project is the row the project list has for that exact id: a list row is
// the same Project a read by id answers. Given the organization, the read is
// one GET.
//
// The list is also the only project read a credential scoped to a project can
// make. The platform checks the read by id against the organization, where a
// role bound to a project doesn't count, so it refuses that credential its own
// project with a bare 403. That 403 falls back to the list, filtered to the
// organization and the id.
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
			"refused and `organization_id` has to be set.\n\n" +
			"A credential scoped to a project can read that project, with or without " +
			"`organization_id`. The platform refuses such a credential a project's read by id, " +
			"because it checks that read against the organization, so the provider reads the " +
			"project from the project list instead, which shows the credential its own projects.",
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
					"left out, in the one list request that reads the project.",
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
				MarkdownDescription: "Human-friendly name. Null when the project has none.",
			},
			"description": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Free-form text describing it, for people to read. Null when the project has none.",
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

	var p k0rdent.Project
	if data.OrganizationID.IsNull() {
		// The list row is the project, so this is one request, and it's one a
		// credential scoped to the project can make.
		row, found := findProject(ctx, d.meta, data.ID.ValueString(), &resp.Diagnostics)
		if !found {
			return
		}
		p = row
		data.OrganizationID = types.StringValue(stateString(row.OrganizationId))
	} else {
		row, ok := d.readInOrg(ctx, data.OrganizationID.ValueString(), data.ID.ValueString(), &resp.Diagnostics)
		if !ok {
			return
		}
		p = row
	}

	data.DisplayName = textOrNull(p.DisplayName)
	data.Description = textOrNull(p.Description)
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

// readInOrg reads the project by id inside the organization. The platform
// checks that read against the organization, so a credential scoped to the
// project is refused it, with a bare 403. That credential can still see the
// project in the project list, so a 403 is answered from the list when it
// shows the project in that organization, and reported as it came otherwise.
func (d *projectDataSource) readInOrg(ctx context.Context, org, id string, diags *diag.Diagnostics) (k0rdent.Project, bool) {
	got, err := d.meta.Client.GetProjectInOrgWithResponse(ctx, org, id)
	if err == nil && got.JSON200 != nil {
		return *got.JSON200, true
	}
	var body []byte
	var httpResp *http.Response
	if got != nil {
		body, httpResp = got.Body, got.HTTPResponse
	}
	if err == nil && httpResp != nil && httpResp.StatusCode == http.StatusForbidden {
		row, found, listErr := listedProject(ctx, d.meta, org, id)
		if found {
			return row, true
		}
		advice := fmt.Sprintf("The project list doesn't show %q in organization %q either, so this "+
			"credential can't see it there. Check both ids, or read it with a credential that has "+
			"organizations-viewer or compute-viewer on the organization.", id, org)
		if listErr != nil {
			advice = fmt.Sprintf("The project list, which shows a credential scoped to a project its own "+
				"projects, couldn't be read either: %s", withoutInlineRequestID(listErr))
		}
		addAdvisedError(diags, "Could not read the project", advice, k0rdent.CheckResponse(httpResp, body))
		return k0rdent.Project{}, false
	}
	// A 404 is an error here, where it removes the resource from state in
	// the resource's Read. A data source naming something absent is a
	// broken configuration, not drift to reconcile.
	addAPIError(diags, "read the project", httpResp, body, err)
	return k0rdent.Project{}, false
}

// listedProject is the project the list shows with this id in this
// organization. found is false when the list doesn't show it, and err is set
// when the list couldn't be read.
func listedProject(ctx context.Context, meta *Meta, org, id string) (p k0rdent.Project, found bool, err error) {
	search := id
	rows, err := readProjects(ctx, meta, k0rdent.ListProjectsParams{OrganizationId: &org, Search: &search})
	if err != nil {
		return k0rdent.Project{}, false, err
	}
	for _, row := range rows {
		if stateString(row.Id) == id && stateString(row.OrganizationId) == org {
			return row, true, nil
		}
	}
	return k0rdent.Project{}, false, nil
}
