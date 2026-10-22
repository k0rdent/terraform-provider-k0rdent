package provider

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ datasource.DataSource              = (*clusterTypeVersionsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*clusterTypeVersionsDataSource)(nil)
	_ datasource.DataSource              = (*clusterTypeVersionDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*clusterTypeVersionDataSource)(nil)
)

const (
	clusterTypeVersionsDataSourceSuffix = "cluster_type_versions"
	clusterTypeVersionDataSourceSuffix  = "cluster_type_version"
)

func newClusterTypeVersionsDataSource() datasource.DataSource {
	return &clusterTypeVersionsDataSource{}
}
func newClusterTypeVersionDataSource() datasource.DataSource {
	return &clusterTypeVersionDataSource{}
}

// ---------------------------------------------------------------------------
// The history
// ---------------------------------------------------------------------------

// clusterTypeVersionsDataSource reads a ClusterType's version history, oldest
// first, and fans out to getClusterTypeVersion once per entry because the list
// record carries neither `k0sVersion` nor a version-scoped `selfLink`. A version
// history is bounded; the same shape would be wrong on a cluster list.
type clusterTypeVersionsDataSource struct{ meta *Meta }

type clusterTypeVersionsModel struct {
	ClusterType      types.String                   `tfsdk:"cluster_type"`
	Region           types.String                   `tfsdk:"region"`
	Status           types.String                   `tfsdk:"status"`
	DisplayNameRegex types.String                   `tfsdk:"display_name_regex"`
	Versions         []clusterTypeVersionEntryModel `tfsdk:"versions"`
}

type clusterTypeVersionEntryModel struct {
	VersionID   types.String `tfsdk:"version_id"`
	Version     types.Int64  `tfsdk:"version"`
	DisplayName types.String `tfsdk:"display_name"`
	Description types.String `tfsdk:"description"`
	Status      types.String `tfsdk:"status"`
	CreatedAt   types.String `tfsdk:"created_at"`
	K8sVersion  types.String `tfsdk:"kubernetes_version"`
	SelfLink    types.String `tfsdk:"self_link"`
}

func (d *clusterTypeVersionsDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(clusterTypeVersionsDataSourceSuffix)
}

