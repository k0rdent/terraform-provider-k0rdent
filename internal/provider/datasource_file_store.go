package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ datasource.DataSource              = (*fileStoreDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*fileStoreDataSource)(nil)
)

const fileStoreDataSourceSuffix = "file_store"

func newFileStoreDataSource() datasource.DataSource { return &fileStoreDataSource{} }

// fileStoreDataSource reads a file store this configuration does not manage,
// so a boundary that holds the hosts can name a store another one owns.
//
// The desired consumer list is absent: it is the owner's configuration, and
// the other side wants what is enforced, which is `consumer_status`. The
// messages and `reconciling` are here and not on the resource because they
// move without the configuration moving.
type fileStoreDataSource struct{ meta *Meta }

type fileStoreDataSourceModel struct {
	ID      types.String `tfsdk:"id"`
	Region  types.String `tfsdk:"region"`
	Project types.String `tfsdk:"project"`

	ShareType   types.String `tfsdk:"share_type"`
	DisplayName types.String `tfsdk:"display_name"`
	Description types.String `tfsdk:"description"`
	SelfLink    types.String `tfsdk:"self_link"`
	CapacityGiB types.Int64  `tfsdk:"capacity_gib"`

	State        types.String `tfsdk:"state"`
	StateReason  types.String `tfsdk:"state_reason"`
	StateMessage types.String `tfsdk:"state_message"`
	Reconciling  types.Bool   `tfsdk:"reconciling"`

	ExportLocations types.List   `tfsdk:"export_locations"`
	PreferredExport types.Object `tfsdk:"preferred_export_location"`
	ConsumerStatus  types.Map    `tfsdk:"consumer_status"`
}

type fileStoreDataConsumerModel struct {
	State             types.String `tfsdk:"state"`
	StateReason       types.String `tfsdk:"state_reason"`
	StateMessage      types.String `tfsdk:"state_message"`
	ResolvedAddresses types.Set    `tfsdk:"resolved_addresses"`
}

func fileStoreDataConsumerType() attr.Type {
	return types.ObjectType{AttrTypes: map[string]attr.Type{
		"state":              types.StringType,
		"state_reason":       types.StringType,
		"state_message":      types.StringType,
		"resolved_addresses": types.SetType{ElemType: types.StringType},
	}}
}

func (d *fileStoreDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(fileStoreDataSourceSuffix)
}

