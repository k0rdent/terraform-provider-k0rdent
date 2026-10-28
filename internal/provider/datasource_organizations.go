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
	_ datasource.DataSource              = (*organizationsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*organizationsDataSource)(nil)
)

const organizationsDataSourceSuffix = "organizations"

func newOrganizationsDataSource() datasource.DataSource { return &organizationsDataSource{} }

// organizationsDataSource lists the organizations the token can see.
//
// # Why `projects` was not enough
//
// `projects` reports the organization of every project the token can see, so
// an organization that already holds one is findable there. The first project
// in an organization is the case it misses: the organization holds nothing yet,
// so the project resource's `organization_id` had to be a literal. This reads
// listOrganizations, which the contract marks public.
//
// An API key may get no rows from that list. So the project resource's `organization_id` stays on the
// hardcoded values guide, with the API as the side that has to change.
type organizationsDataSource struct{ meta *Meta }

type organizationsModel struct {
	Status           types.String             `tfsdk:"status"`
	DisplayNameRegex types.String             `tfsdk:"display_name_regex"`
	Organizations    []organizationEntryModel `tfsdk:"organizations"`
	IDs              types.List               `tfsdk:"ids"`
}

type organizationEntryModel struct {
	ID          types.String `tfsdk:"id"`
	DisplayName types.String `tfsdk:"display_name"`
	Description types.String `tfsdk:"description"`
	Status      types.String `tfsdk:"status"`
	SelfLink    types.String `tfsdk:"self_link"`
}

func (d *organizationsDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(organizationsDataSourceSuffix)
}

func (d *organizationsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every organization the token can see.\n\n" +
			"`" + ResourceName(projectResourceSuffix) + "` takes an organization's `id` in " +
			"`organization_id`. This is where to find it, including for an organization that " +
			"holds no project yet, which `" + ResourceName(projectsDataSourceSuffix) + "` cannot " +
			"report.",
		Attributes: map[string]schema.Attribute{
			"status": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Only return organizations in this lifecycle status: " +
					enumProse(k0rdent.ListOrganizationsParamsStatusValues()) + ". Filtered by the platform.",
				// The contract leaves `deleted` out of this enum: the operation
				// refuses it with 422, since a tombstone is never in a live read.
				Validators: []validator.String{oneOfEnum(k0rdent.ListOrganizationsParamsStatusValues())},
			},
			"display_name_regex": displayNameRegexAttribute("organizations"),
			// A List because a catalog has to be indexable and a Set is not.
			// The contract sorts by `id` descending by default, and Read sorts
			// ascending before state is written so the order is the provider's.
			"organizations": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The organizations. Sorted by `id`. That order is stable " +
					"across reads but not across insertions, so a new organization can shift every " +
					"index after it: address them with `for_each` rather than `organizations[0]`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "The id that goes in `" + ResourceName(projectResourceSuffix) +
								"`'s `organization_id`.",
						},
						"display_name": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Human-readable name. Null for an organization created before the platform required one.",
						},
						// The list blanks it, with labels, contact and counts, for a caller
						// whose role there doesn't grant org.settings.read.
						"description": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "Free-form text describing it, for people to read. Null when " +
								"the credential holds no role on the organization that reads its settings, " +
								"such as `organizations-viewer`: a project-scoped credential, or " +
								"`compute-admin` alone on the organization, gets only the id, name, status " +
								"and link. Also null for an organization created before the platform " +
								"required a description.",
						},
						"status": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "Lifecycle status. `active` is the one a project can be " +
								"created in.",
						},
						"self_link": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Server-defined URL for this organization.",
						},
					},
				},
			},
			// Same positions as `organizations`.
			"ids": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Just the ids, for `for_each` and `contains`. Same order as " +
					"`organizations`: sorted by `id`.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *organizationsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *organizationsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data organizationsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !providerConfigured(d.meta, &resp.Diagnostics) {
		return
	}

	filter, diags := readObjectFilter(ctx, types.MapNull(types.StringType), data.DisplayNameRegex, types.StringNull())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	var params k0rdent.ListOrganizationsParams
	if !data.Status.IsNull() && !data.Status.IsUnknown() {
		v := k0rdent.ListOrganizationsParamsStatus(data.Status.ValueString())
		params.Status = &v
	}

	// This operation documents `offset` and `limit`, as listProjects does.
	organizations, ok := listByOffset(&resp.Diagnostics, "organizations",
		func(o k0rdent.Organization) string { return deref(o.Id) },
		func(offset, limit int32) (page[k0rdent.Organization], error) {
			params.Offset, params.Limit = &offset, &limit
			got, err := d.meta.Client.ListOrganizationsWithResponse(ctx, &params)
			if err != nil {
				return page[k0rdent.Organization]{}, err
			}
			if got.JSON200 == nil {
				return page[k0rdent.Organization]{}, unreadPage(got.HTTPResponse, got.Body)
			}
			return page[k0rdent.Organization]{items: rowsOf(got.JSON200.Organizations), total: got.JSON200.TotalCount}, nil
		})
	if !ok {
		return
	}

	data.Organizations = []organizationEntryModel{}
	for _, o := range organizations {
		if !filter.keeps(filterSubject{displayName: o.DisplayName}) {
			continue
		}
		data.Organizations = append(data.Organizations, organizationEntryModel{
			ID:          stringOrNull(o.Id),
			DisplayName: textOrNull(o.DisplayName),
			Description: textOrNull(o.Description),
			Status:      enumOrNull(o.Status),
			SelfLink:    stringOrNull(o.SelfLink),
		})
	}

	// Sorted on the id so a reorder upstream cannot move `ids[i]` onto a
	// different organization.
	sort.SliceStable(data.Organizations, func(i, j int) bool {
		return data.Organizations[i].ID.ValueString() < data.Organizations[j].ID.ValueString()
	})

	ids := make([]string, 0, len(data.Organizations))
	for _, o := range data.Organizations {
		if !o.ID.IsNull() {
			ids = append(ids, o.ID.ValueString())
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
