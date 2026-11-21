package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ datasource.DataSource              = (*clusterCandidatesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*clusterCandidatesDataSource)(nil)
)

// clusterCandidatesDataSource finds an existing cluster without treating
// absence as an error. Provenance stays on the managed resource, where the
// durable token can be write-only.
type clusterCandidatesDataSource struct{ meta *Meta }

type clusterCandidatesDataSourceModel struct {
	ID       types.String                `tfsdk:"id"`
	State    types.String                `tfsdk:"state"`
	Region   types.String                `tfsdk:"region"`
	Project  types.String                `tfsdk:"project"`
	Clusters []clusterCandidateDataModel `tfsdk:"clusters"`
}

type clusterCandidateDataModel struct {
	ID      types.String `tfsdk:"id"`
	UID     types.String `tfsdk:"uid"`
	State   types.String `tfsdk:"state"`
	Region  types.String `tfsdk:"region"`
	Project types.String `tfsdk:"project"`
}

func newClusterCandidatesDataSource() datasource.DataSource { return &clusterCandidatesDataSource{} }

func (d *clusterCandidatesDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(clusterDataSourceSuffix + "s")
}

func (d *clusterCandidatesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Zero or one Kubernetes cluster with an exact id in the configured region and project. " +
			"An absent or deleting cluster returns an empty list. This lets ordinary `import` and managed resource " +
			"`for_each` blocks skip adoption when the cluster doesn't exist. Provenance stays on the managed resource's " +
			"write-only `create_token`, so this read never receives or stores that token.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The cluster's exact id.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"state":   stateFilterAttribute("clusters", "state", enumStrings(k0rdent.ClusterStateValues()...), false),
			"region":  dataSourceRegionAttribute(),
			"project": dataSourceProjectAttribute(),
			"clusters": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The matching cluster, or an empty list when nothing exists under this id.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "The cluster's exact id.",
						},
						"uid": uidDataSourceAttribute("cluster"),
						"state": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Lifecycle state as last read from the platform.",
						},
						"region":  dataSourceRegionAttribute(),
						"project": dataSourceProjectAttribute(),
					},
				},
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *clusterCandidatesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *clusterCandidatesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data clusterCandidatesDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	data.Clusters = []clusterCandidateDataModel{}
	got, err := d.meta.Client.GetClusterWithResponse(ctx, target.region, target.project, data.ID.ValueString(), nil)
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		if isNotFound(httpResp, body) {
			resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
			return
		}
		addAPIError(&resp.Diagnostics, "read the cluster", httpResp, body, err)
		return
	}
	state := stateString(got.JSON200.State)
	if goingAway(state) || (!data.State.IsNull() && data.State.ValueString() != state) {
		resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
		return
	}
	data.Clusters = append(data.Clusters, clusterCandidateDataModel{
		ID: types.StringValue(data.ID.ValueString()), UID: uidValue(got.JSON200.Uid),
		State:  types.StringValue(state),
		Region: types.StringValue(target.region), Project: types.StringValue(target.project),
	})
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
