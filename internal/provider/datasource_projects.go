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
	_ datasource.DataSource              = (*projectsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*projectsDataSource)(nil)
)

const projectsDataSourceSuffix = "projects"

func newProjectsDataSource() datasource.DataSource { return &projectsDataSource{} }

type projectsDataSource struct{ meta *Meta }

type projectsModel struct {
	OrganizationID   types.String        `tfsdk:"organization_id"`
	DisplayNameRegex types.String        `tfsdk:"display_name_regex"`
	Status           types.String        `tfsdk:"status"`
	Projects         []projectEntryModel `tfsdk:"projects"`
	IDs              types.List          `tfsdk:"ids"`
}

type projectEntryModel struct {
	ID             types.String `tfsdk:"id"`
	DisplayName    types.String `tfsdk:"display_name"`
	Description    types.String `tfsdk:"description"`
	Status         types.String `tfsdk:"status"`
	OrganizationID types.String `tfsdk:"organization_id"`
	Region         types.String `tfsdk:"region"`
	SelfLink       types.String `tfsdk:"self_link"`
}

func (d *projectsDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(projectsDataSourceSuffix)
}

func (d *projectsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every project the token can reach.\n\n" +
			"The provider's `project` takes a project's `id`. This is where to find it.",
		Attributes: map[string]schema.Attribute{
			"organization_id": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Only return projects in this organization.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"display_name_regex": displayNameRegexAttribute("projects"),
			"status":             stateFilterAttribute("projects", "status", projectStatusesFilter, true),
			// A List because a catalog has to be indexable and a Set is not.
			// The contract promises no order, so Read sorts before state is
			// written.
			"projects": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The projects. Sorted by `id`, then `organization_id`. " +
					"That order is stable across reads but not across insertions, so a new " +
					"project can shift every index after it: address them with `for_each` " +
					"rather than `projects[0]`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The id that goes in the provider's `project`.",
						},
						"display_name": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Human-readable name, if the project has one.",
						},
						"description": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Free-form text describing it, for people to read, when one was set.",
						},
						"status": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "`active`, `suspended`, `disabled` or `deleting`.",
						},
						"organization_id": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The organization this project belongs to.",
						},
						"region": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "The region the project is locked to, which every regional " +
								"resource in it has to use. Null for a project made before regions were required.",
						},
						"self_link": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Server-defined URL for this project.",
						},
					},
				},
			},
			// Same positions as `projects`.
			"ids": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Just the slugs, for `for_each` and `contains`. Same order as " +
					"`projects`: sorted by `id`, then `organization_id`.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *projectsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *projectsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data projectsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !providerConfigured(d.meta, &resp.Diagnostics) {
		return
	}

	var params k0rdent.ListProjectsParams
	if !data.OrganizationID.IsNull() && !data.OrganizationID.IsUnknown() {
		v := data.OrganizationID.ValueString()
		params.OrganizationId = &v
	}
	filter, diags := readObjectFilter(ctx, types.MapNull(types.StringType), data.DisplayNameRegex, data.Status)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if filter.state != "" {
		status := k0rdent.ListProjectsParamsStatus(filter.state)
		params.Status = &status
	}

	// This operation documents `offset` and `limit`, as the five networking
	// lists and the cluster type catalog do. The other catalogs are still on
	// `pageToken`.
	// Keyed on the organization too: one project id can be in two of them.
	projects, ok := listProjects(ctx, d.meta, params, &resp.Diagnostics)
	if !ok {
		return
	}
	data.Projects = []projectEntryModel{}
	for _, p := range projects {
		if !filter.keeps(filterSubject{displayName: p.DisplayName, state: stateString(p.Status)}) {
			continue
		}
		entry := projectEntryModel{
			ID:             stringOrNull(p.Id),
			DisplayName:    stringOrNull(p.DisplayName),
			Description:    stringOrNull(p.Description),
			Status:         types.StringNull(),
			OrganizationID: stringOrNull(p.OrganizationId),
			Region:         stringOrNull(p.Region),
			SelfLink:       stringOrNull(p.SelfLink),
		}
		if p.Status != nil {
			entry.Status = types.StringValue(string(*p.Status))
		}
		data.Projects = append(data.Projects, entry)
	}

	// Sorted on the slug so a reorder upstream cannot move `ids[i]` onto a
	// different project.
	sort.SliceStable(data.Projects, func(i, j int) bool {
		a, b := data.Projects[i], data.Projects[j]
		if a.ID.ValueString() != b.ID.ValueString() {
			return a.ID.ValueString() < b.ID.ValueString()
		}
		return a.OrganizationID.ValueString() < b.OrganizationID.ValueString()
	})

	ids := make([]string, 0, len(data.Projects))
	for _, p := range data.Projects {
		if !p.ID.IsNull() {
			ids = append(ids, p.ID.ValueString())
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
