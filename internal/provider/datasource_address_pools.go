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
	_ datasource.DataSource              = (*addressPoolsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*addressPoolsDataSource)(nil)
	_ datasource.DataSource              = (*addressPoolDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*addressPoolDataSource)(nil)
)

const (
	addressPoolsDataSourceSuffix = "address_pools"
	addressPoolDataSourceSuffix  = "address_pool"
)

func newAddressPoolsDataSource() datasource.DataSource { return &addressPoolsDataSource{} }
func newAddressPoolDataSource() datasource.DataSource  { return &addressPoolDataSource{} }

type addressPoolsDataSource struct{ meta *Meta }
type addressPoolDataSource struct{ meta *Meta }

// An AddressPool is the thing a subnet allocates out of, and until this existed
// the only way to fill in a subnet's `address_pool` was to be handed the URI.
// Two of the subnet's other arguments are constrained by the pool as well:
// `prefix_length` has to land inside [minPrefixLength, maxPrefixLength], and
// `allocate_gateway` does nothing unless the pool is routable. Neither of those
// is checkable from inside the subnet resource, because Terraform cannot read
// another object during validation, so publishing them is the closest the
// provider can get to catching it before the apply.
//
// Read-only because the mutations are operator-only: createAddressPool is
// declared `x-visibility: internal` and is not in the generated client at all.

type addressPoolEntryModel struct {
	ID              types.String `tfsdk:"id"`
	DisplayName     types.String `tfsdk:"display_name"`
	Description     types.String `tfsdk:"description"`
	IPVersion       types.String `tfsdk:"ip_version"`
	Prefixes        types.List   `tfsdk:"prefixes"`
	ExcludeRanges   types.List   `tfsdk:"exclude_ranges"`
	MinPrefixLength types.Int64  `tfsdk:"min_prefix_length"`
	MaxPrefixLength types.Int64  `tfsdk:"max_prefix_length"`
	Routable        types.Bool   `tfsdk:"routable"`
	State           types.String `tfsdk:"state"`
	SelfLink        types.String `tfsdk:"self_link"`
}

type addressPoolsModel struct {
	Region           types.String            `tfsdk:"region"`
	OnlyRoutable     types.Bool              `tfsdk:"only_routable"`
	DisplayNameRegex types.String            `tfsdk:"display_name_regex"`
	State            types.String            `tfsdk:"state"`
	AddressPools     []addressPoolEntryModel `tfsdk:"address_pools"`
	IDs              types.List              `tfsdk:"ids"`
}

type addressPoolModel struct {
	Region types.String `tfsdk:"region"`
	addressPoolEntryModel
}

func addressPoolEntryAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "The pool's name.",
		},
		"display_name": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Human-friendly name, for display only.",
		},
		"description": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Free-form text describing it, for people to read.",
		},
		"ip_version": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "IP family of every prefix in the pool. IPv4 only, today.",
		},
		// A List because the platform returns one and nothing here promises an
		// order, so the provider sorts. Prefixes within a pool cannot overlap,
		// so the value is its own identity and the order is total.
		"prefixes": schema.ListAttribute{
			Computed:            true,
			ElementType:         types.StringType,
			MarkdownDescription: "The blocks subnets are allocated from. Sorted lexically by prefix.",
		},
		// Same reasoning as `prefixes`.
		"exclude_ranges": schema.ListAttribute{
			Computed:    true,
			ElementType: types.StringType,
			MarkdownDescription: "Addresses inside `prefixes` the operator has reserved. A subnet " +
				"allocated from this pool will never contain them. Sorted lexically by range.",
		},
		"min_prefix_length": schema.Int64Attribute{
			Computed: true,
			MarkdownDescription: "Smallest `prefix_length` a subnet may ask this pool for. " +
				"Defaults to `8` on IPv4.",
		},
		"max_prefix_length": schema.Int64Attribute{
			Computed: true,
			MarkdownDescription: "Largest `prefix_length` a subnet may ask this pool for. Defaults " +
				"to `30` on IPv4; `/32` is permitted where the operator has raised it, for a " +
				"single-host case such as a control-plane VIP.",
		},
		"routable": schema.BoolAttribute{
			Computed: true,
			MarkdownDescription: "Whether subnets from this pool are reachable from outside the " +
				"tenant VPC. A subnet's `allocate_gateway` only takes effect on a routable pool.",
		},
		"state": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "Lifecycle state. A pool that is not `active` cannot be allocated from.",
		},
		"self_link": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "Platform-defined reference to this pool. **This is the value a `" +
				ResourceName(subnetResourceSuffix) + "` takes as its `address_pool`.**",
		},
	}
}

func (d *addressPoolsDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(addressPoolsDataSourceSuffix)
}

