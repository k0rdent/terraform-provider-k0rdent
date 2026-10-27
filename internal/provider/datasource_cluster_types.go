package provider

import (
	"context"
	"net/http"
	"sort"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ datasource.DataSource                   = (*clusterTypesDataSource)(nil)
	_ datasource.DataSourceWithConfigure      = (*clusterTypesDataSource)(nil)
	_ datasource.DataSourceWithValidateConfig = (*clusterTypesDataSource)(nil)
	_ datasource.DataSource                   = (*clusterTypeDataSource)(nil)
	_ datasource.DataSourceWithConfigure      = (*clusterTypeDataSource)(nil)
)

const (
	clusterTypesDataSourceSuffix = "cluster_types"
	clusterTypeDataSourceSuffix  = "cluster_type"
)

func newClusterTypesDataSource() datasource.DataSource { return &clusterTypesDataSource{} }
func newClusterTypeDataSource() datasource.DataSource  { return &clusterTypeDataSource{} }

// ---------------------------------------------------------------------------
// The catalog
// ---------------------------------------------------------------------------

type clusterTypesDataSource struct{ meta *Meta }

type clusterTypesModel struct {
	Region            types.String            `tfsdk:"region"`
	IncludeDeprecated types.Bool              `tfsdk:"include_deprecated"`
	DisplayNameRegex  types.String            `tfsdk:"display_name_regex"`
	Status            types.String            `tfsdk:"status"`
	ClusterTypes      []clusterTypeEntryModel `tfsdk:"cluster_types"`
}

type clusterTypeEntryModel struct {
	ID          types.String `tfsdk:"id"`
	DisplayName types.String `tfsdk:"display_name"`
	Description types.String `tfsdk:"description"`
	Status      types.String `tfsdk:"status"`
	Version     types.Int64  `tfsdk:"version"`
	SelfLink    types.String `tfsdk:"self_link"`
}

func (d *clusterTypesDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(clusterTypesDataSourceSuffix)
}

func (d *clusterTypesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every ClusterType available in a region.\n\n" +
			"This is the catalog a cluster is created from: an entry's `self_link` is what a " +
			"`" + ResourceName(clusterResourceSuffix) + "` resource's `cluster_type` takes. " +
			"It needs a region but no project: the catalog is region-wide and scoped only by the " +
			"token.",
		Attributes: map[string]schema.Attribute{
			"region": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Region to read. Defaults to the provider's `region`. " +
					"Region ids come from `" + regionsOperation + "`.",
				Validators: []validator.String{urlSafeIdentifier{}},
			},
			"include_deprecated": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Include the ClusterTypes the platform hides by default: the `deprecated` " +
					"ones, and on a platform that lists each type at its latest `available` version, " +
					"every one with no `available` version.",
			},
			"display_name_regex": displayNameRegexAttribute("ClusterTypes"),
			"status": func() schema.StringAttribute {
				a := stateFilterAttribute("ClusterTypes", "status", clusterTypeStatusesFilter, false)
				a.MarkdownDescription += " `draft` and `deprecated` ask the platform for the types it hides " +
					"by default too, so neither needs `include_deprecated` beside it. " +
					"`include_deprecated = false` beside `deprecated` is refused at plan, since the pair can " +
					"only answer an empty list."
				return a
			}(),
			// A List because a catalog has to be indexable and a Set is not:
			// a set of objects is ordered by a hash of its content.
			"cluster_types": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The catalog. Sorted by `id`, then `self_link`. The order holds " +
					"across reads, but a new entry sorting above an existing one shifts every index " +
					"below it, so iterate with `for_each` rather than reading `cluster_types[0]`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The ClusterType's id, its slug in the region's catalog.",
						},
						"display_name": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Human-readable name, for display only.",
						},
						"description": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Free-form text describing it, for people to read, when one was set.",
						},
						"status": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Lifecycle status of the latest version: `available`, `draft` or `deprecated`.",
						},
						"version": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Sequential version number, for display.",
						},
						"self_link": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "Reference to this ClusterType, suitable for a cluster's " +
								"`cluster_type`. Head-scoped, so it binds whatever the current version is.",
						},
					},
				},
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *clusterTypesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *clusterTypesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data clusterTypesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	region, ok := resolveRegion(d.meta, data.Region, &resp.Diagnostics)
	if !ok {
		return
	}

	filter, diags := readObjectFilter(ctx, types.MapNull(types.StringType), data.DisplayNameRegex, data.Status)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	var params k0rdent.ListClusterTypesParams
	if !data.IncludeDeprecated.IsNull() && !data.IncludeDeprecated.IsUnknown() {
		v := data.IncludeDeprecated.ValueBool()
		params.IncludeDeprecated = &v
	}
	// The platform hides deprecated types unless asked, and one that lists
	// each type at its latest `available` version hides every type without
	// one, so a filter for any other status could come back empty. Those
	// platforms take `includeDeprecated` as an alias of `includeUnavailable`.
	if filter.state != "" && filter.state != string(k0rdent.ClusterTypeVersionStatusAvailable) {
		v := true
		params.IncludeDeprecated = &v
	}

	// Offset paging is the only way past the first page: a request without it
	// answers the first page and no `nextPageToken`, and the cursor was never
	// implemented here. The contract allows a missing collection, `null` and
	// `[]`, and all three mean no cluster types, which is not an error.
	listed, ok := listByOffset(&resp.Diagnostics, "cluster types",
		func(ct k0rdent.ClusterTypeSummary) string { return deref(ct.Id) },
		func(offset, limit int32) (page[k0rdent.ClusterTypeSummary], error) {
			params.Offset, params.Limit = &offset, &limit
			got, err := d.meta.Client.ListClusterTypesWithResponse(ctx, region, &params)
			if err != nil {
				return page[k0rdent.ClusterTypeSummary]{}, err
			}
			if got.JSON200 == nil {
				return page[k0rdent.ClusterTypeSummary]{}, unreadPage(got.HTTPResponse, got.Body)
			}
			return page[k0rdent.ClusterTypeSummary]{items: got.JSON200.ClusterTypes, total: got.JSON200.TotalCount}, nil
		})
	if !ok {
		return
	}
	data.ClusterTypes = []clusterTypeEntryModel{}
	for _, ct := range listed {
		if !filter.keeps(filterSubject{displayName: &ct.DisplayName, state: string(ct.Status)}) {
			continue
		}
		entry := clusterTypeEntryModel{
			ID:          stringOrNull(ct.Id),
			DisplayName: types.StringValue(ct.DisplayName),
			Description: stringOrNull(ct.Description),
			Status:      types.StringValue(string(ct.Status)),
			SelfLink:    stringOrNull(ct.SelfLink),
			Version:     types.Int64Null(),
		}
		if ct.Version != nil {
			entry.Version = types.Int64Value(int64(*ct.Version))
		}
		data.ClusterTypes = append(data.ClusterTypes, entry)
	}

	// The platform promises no order, so sort on identity: an index into the
	// list has to mean the same entry on the next read.
	sort.SliceStable(data.ClusterTypes, func(i, j int) bool {
		a, b := data.ClusterTypes[i], data.ClusterTypes[j]
		if a.ID.ValueString() != b.ID.ValueString() {
			return a.ID.ValueString() < b.ID.ValueString()
		}
		return a.SelfLink.ValueString() < b.SelfLink.ValueString()
	})

	data.Region = types.StringValue(region)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// ---------------------------------------------------------------------------
