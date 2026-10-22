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
	_ datasource.DataSource              = (*shareTypesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*shareTypesDataSource)(nil)
	_ datasource.DataSource              = (*shareTypeDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*shareTypeDataSource)(nil)
)

const (
	shareTypesDataSourceSuffix = "share_types"
	shareTypeDataSourceSuffix  = "share_type"
)

func newShareTypesDataSource() datasource.DataSource { return &shareTypesDataSource{} }
func newShareTypeDataSource() datasource.DataSource  { return &shareTypeDataSource{} }

type shareTypesDataSource struct{ meta *Meta }
type shareTypeDataSource struct{ meta *Meta }

// Read-only: the classes are the operator's, and the contract exposes no
// create, update or delete for them at any visibility. `capabilities` is an
// open map because the key set is not fixed and a client must ignore what it
// does not recognise, so named attributes would go stale on the next class.
type shareTypeEntryModel struct {
	ID             types.String `tfsdk:"id"`
	DisplayName    types.String `tfsdk:"display_name"`
	Capabilities   types.Map    `tfsdk:"capabilities"`
	MaxCapacityGiB types.Int64  `tfsdk:"max_capacity_gib"`
	State          types.String `tfsdk:"state"`
	SelfLink       types.String `tfsdk:"self_link"`
}

type shareTypesModel struct {
	Region           types.String          `tfsdk:"region"`
	DisplayNameRegex types.String          `tfsdk:"display_name_regex"`
	State            types.String          `tfsdk:"state"`
	ShareTypes       []shareTypeEntryModel `tfsdk:"share_types"`
	IDs              types.List            `tfsdk:"ids"`
}

type shareTypeModel struct {
	Region types.String `tfsdk:"region"`
	shareTypeEntryModel
}

func shareTypeEntryAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "The class id, and the value a file store sets as its " +
				"`share_type`. Named by capability, for example `general` or `high-speed`, never " +
				"after a vendor, product or array. Stable: an id is not reused for a different " +
				"capability.",
		},
		"display_name": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Human-friendly name, suitable for a picker.",
		},
		"capabilities": schema.MapAttribute{
			Computed:    true,
			ElementType: types.StringType,
			MarkdownDescription: "Capability facts about the class as flat key/value pairs, for " +
				"example `protocol = \"nfs\"` or `tier = \"performance\"`.\n\n" +
				"The set of keys is not fixed, so read the ones you know and ignore the rest. " +
				"Values are drawn from a capability vocabulary only: the backing hardware cannot " +
				"be inferred from this map and must not be guessed at.",
		},
		"max_capacity_gib": schema.Int64Attribute{
			Computed: true,
			MarkdownDescription: "The largest `capacity_gib` one store can be given from this class. " +
				"Null when the class sets no ceiling of its own.",
		},
		"state": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "The class's lifecycle state. Only an `active` class can be " +
				"provisioned from.",
		},
		"self_link": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Platform-defined reference to this class.",
		},
	}
}

func (d *shareTypesDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(shareTypesDataSourceSuffix)
}