func (d *fileStoreDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	exportAttrs := map[string]schema.Attribute{
		"path":     schema.StringAttribute{Computed: true, MarkdownDescription: "Export path on the server."},
		"server":   schema.StringAttribute{Computed: true, MarkdownDescription: "Address of the storage server."},
		"protocol": schema.StringAttribute{Computed: true, MarkdownDescription: "Protocol the export is served over."},
		"options":  schema.StringAttribute{Computed: true, MarkdownDescription: "Mount options the platform recommends."},
	}
	listedExportAttrs := map[string]schema.Attribute{
		"preferred": schema.BoolAttribute{Computed: true, MarkdownDescription: "Whether this is the location to prefer."},
		"admin_only": schema.BoolAttribute{
			Computed:            true,
			MarkdownDescription: "Whether this location is meant for the operator rather than for tenant hosts.",
		},
	}
	for k, v := range exportAttrs {
		listedExportAttrs[k] = v
	}

	resp.Schema = schema.Schema{
		MarkdownDescription: "One file store, read rather than managed.\n\n" +
			"Use it to name a store another configuration owns, from a state boundary that holds " +
			"the things mounting it. `consumer_status` is what to read on that side: it is what " +
			"the backing storage is actually enforcing rather than what was asked for, so a host " +
			"can mount once its entry reads `active` and not before.\n\n" +
			"The desired consumer list is not returned. That belongs to whoever manages the store.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "File store name.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"region":  dataSourceRegionAttribute(),
			"project": dataSourceProjectAttribute(),
			"share_type": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The share type this store was provisioned from.",
			},
			"display_name": schema.StringAttribute{Computed: true, MarkdownDescription: "Human-friendly name, for display only."},
			"description":  schema.StringAttribute{Computed: true, MarkdownDescription: "Free-form text describing it, for people to read."},
			"self_link": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Platform-defined reference to this store.",
			},
			"capacity_gib": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Declared capacity, in binary gibibytes. It is what the store is " +
					"accounted at rather than a hard quota: the storage behind this API does not " +
					"enforce it, so a store can consume beyond it.",
			},
			"state": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Lifecycle state. `active` is settled and mountable.",
			},
			"state_reason": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Machine-readable cause of the current state, when the platform gives one.",
			},
			"state_message": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The platform's own explanation of the current state. Read it, do not branch on it.",
			},
			"reconciling": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the platform is still bringing the store into line with " +
					"what was asked for.",
			},
			"export_locations": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "Where the store can be mounted from, sorted by `server` and " +
					"then `path`. To mount, use `preferred_export_location`.",
				NestedObject: schema.NestedAttributeObject{Attributes: listedExportAttrs},
			},
			"preferred_export_location": schema.SingleNestedAttribute{
				Computed: true,
				MarkdownDescription: "The location a tenant host should mount: the one the platform " +
					"marks preferred and not operator-only. Null until the store is active. The " +
					"mount source is `<server>:<path>`.",
				Attributes: exportAttrs,
			},
			"consumer_status": schema.MapNestedAttribute{
				Computed: true,
				MarkdownDescription: "What the backing storage is enforcing, keyed by consumer `ref`. " +
					"A superset of the configured consumers while a revocation is in flight.\n\n" +
					"A `failed` entry whose `state_reason` is `GROUP_MEMBERSHIP_DRIFTED` still covers " +
					"the machines it was written against, so it is not necessarily an outage.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"state": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "`creating`, `active`, `deleting` or `failed`.",
						},
						"state_reason": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Machine-readable cause, upper-cased. Stable enough to branch on.",
						},
						"state_message": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "Why this consumer is in this state. Read it, do not branch on it.",
						},
						"resolved_addresses": schema.SetAttribute{
							Computed:    true,
							ElementType: types.StringType,
							MarkdownDescription: "The addresses this consumer resolved to, which is " +
								"what answers which machines the grant actually reached. IPv4 only.",
						},
					},
				},
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *fileStoreDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *fileStoreDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data fileStoreDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(d.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}

	got, err := d.meta.Client.GetFileStoreWithResponse(ctx, target.region, target.project, data.ID.ValueString())
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(&resp.Diagnostics, "read the file store", httpResp, body, err)
		return
	}
	f := *got.JSON200

	// The resource's mapping, so exports sort and pick the preferred one the
	// same way on both sides of a boundary.
	shared, diags := fileStoreStateFrom(ctx, f, fileStoreModel{Consumers: types.MapNull(fileStoreConsumerType())})
	resp.Diagnostics.Append(diags...)

	data.Region = types.StringValue(target.region)
	data.Project = types.StringValue(target.project)
	data.ShareType = shared.ShareType
	data.DisplayName = stringOrNull(f.DisplayName)
	data.Description = stringOrNull(f.Description)
	data.SelfLink = shared.SelfLink
	data.CapacityGiB = shared.CapacityGiB
	data.State = shared.State
	data.StateMessage = stringOrNull(f.StateMessage)
	data.Reconciling = boolOrNull(f.Reconciling)
	data.StateReason = types.StringNull()
	if f.StateReason != nil {
		data.StateReason = types.StringValue(string(*f.StateReason))
	}
	data.ExportLocations = shared.ExportLocations
	data.PreferredExport = shared.PreferredExport

	m, ds := fileStoreDataConsumers(ctx, f)
	resp.Diagnostics.Append(ds...)
	data.ConsumerStatus = m

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

func fileStoreDataConsumers(ctx context.Context, f k0rdent.FileStore) (types.Map, diag.Diagnostics) {
	var diags diag.Diagnostics
	statuses := map[string]fileStoreDataConsumerModel{}
	if f.ConsumerStatus != nil {
		for _, c := range *f.ConsumerStatus {
			// Nothing to key it by, and nothing it could be matched against.
			if deref(c.Ref) == "" {
				continue
			}
			addresses, ds := stringSetOrNull(ctx, c.ResolvedAddresses)
			diags.Append(ds...)
			entry := fileStoreDataConsumerModel{
				State:             types.StringNull(),
				StateReason:       types.StringNull(),
				StateMessage:      stringOrNull(c.StateMessage),
				ResolvedAddresses: addresses,
			}
			if c.State != nil {
				entry.State = types.StringValue(string(*c.State))
			}
			if c.StateReason != nil {
				entry.StateReason = types.StringValue(string(*c.StateReason))
			}
			statuses[*c.Ref] = entry
		}
	}
	m, ds := types.MapValueFrom(ctx, fileStoreDataConsumerType(), statuses)
	diags.Append(ds...)
	return m, diags
}