// One ClusterType, with its node pools
// ---------------------------------------------------------------------------

type clusterTypeDataSource struct{ meta *Meta }

type clusterTypeModel struct {
	ID          types.String                `tfsdk:"id"`
	Region      types.String                `tfsdk:"region"`
	DisplayName types.String                `tfsdk:"display_name"`
	Description types.String                `tfsdk:"description"`
	Status      types.String                `tfsdk:"status"`
	Version     types.Int64                 `tfsdk:"version"`
	SelfLink    types.String                `tfsdk:"self_link"`
	K8sVersion  types.String                `tfsdk:"kubernetes_version"`
	NodePools   []clusterTypePoolEntryModel `tfsdk:"node_pools"`
}

type clusterTypePoolEntryModel struct {
	ID               types.String `tfsdk:"id"`
	DisplayName      types.String `tfsdk:"display_name"`
	Role             types.String `tfsdk:"role"`
	MachineType      types.String `tfsdk:"machine_type"`
	NodeCountDefault types.Int64  `tfsdk:"node_count_default"`
	NodeCountMin     types.Int64  `tfsdk:"node_count_min"`
	NodeCountMax     types.Int64  `tfsdk:"node_count_max"`
}

func (d *clusterTypeDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(clusterTypeDataSourceSuffix)
}

func (d *clusterTypeDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One ClusterType, including its node pools.\n\n" +
			"The catalog list does not carry node pools, and pool ids are what a cluster's " +
			"`node_pool` map is keyed by, so this is the data source that tells you which pools " +
			"exist, what they are for, and what node counts they accept.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The ClusterType's id, its slug in the region's catalog.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"region": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Region to read. Defaults to the provider's `region`.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-readable name, for display only."},
			"description":  schema.StringAttribute{Computed: true, MarkdownDescription: "Free-form text describing it, for people to read, when one was set."},
			"status": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Lifecycle status of the current version.",
			},
			"version": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Sequential version number of the current version, for display.",
			},
			"self_link": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Reference to this ClusterType, for a cluster's `cluster_type`. " +
					"Head-scoped: it binds whatever the current version is at create time.",
			},
			"kubernetes_version": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The k0s version clusters of this type are provisioned with, " +
					"as `vX.Y.Z` or `vX.Y.Z+k0s.N`. Null for a ClusterType that is not " +
					"Kubernetes-flavoured. There is no catalogue of supported versions, this is " +
					"the version this type gives you.",
			},
			// A List, not a Set: a set of objects is ordered by a hash of its
			// content, which gives no usable position.
			"node_pools": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Node pools defined at the current version. Sorted by `id`.",
				NestedObject:        schema.NestedAttributeObject{Attributes: clusterTypePoolAttributes()},
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

