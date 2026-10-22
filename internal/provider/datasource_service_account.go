package provider

import (
	"context"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ datasource.DataSource              = (*serviceAccountDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*serviceAccountDataSource)(nil)
)

const serviceAccountDataSourceSuffix = "service_account"

func newServiceAccountDataSource() datasource.DataSource { return &serviceAccountDataSource{} }

// serviceAccountDataSource reads one service account this configuration does
// not manage.
//
// The case it's for is the pipeline whose identity another configuration
// owns: the configuration that issues its credential, or the one that checks
// it's still `active` before an apply that depends on it, needs the account's
// `client_id` and status without taking over its destroy.
//
// It carries what the resource leaves out because it moves without a write:
// `active_credential_count` drops when a credential lapses, and `updated_at`
// with every change anyone makes. A data source is re-read every run and
// compared against nothing, so neither is noise here.
type serviceAccountDataSource struct{ meta *Meta }

type serviceAccountDataSourceModel struct {
	ID             types.String `tfsdk:"id"`
	OrganizationID types.String `tfsdk:"organization_id"`

	DisplayName           types.String `tfsdk:"display_name"`
	Description           types.String `tfsdk:"description"`
	Scope                 types.String `tfsdk:"scope"`
	ScopeID               types.String `tfsdk:"scope_id"`
	Roles                 types.Set    `tfsdk:"roles"`
	Status                types.String `tfsdk:"status"`
	ClientID              types.String `tfsdk:"client_id"`
	ActiveCredentialCount types.Int64  `tfsdk:"active_credential_count"`
	CreatedBy             types.String `tfsdk:"created_by"`
	CreatedAt             types.String `tfsdk:"created_at"`
	UpdatedAt             types.String `tfsdk:"updated_at"`
	SelfLink              types.String `tfsdk:"self_link"`
}

func (d *serviceAccountDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(serviceAccountDataSourceSuffix)
}

func (d *serviceAccountDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One service account, read rather than managed.\n\n" +
			"Use it to hand a pipeline's `client_id` to the configuration that needs it, or to check the " +
			"account is still `active`, without putting its destroy in this state file. Like the resource, " +
			"it takes no `region` and no `project`, and is addressed inside an organization. Unlike a " +
			"project's, the organization can't be looked up from the id: the platform lists service " +
			"accounts only within a scope.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Service account id.",
				Validators:          idValidators(),
			},
			"organization_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The organization the service account is in.",
				Validators:          idValidators(),
			},
			"display_name": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Human-friendly name the account is shown under.",
			},
			"description": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "What the account is for, if anyone said.",
			},
			"scope": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The level its roles apply at: `organization`, `folder` or `project`.",
			},
			"scope_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The organization, folder or project `scope` names.",
			},
			"roles": schema.SetAttribute{
				Computed:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Role slugs bound to the account at its scope.",
			},
			"status": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "`active`, or `disabled`, in which case no credential of it mints a " +
					"token and its outstanding tokens were revoked when it was disabled.",
			},
			"client_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The OAuth2 client id the account authenticates with.",
			},
			"active_credential_count": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "Credentials that haven't expired, of the five an account can hold. " +
					"It drops on its own as credentials lapse.",
			},
			"created_by": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The user id of the administrator who created it.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When it was created, as RFC 3339.",
			},
			"updated_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When it last changed, as RFC 3339.",
			},
			"self_link": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Platform-defined reference to this service account.",
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *serviceAccountDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *serviceAccountDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data serviceAccountDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() || !providerConfigured(d.meta, &resp.Diagnostics) {
		return
	}

	got, err := d.meta.Client.GetServiceAccountWithResponse(ctx, data.OrganizationID.ValueString(), data.ID.ValueString())
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		// A 404 is an error here: a data source naming something absent is
		// a broken configuration, not drift to reconcile.
		addAPIError(&resp.Diagnostics, "read the service account", httpResp, body, err)
		return
	}
	sa := *got.JSON200

	data.DisplayName = types.StringValue(sa.DisplayName)
	data.Description = stringOrNull(sa.Description)
	data.Scope = types.StringValue(string(sa.Scope))
	data.ScopeID = types.StringValue(sa.ScopeId)
	data.Status = enumOrNull(sa.Status)
	data.ClientID = stringOrNull(sa.ClientId)
	data.ActiveCredentialCount = int64OrNull(sa.ActiveCredentialCount)
	data.CreatedBy = stringOrNull(sa.CreatedBy)
	data.CreatedAt = timeOrNull(sa.CreatedAt)
	data.UpdatedAt = timeOrNull(sa.UpdatedAt)
	data.SelfLink = stringOrNull(sa.SelfLink)
	roles, diags := types.SetValueFrom(ctx, types.StringType, rowsOf(sa.Roles))
	resp.Diagnostics.Append(diags...)
	data.Roles = roles

	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}