func (d *shareTypesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every storage capability class offered in a region.\n\n" +
			"A class is what a `" + ResourceName(fileStoreResourceSuffix) + "` is provisioned " +
			"from, and its id is that resource's `share_type`. Classes are operator-authored and " +
			"there is no published list, so this is how a configuration discovers them rather " +
			"than hard-coding an id that may not exist in the region it is applied to.\n\n" +
			"Region-scoped, not project-scoped: a class serves every project in its region.",
		Attributes: map[string]schema.Attribute{
			"region":             dataSourceRegionAttribute(),
			"display_name_regex": displayNameRegexAttribute("classes"),
			"state":              stateFilterAttribute("classes", "state", shareTypeStatesFilter, false),
			// A List because a catalog has to be indexable and a Set is not.
			"share_types": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The classes. Sorted by `id`.\n\n" +
					"That order is stable when the platform reorders the catalog, but not when it " +
					"gains a class: one sorting ahead of the rest shifts every index after it. Prefer " +
					"`for_each` over `[0]`.",
				NestedObject: schema.NestedAttributeObject{Attributes: shareTypeEntryAttributes()},
			},
			"ids": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Just the ids, for `for_each` and `contains`. Sorted by `id` " +
					"alongside `share_types`, so `ids[i]` is the id of `share_types[i]`.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *shareTypesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *shareTypesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data shareTypesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	region, ok := resolveRegion(d.meta, data.Region, &resp.Diagnostics)
	if !ok {
		return
	}

	shareTypes, diags := listShareTypes(ctx, d.meta, region)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Sorted before anything is built from it, so a class appearing in a
	// different order upstream does not move the index a dependent resource reads.
	sort.SliceStable(shareTypes, func(i, j int) bool {
		return deref(shareTypes[i].Id) < deref(shareTypes[j].Id)
	})
	filter, filterDiags := readObjectFilter(ctx, types.MapNull(types.StringType), data.DisplayNameRegex, data.State)
	resp.Diagnostics.Append(filterDiags...)
	if resp.Diagnostics.HasError() {
		return
	}

	data.Region = types.StringValue(region)
	data.ShareTypes = []shareTypeEntryModel{}
	ids := []string{}
	for _, st := range shareTypes {
		if !filter.keeps(filterSubject{displayName: st.DisplayName, state: stateString(st.State)}) {
			continue
		}
		entry, d := shareTypeEntryFrom(ctx, st)
		resp.Diagnostics.Append(d...)
		if resp.Diagnostics.HasError() {
			return
		}
		data.ShareTypes = append(data.ShareTypes, entry)
		ids = append(ids, deref(st.Id))
	}

	list, diags := types.ListValueFrom(ctx, types.StringType, ids)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.IDs = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// listShareTypes walks every page.
//
// The cursor is the deprecated pagination style. `listShareTypes` documents
// `pageToken` and `pageSize` and does not document `offset` and `limit`, and
// the shared parameter's own note says operations migrate individually and that
// an operation which has not migrated accepts only the cursor. So this is not a
// choice: a data source is born on a deprecated parameter, and offset paging
// would be rejected with a 422.
func listShareTypes(ctx context.Context, meta *Meta, region string) ([]k0rdent.ShareType, diag.Diagnostics) {
	var diags diag.Diagnostics
	shareTypes, _ := listByToken(&diags, "storage share types", func(token *string, size *int) (page[k0rdent.ShareType], error) {
		got, err := meta.Client.ListShareTypesWithResponse(ctx, region, &k0rdent.ListShareTypesParams{PageToken: token, PageSize: size})
		if err != nil {
			return page[k0rdent.ShareType]{}, err
		}
		if got.JSON200 == nil {
			return page[k0rdent.ShareType]{}, unreadPage(got.HTTPResponse, got.Body)
		}
		return page[k0rdent.ShareType]{items: rowsOf(got.JSON200.ShareTypes), next: got.JSON200.NextPageToken}, nil
	})
	return shareTypes, diags
}

func (d *shareTypeDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(shareTypeDataSourceSuffix)
}

func (d *shareTypeDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := shareTypeEntryAttributes()
	attrs["id"] = schema.StringAttribute{
		Required:            true,
		MarkdownDescription: "The class id, for example `general` or `high-speed`.",
		Validators:          []validator.String{urlSafeIdentifier{}},
	}
	attrs["region"] = dataSourceRegionAttribute()

	resp.Schema = schema.Schema{
		MarkdownDescription: "One storage capability class, by id.\n\n" +
			"Reading the class a store names is the plan-time way to find out that it does not " +
			"exist in this region. Supplying one that does not is otherwise a `422` partway " +
			"through the apply.\n\n" +
			"Use `" + ResourceName(shareTypesDataSourceSuffix) + "` when the id is not known " +
			"ahead of time, or to pick by capability.",
		Attributes: attrs,
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *shareTypeDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *shareTypeDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data shareTypeModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	region, ok := resolveRegion(d.meta, data.Region, &resp.Diagnostics)
	if !ok {
		return
	}
	id := data.ID.ValueString()

	got, err := d.meta.Client.GetShareTypeWithResponse(ctx, region, id)
	if err != nil {
		addAPIError(&resp.Diagnostics, "read the storage share type", nil, nil, err)
		return
	}
	if isNotFound(got.HTTPResponse, got.Body) {
		// Local: an error, not an empty result. The store about to be planned
		// against it has no class to provision from, and the platform refuses
		// an unknown class rather than substituting one.
		resp.Diagnostics.AddError(
			"No such storage share type",
			fmt.Sprintf("No share type %q in region %q. Classes are operator-authored and differ "+
				"between regions; `%s` lists the ones this token can see.", id, region,
				ResourceName(shareTypesDataSourceSuffix)),
		)
		return
	}
	if got.JSON200 == nil {
		addAPIError(&resp.Diagnostics, "read the storage share type", got.HTTPResponse, got.Body, nil)
		return
	}

	entry, diags := shareTypeEntryFrom(ctx, *got.JSON200)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.shareTypeEntryModel = entry
	data.Region = types.StringValue(region)

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// shareTypeEntryFrom maps one share type onto the model.
//
// `capabilities` is copied whole, so a key added upstream appears in state on
// the next refresh with no change here. The contract keeps vendor names, model
// numbers and array identifiers out of it and tells clients to ignore keys they
// do not recognise, so dropping unknown keys would discard what the map is for.
//
// If that guarantee is ever relaxed upstream, this is the line to revisit.
func shareTypeEntryFrom(ctx context.Context, st k0rdent.ShareType) (shareTypeEntryModel, diag.Diagnostics) {
	capabilities, diags := stringMapOrNull(ctx, st.Capabilities)

	state := types.StringNull()
	if st.State != nil {
		state = types.StringValue(string(*st.State))
	}
	return shareTypeEntryModel{
		ID:             types.StringValue(deref(st.Id)),
		DisplayName:    stringOrNull(st.DisplayName),
		Capabilities:   capabilities,
		MaxCapacityGiB: int64OrNull(st.MaxCapacityGiB),
		State:          state,
		SelfLink:       stringOrNull(st.SelfLink),
	}, diags
}