func (d *clusterTypeVersionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every version of one ClusterType, oldest first.\n\n" +
			"This is the nearest thing the platform has to a catalogue of supported Kubernetes " +
			"versions. A tenant does not choose a Kubernetes version directly: it comes with the " +
			"ClusterType version a cluster is bound to, so the list to read is this one.\n\n" +
			"Each entry's `self_link` is version-scoped, which is what pins a cluster to an " +
			"immutable version instead of to whatever the latest one becomes.",
		Attributes: map[string]schema.Attribute{
			"cluster_type": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "ClusterType slug whose history to read.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"region": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Region to read. Defaults to the provider's `region`.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"status": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Return only versions with this lifecycle status: " +
					enumProse(k0rdent.ClusterTypeVersionStatusValues()) + ". Omit it for all of them. " +
					"Only `available` can back a new cluster. Filtered by the platform.",
				// The platform answers 422 rather than an empty list for a
				// status it does not know, which is a value worth catching in
				// the plan.
				Validators: []validator.String{oneOfEnum(k0rdent.ClusterTypeVersionStatusValues())},
			},
			"display_name_regex": func() schema.StringAttribute {
				a := displayNameRegexAttribute("versions")
				a.MarkdownDescription += " It runs before each kept version is read for its " +
					"`kubernetes_version`, so a narrow pattern also saves those reads."
				return a
			}(),
			// A List and the order is meaningful, unlike the ClusterType
			// catalog: oldest first, so `versions[0]` is the first version and
			// the last entry is the newest.
			"versions": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "The history, oldest first. Sorted by `version`, then " +
					"`version_id`. A new version appends, but a `status` filter shifts every " +
					"index, so reach an entry with `for_each` over `version_id` rather than " +
					"`versions[0]`.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"version_id": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "UUID of this version. Stable, and what pins a reference.",
						},
						"version": schema.Int64Attribute{
							Computed:            true,
							MarkdownDescription: "Sequential version number, for display.",
						},
						"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-readable name, for display only."},
						"description":  schema.StringAttribute{Computed: true, MarkdownDescription: "Free-form text describing it, for people to read, when one was set."},
						"status": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "`available`, `draft` or `deprecated`. The progression is " +
								"forward-only and only `available` can back a new cluster.",
						},
						"created_at": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "When this version was created, as RFC 3339. This is a row " +
								"timestamp, not a support commitment: the platform publishes no " +
								"end-of-support date for a version.",
						},
						"kubernetes_version": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "The k0s version this version of the type provisions, as " +
								"`vX.Y.Z`. Null for a ClusterType that is not Kubernetes-flavoured. Two " +
								"entries can carry the same one: a new ClusterType version may change " +
								"node pools or CIDRs and leave Kubernetes where it was.",
						},
						"self_link": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "Version-scoped reference, suitable for a cluster's " +
								"`cluster_type`. Binds this exact version, so a new one being published " +
								"does not move the cluster.",
						},
					},
				},
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *clusterTypeVersionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *clusterTypeVersionsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data clusterTypeVersionsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	region, ok := resolveRegion(d.meta, data.Region, &resp.Diagnostics)
	if !ok {
		return
	}
	clusterType := data.ClusterType.ValueString()

	filter, diags := readObjectFilter(ctx, types.MapNull(types.StringType), data.DisplayNameRegex, types.StringNull())
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	var params k0rdent.ListClusterTypeVersionsParams
	if !data.Status.IsNull() && !data.Status.IsUnknown() {
		status := k0rdent.ClusterTypeVersionStatus(data.Status.ValueString())
		params.Status = &status
	}

	// No pagination on this operation, unlike every other catalog read here.
	// The contract declares neither a cursor nor an offset, so one call is the
	// whole history and a paging loop would be inventing a parameter.
	got, err := d.meta.Client.ListClusterTypeVersionsWithResponse(ctx, region, clusterType, &params)
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(&resp.Diagnostics, "list the cluster type's versions", httpResp, body, err)
		return
	}

	data.Versions = []clusterTypeVersionEntryModel{}
	for _, v := range got.JSON200.Versions {
		// Before readVersion, which costs a request per version kept.
		if !filter.keeps(filterSubject{displayName: &v.DisplayName}) {
			continue
		}
		entry := clusterTypeVersionEntryModel{
			Version:     types.Int64Null(),
			VersionID:   types.StringNull(),
			DisplayName: types.StringValue(v.DisplayName),
			Description: stringOrNull(v.Description),
			Status:      types.StringValue(string(v.Status)),
			CreatedAt:   types.StringNull(),
			K8sVersion:  types.StringNull(),
			SelfLink:    types.StringNull(),
		}
		if v.Version != nil {
			entry.Version = types.Int64Value(int64(*v.Version))
		}
		if v.CreatedAt != nil {
			entry.CreatedAt = types.StringValue(v.CreatedAt.Format(time.RFC3339))
		}
		if v.VersionId != nil {
			entry.VersionID = types.StringValue(v.VersionId.String())

			snapshot, ok := d.meta.readVersion(ctx, region, clusterType, *v.VersionId, &resp.Diagnostics)
			if !ok {
				return
			}
			entry.SelfLink = stringOrNull(snapshot.SelfLink)
			if snapshot.K8s != nil {
				entry.K8sVersion = types.StringValue(snapshot.K8s.K0sVersion)
			}
		}
		data.Versions = append(data.Versions, entry)
	}

	// The API promises no order, and dependents correlate elements by index.
	sort.SliceStable(data.Versions, func(i, j int) bool {
		a, b := data.Versions[i], data.Versions[j]
		if a.Version.ValueInt64() != b.Version.ValueInt64() {
			return a.Version.ValueInt64() < b.Version.ValueInt64()
		}
		return a.VersionID.ValueString() < b.VersionID.ValueString()
	})

	data.Region = types.StringValue(region)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// readVersion fetches one pinned snapshot. Shared by both data sources because
// the list needs it per entry and the singular one is nothing else.
func (m *Meta) readVersion(ctx context.Context, region, clusterType string, versionID openapi_types.UUID, diags *diag.Diagnostics) (k0rdent.ClusterType, bool) {
	got, err := m.Client.GetClusterTypeVersionWithResponse(ctx, region, clusterType, versionID)
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(diags, "read that version of the cluster type", httpResp, body, err)
		return k0rdent.ClusterType{}, false
	}
	return *got.JSON200, true
}

// boundKubernetesVersion is the Kubernetes version that the ClusterType version
// ref names carries, for a cluster bound to it. The cluster read doesn't carry
// one: the platform versions a cluster by its ClusterType version, and the
// version's own `k8s.k0sVersion` is where the number is.
//
// A reference with no version in it, a BMaaS-only version and a version the
// platform no longer finds all read null, because none of them is a
// Kubernetes version to report. A read that fails otherwise reads null too,
// with a warning: the attribute is a reading, and losing it is no reason to
// fail the refresh, or the read that records a failed upgrade step.
func (m *Meta) boundKubernetesVersion(ctx context.Context, region, ref string, diags *diag.Diagnostics) types.String {
	typeID, versionID := refTypeAndVersion(ref)
	id, err := uuid.Parse(versionID)
	if typeID == "" || err != nil {
		return types.StringNull()
	}
	got, err := m.Client.GetClusterTypeVersionWithResponse(ctx, region, typeID, id)
	var failed diag.Diagnostics
	switch {
	case err != nil:
		addAPIError(&failed, "read the cluster's version of the cluster type", nil, nil, err)
	case isNotFound(got.HTTPResponse, got.Body):
		return types.StringNull()
	case got.JSON200 == nil:
		addAPIError(&failed, "read the cluster's version of the cluster type", got.HTTPResponse, got.Body, nil)
	case got.JSON200.K8s == nil || got.JSON200.K8s.K0sVersion == "":
		return types.StringNull()
	default:
		return types.StringValue(got.JSON200.K8s.K0sVersion)
	}
	for _, e := range failed.Errors() {
		diags.AddWarning("Could not read the cluster's Kubernetes version",
			"Reading the ClusterType version the cluster is bound to failed, so `kubernetes_version` "+
				"reads null. Nothing else in state depends on it.\n\n"+e.Summary()+"\n\n"+e.Detail())
	}
	return types.StringNull()
}

