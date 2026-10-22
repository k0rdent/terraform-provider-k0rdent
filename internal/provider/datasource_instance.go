package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*instanceDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*instanceDataSource)(nil)
)

const instanceDataSourceSuffix = "instance"

func newInstanceDataSource() datasource.DataSource { return &instanceDataSource{} }

// instanceDataSource reads one instance by id, through getInstance. It carries
// exactly what one row of `instances` carries, from the same mapping, so the
// two cannot describe a host differently.
type instanceDataSource struct{ meta *Meta }

type instanceDataSourceModel struct {
	Region  types.String `tfsdk:"region"`
	Project types.String `tfsdk:"project"`
	instanceEntryModel
}

func (d *instanceDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(instanceDataSourceSuffix)
}

func (d *instanceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := instanceAttributes()
	attrs["id"] = schema.StringAttribute{
		Required: true,
		MarkdownDescription: "Instance id, as `" + ResourceName(instancesDataSourceSuffix) +
			"` reports it.",
		Validators: []validator.String{urlSafeIdentifier{}, stringvalidator.RegexMatches(instanceIDPattern,
			"must be inst- and 32 lower-case hex digits, as the instances data source reports it")},
	}
	attrs["region"] = dataSourceRegionAttribute()
	attrs["project"] = dataSourceProjectAttribute()
	resp.Schema = schema.Schema{
		MarkdownDescription: "One instance, a host an instance group holds, read by id.\n\n" +
			"A deleted instance is an error here, as the platform answers 410 for it. Its " +
			"`network_interfaces` carry the `mac_address` or `guid` that narrows a `" +
			ResourceName(fileStoreResourceSuffix) + "` grant to one interface.",
		Attributes: attrs,
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *instanceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *instanceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data instanceDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	got, err := d.meta.Client.GetInstanceWithResponse(ctx, target.region, target.project, data.ID.ValueString(), nil)
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(&resp.Diagnostics, "read the instance", httpResp, body, err)
		return
	}

	id := data.ID
	data.instanceEntryModel = instanceEntryOf(*got.JSON200)
	// The id is the one asked for, should the response leave it out.
	if data.ID.IsNull() {
		data.ID = id
	}
	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