func (d *addressPoolsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every AddressPool in a region.\n\n" +
			"Pools are operator-authored and there is no published list, so without this the only " +
			"way to fill in a subnet's `address_pool` is to be told the URI.\n\n" +
			"Region-scoped, not project-scoped: a pool serves every project in its region.",
		Attributes: map[string]schema.Attribute{
			"region": dataSourceRegionAttribute(),
			"only_routable": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Return only pools whose subnets are reachable from outside the " +
					"tenant VPC. Filtered in the provider, because the list operation takes no " +
					"filter, so every pool is fetched either way.",
			},
			"display_name_regex": displayNameRegexAttribute("pools"),
			"state":              stateFilterAttribute("pools", "state", addressPoolStatesFilter, false),
			// A List because a catalog has to be indexable and a Set is not.
			// The contract promises no order for any list operation, so the
			// provider imposes one.
			"address_pools": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The pools, sorted by `id`. The order survives a reorder " +
					"upstream but not an insertion: a new pool sorting ahead of the others shifts " +
					"every index after it, so reach for `for_each` rather than `[0]`.",
				NestedObject: schema.NestedAttributeObject{Attributes: addressPoolEntryAttributes()},
			},
			// Built from the same sorted slice, so `ids[i]` names `address_pools[i]`.
			"ids": schema.ListAttribute{
				Computed:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Just the ids, for `for_each` and `contains`. Sorted by `id`, " +
					"the same order as `address_pools`.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *addressPoolsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *addressPoolsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data addressPoolsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	region, ok := resolveRegion(d.meta, data.Region, &resp.Diagnostics)
	if !ok {
		return
	}

	pools, ok := listAddressPools(ctx, d.meta, region, &resp.Diagnostics)
	if !ok {
		return
	}

	// Sorted once, before the filter, so `ids[i]` keeps naming `address_pools[i]`
	// and neither moves when the platform returns the same pools in another order.
	sort.SliceStable(pools, func(i, j int) bool {
		return deref(pools[i].Id) < deref(pools[j].Id)
	})

	onlyRoutable := !data.OnlyRoutable.IsNull() && data.OnlyRoutable.ValueBool()
	filter, diags := readObjectFilter(ctx, types.MapNull(types.StringType), data.DisplayNameRegex, data.State)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	data.Region = types.StringValue(region)
	data.AddressPools = []addressPoolEntryModel{}
	ids := []string{}
	for _, pool := range pools {
		if onlyRoutable && !pool.Routable {
			continue
		}
		if !filter.keeps(filterSubject{displayName: pool.DisplayName, state: stateString(pool.State)}) {
			continue
		}
		entry, d := addressPoolEntryFrom(ctx, pool)
		resp.Diagnostics.Append(d...)
		if resp.Diagnostics.HasError() {
			return
		}
		data.AddressPools = append(data.AddressPools, entry)
		ids = append(ids, deref(pool.Id))
	}

	list, diags := types.ListValueFrom(ctx, types.StringType, ids)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.IDs = list

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func (d *addressPoolDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(addressPoolDataSourceSuffix)
}

func (d *addressPoolDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := addressPoolEntryAttributes()
	attrs["id"] = schema.StringAttribute{
		Required:            true,
		MarkdownDescription: "The pool's name.",
		Validators:          []validator.String{urlSafeIdentifier{}},
	}
	attrs["region"] = dataSourceRegionAttribute()

	resp.Schema = schema.Schema{
		MarkdownDescription: "One AddressPool, by name.\n\n" +
			"Use `" + ResourceName(addressPoolsDataSourceSuffix) + "` when the name is not known " +
			"ahead of time, or to pick by `routable`.",
		Attributes: attrs,
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *addressPoolDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *addressPoolDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data addressPoolModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	region, ok := resolveRegion(d.meta, data.Region, &resp.Diagnostics)
	if !ok {
		return
	}
	id := data.ID.ValueString()

	got, err := d.meta.Client.GetAddressPoolWithResponse(ctx, region, id)
	if err != nil {
		addAPIError(&resp.Diagnostics, "read the address pool", nil, nil, err)
		return
	}
	if isNotFound(got.HTTPResponse, got.Body) {
		// A data source that cannot find what it was named is an error: the
		// subnet about to be planned against it has nowhere to allocate from.
		resp.Diagnostics.AddError(
			"No such address pool",
			fmt.Sprintf("No AddressPool named %q in region %q. Pools are operator-authored; `%s` "+
				"lists the ones this token can see.", id, region,
				ResourceName(addressPoolsDataSourceSuffix)),
		)
		return
	}
	if got.JSON200 == nil {
		addAPIError(&resp.Diagnostics, "read the address pool", got.HTTPResponse, got.Body, nil)
		return
	}

	entry, diags := addressPoolEntryFrom(ctx, *got.JSON200)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	data.addressPoolEntryModel = entry
	data.Region = types.StringValue(region)

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func addressPoolEntryFrom(ctx context.Context, p k0rdent.AddressPool) (addressPoolEntryModel, diag.Diagnostics) {
	var diags diag.Diagnostics

	// Copies, so sorting for a stable state does not reorder the response itself.
	prefixValues := append([]string(nil), p.Prefixes...)
	sort.Strings(prefixValues)
	prefixes, d := types.ListValueFrom(ctx, types.StringType, prefixValues)
	diags.Append(d...)

	excludeValues := presentList(p.ExcludeRanges)
	if excludeValues != nil {
		sorted := append([]string(nil), *excludeValues...)
		sort.Strings(sorted)
		excludeValues = &sorted
	}
	excludes, d := stringListOrNull(ctx, excludeValues)
	diags.Append(d...)

	out := addressPoolEntryModel{
		ID:              types.StringValue(deref(p.Id)),
		DisplayName:     stringOrNull(p.DisplayName),
		Description:     stringOrNull(p.Description),
		IPVersion:       types.StringValue(string(p.IpVersion)),
		Prefixes:        prefixes,
		ExcludeRanges:   excludes,
		MinPrefixLength: int64OrNull(p.MinPrefixLength),
		MaxPrefixLength: int64OrNull(p.MaxPrefixLength),
		Routable:        types.BoolValue(p.Routable),
		State:           types.StringNull(),
		SelfLink:        stringOrNull(p.SelfLink),
	}
	if p.State != nil {
		out.State = types.StringValue(string(*p.State))
	}
	return out, diags
}