// ---------------------------------------------------------------------------
// One pinned version
// ---------------------------------------------------------------------------

type clusterTypeVersionDataSource struct{ meta *Meta }

type clusterTypeVersionModel struct {
	ClusterType types.String                `tfsdk:"cluster_type"`
	VersionID   types.String                `tfsdk:"version_id"`
	Region      types.String                `tfsdk:"region"`
	DisplayName types.String                `tfsdk:"display_name"`
	Description types.String                `tfsdk:"description"`
	Status      types.String                `tfsdk:"status"`
	Version     types.Int64                 `tfsdk:"version"`
	SelfLink    types.String                `tfsdk:"self_link"`
	K8sVersion  types.String                `tfsdk:"kubernetes_version"`
	NodePools   []clusterTypePoolEntryModel `tfsdk:"node_pools"`
}

func (d *clusterTypeVersionDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(clusterTypeVersionDataSourceSuffix)
}

func (d *clusterTypeVersionDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One version of a ClusterType, as it was at that version.\n\n" +
			"The difference from `" + ResourceName(clusterTypeDataSourceSuffix) + "` is that this " +
			"reads a named version rather than the latest, and its `self_link` is version-scoped. " +
			"A cluster created against that link stays on this version when a newer one is " +
			"published; one created against the head link does not.",
		Attributes: map[string]schema.Attribute{
			"cluster_type": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The ClusterType's id, its slug in the region's catalog.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"version_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "UUID of the version to read, from `" +
					ResourceName(clusterTypeVersionsDataSourceSuffix) + "`.",
				Validators: []validator.String{versionID{}},
			},
			"region": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Region to read. Defaults to the provider's `region`.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-readable name, for display only."},
			"description":  schema.StringAttribute{Computed: true, MarkdownDescription: "Free-form text describing it, for people to read, when one was set."},
			"status": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Lifecycle status of this version. A `deprecated` or `draft` " +
					"version cannot back a new cluster, and this is the attribute to assert on " +
					"before pinning to one.",
			},
			"version": schema.Int64Attribute{
				Computed:            true,
				MarkdownDescription: "Sequential version number, for display.",
			},
			"self_link": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Version-scoped reference, for a cluster's `cluster_type`. This is " +
					"the pinned form.",
			},
			"kubernetes_version": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "The k0s version this version provisions, as `vX.Y.Z`. Null for a " +
					"ClusterType that is not Kubernetes-flavoured.",
			},
			// A List, and position is not arbitrary: pools are keyed by `id`
			// and a module selects on `role`, never on index. Same reasoning as
			// the head-version data source.
			"node_pools": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "Node pools as they were defined at this version. Sorted by `id`.",
				NestedObject:        schema.NestedAttributeObject{Attributes: clusterTypePoolAttributes()},
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *clusterTypeVersionDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *clusterTypeVersionDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data clusterTypeVersionModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	region, ok := resolveRegion(d.meta, data.Region, &resp.Diagnostics)
	if !ok {
		return
	}

	versionID, err := uuid.Parse(data.VersionID.ValueString())
	if err != nil {
		// Local: configuration validation, no request made. A malformed id is a
		// 400 whose message names the path parameter rather than the attribute,
		// and it costs a round trip to get it.
		resp.Diagnostics.AddAttributeError(path.Root("version_id"), "Not a version id",
			fmt.Sprintf("%q is not a UUID. Version ids come from `%s`.",
				data.VersionID.ValueString(), ResourceName(clusterTypeVersionsDataSourceSuffix)))
		return
	}

	ct, ok := d.meta.readVersion(ctx, region, data.ClusterType.ValueString(), versionID, &resp.Diagnostics)
	if !ok {
		return
	}

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

	// Sorted on the pool id, because the response order is not promised.
	sort.SliceStable(data.NodePools, func(i, j int) bool {
		return data.NodePools[i].ID.ValueString() < data.NodePools[j].ID.ValueString()
	})

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