// clusterTypePoolAttributes is the pool shape, shared by the head-version and
// the pinned-version data sources.
func clusterTypePoolAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "Pool slug. This is the key a cluster's or instance group's " +
				"`node_pool` map uses.",
		},
		"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Optional display name."},
		"role": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "`control-plane` or `worker`. Control-plane pools can't be " +
				"scaled.",
		},
		"machine_type": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "Reference to the MachineType backing this pool." +
				knownGap("A name only. A tenant can't read what hardware or image it is."),
		},
		"node_count_default": schema.Int64Attribute{
			Computed: true,
			MarkdownDescription: "Count used when a cluster does not override this pool. " +
				"Null means a cluster must supply one.",
		},
		"node_count_min": schema.Int64Attribute{
			Computed:            true,
			MarkdownDescription: "Lowest count this pool accepts. Null means no floor above zero.",
		},
		"node_count_max": schema.Int64Attribute{
			Computed:            true,
			MarkdownDescription: "Highest count this pool accepts. Null means unbounded.",
		},
	}
}

func (d *clusterTypeDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *clusterTypeDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data clusterTypeModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	region, ok := resolveRegion(d.meta, data.Region, &resp.Diagnostics)
	if !ok {
		return
	}

	got, err := d.meta.Client.GetClusterTypeWithResponse(ctx, region, data.ID.ValueString())
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(&resp.Diagnostics, "read the cluster type", httpResp, body, err)
		return
	}
	ct := *got.JSON200

	data.Region = types.StringValue(region)
	data.DisplayName = types.StringValue(ct.DisplayName)
	data.Description = stringOrNull(ct.Description)
	data.Status = types.StringValue(string(ct.Status))
	data.SelfLink = stringOrNull(ct.SelfLink)
	data.Version = types.Int64Null()
	if ct.Version != nil {
		data.Version = types.Int64Value(int64(*ct.Version))
	}
	data.K8sVersion = types.StringNull()
	if ct.K8s != nil {
		data.K8sVersion = types.StringValue(ct.K8s.K0sVersion)
	}

	data.NodePools = make([]clusterTypePoolEntryModel, 0, len(ct.NodePools))
	for _, p := range ct.NodePools {
		data.NodePools = append(data.NodePools, clusterTypePoolEntryModel{
			ID:               types.StringValue(p.Id),
			DisplayName:      stringOrNull(p.DisplayName),
			Role:             types.StringValue(string(p.Role)),
			MachineType:      types.StringValue(p.MachineType),
			NodeCountDefault: int64OrNull(p.NodeCountDefault),
			NodeCountMin:     int64OrNull(p.NodeCountMin),
			NodeCountMax:     int64OrNull(p.NodeCountMax),
		})
	}
	// Pool ids are unique within a ClusterType, so sorting on one is a total
	// order and an index into the list is stable across reads.
	sort.SliceStable(data.NodePools, func(i, j int) bool {
		return data.NodePools[i].ID.ValueString() < data.NodePools[j].ID.ValueString()
	})

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// ValidateConfig refuses `status = "deprecated"` beside
// `include_deprecated = false`. The platform hides deprecated types unless
// asked, so the pair can only ever answer an empty list, and a configuration
// that wrote it meant one of the two. `draft` beside it is not refused: Read
// asks for the hidden types anyway, and the status filter drops the
// deprecated ones, so the pair answers the draft types.
func (d *clusterTypesDataSource) ValidateConfig(ctx context.Context, req datasource.ValidateConfigRequest, resp *datasource.ValidateConfigResponse) {
	var data clusterTypesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if data.Status.IsUnknown() || data.Status.ValueString() != string(k0rdent.ClusterTypeVersionStatusDeprecated) ||
		data.IncludeDeprecated.IsNull() || data.IncludeDeprecated.IsUnknown() || data.IncludeDeprecated.ValueBool() {
		return
	}
	// Local: configuration validation, no request made.
	resp.Diagnostics.AddAttributeError(path.Root("include_deprecated"), "Deprecated types excluded and asked for",
		"`status = \"deprecated\"` asks for deprecated types only, and `include_deprecated = false` "+
			"leaves them out, so the list would always be empty. Remove `include_deprecated`: "+
			"`status = \"deprecated\"` already asks the platform for them.")
}
