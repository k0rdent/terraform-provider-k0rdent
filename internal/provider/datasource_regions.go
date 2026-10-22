package provider

import (
	"context"
	"net/http"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*regionsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*regionsDataSource)(nil)
)

const regionsDataSourceSuffix = "regions"

func newRegionsDataSource() datasource.DataSource { return &regionsDataSource{} }

type regionsDataSource struct{ meta *Meta }

type regionsModel struct {
	IncludeGlobal types.Bool         `tfsdk:"include_global"`
	Regions       []regionEntryModel `tfsdk:"regions"`
	IDs           types.List         `tfsdk:"ids"`
	ProjectIDs    types.List         `tfsdk:"project_region_ids"`
}

type regionEntryModel struct {
	ID            types.String `tfsdk:"id"`
	Type          types.String `tfsdk:"type"`
	HostsProjects types.Bool   `tfsdk:"hosts_projects"`
}

func (d *regionsDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(regionsDataSourceSuffix)
}

func (d *regionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every region the token can reach.\n\n" +
			"Region ids are operator-declared and there is no published list, so without this " +
			"the only way to fill in the provider's `region` is to be told one.",
		Attributes: map[string]schema.Attribute{
			"include_global": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Include the `global` control-plane tier even where it holds no " +
					"compute. Left out, `global` is listed only where it hosts projects, which is the " +
					"all-in-one install that runs compute under it.",
			},
			// A List, not a Set, because a catalog has to be indexable. A Set orders
			// by a hash of the element, which is no order a configuration can use.
			"regions": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The regions. Sorted by `id`.\n\n" +
					"That order holds when the platform returns the same regions in a different " +
					"order, but not when a region is added: one sorting ahead of the rest shifts " +
					"every index after it. Prefer `for_each` over `[0]`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The value that goes in the provider's `region`.",
						},
						"type": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "`regional` or `global`.",
						},
						"hosts_projects": schema.BoolAttribute{
							Computed: true,
							MarkdownDescription: "Whether a project can be created here: the regions a `" +
								ResourceName(projectResourceSuffix) + "`'s `region` accepts. Every `regional` " +
								"region does. `global` does only on an installation that runs compute under it.",
						},
					},
				},
			},
			"ids": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Just the ids, for `for_each` and `contains`. Sorted by `id` " +
					"alongside `regions`, so `ids[i]` is the id of `regions[i]`.",
			},
			"project_region_ids": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "The ids of the regions that host projects, sorted: the values a `" +
					ResourceName(projectResourceSuffix) + "`'s `region` takes. The contract asks a client " +
					"offering a choice of project region to offer these, not the `regional` ones.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *regionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *regionsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data regionsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if !providerConfigured(d.meta, &resp.Diagnostics) {
		return
	}

	got, err := d.meta.Client.ListRegionsWithResponse(ctx)
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(&resp.Diagnostics, "list the regions", httpResp, body, err)
		return
	}

	includeGlobal := !data.IncludeGlobal.IsNull() && data.IncludeGlobal.ValueBool()

	// Sorted before either collection is built from it, so a reordered response
	// does not move the element a dependent resource reads by index.
	regions := got.JSON200.Regions
	sort.SliceStable(regions, func(i, j int) bool { return regions[i].Id < regions[j].Id })

	data.Regions = []regionEntryModel{}
	ids := []string{}
	for _, r := range regions {
		// `global` is the control-plane tier, and a region only where it
		// hosts projects.
		if r.Id == globalRegion && !includeGlobal && !r.HostsProjects {
			continue
		}
		data.Regions = append(data.Regions, regionEntryModel{
			ID:            types.StringValue(r.Id),
			Type:          types.StringValue(string(r.Type)),
			HostsProjects: types.BoolValue(r.HostsProjects),
		})
		ids = append(ids, r.Id)
	}

	list, diags := types.ListValueFrom(ctx, types.StringType, ids)
	resp.Diagnostics.Append(diags...)
	hosting := projectRegionIDs(regions)
	if hosting == nil {
		hosting = []string{}
	}
	projectIDs, diags := types.ListValueFrom(ctx, types.StringType, hosting)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.IDs, data.ProjectIDs = list, projectIDs

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
