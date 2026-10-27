package provider

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
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
	_ datasource.DataSource              = (*webhookDeliveriesDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*webhookDeliveriesDataSource)(nil)
)

const webhookDeliveriesDataSourceSuffix = "webhook_deliveries"

// The bounds on how much of a delivery history one read takes.
//
// A history is a log, not a catalog: it grows with every event the endpoint
// is sent, and the contract gives no retention. So this reads the newest
// deliveries rather than every page, which would fail on the provider's own
// page bound once an endpoint had been busy long enough.
const (
	// webhookDeliveriesDefault is the contract's default page, so a
	// configuration that sets nothing sees what the platform shows first.
	webhookDeliveriesDefault = 50
	// webhookDeliveriesMax bounds one read at five full pages.
	webhookDeliveriesMax = 1000
	// webhookDeliveryPageSize is the contract's largest `limit`. A larger one
	// is a 400, not a clamp.
	webhookDeliveryPageSize = 200
)

func newWebhookDeliveriesDataSource() datasource.DataSource {
	return &webhookDeliveriesDataSource{}
}

// webhookDeliveriesDataSource is one subscription's recent delivery attempts,
// newest first: what the subscription's rolled-up health doesn't say, which
// deliveries failed, what the endpoint answered, and whether a retry is still
// coming.
//
// # What it carries
//
// Every field the contract gives a delivery. None is a credential: a delivery
// has no request or response body, no headers and no signature, and the
// signing secret is never part of it. `last_error` is the platform's own text
// for a failure, kept as it's reported, the way `state_message` is on the
// objects that have one.
//
// # Why it reads the subscription list first
//
// The operation answers an empty page, not a 404, for an id the caller's
// organization doesn't have. A mistyped id would then read as an endpoint
// with no deliveries, forever. The list is the one this run shares across
// subscription lookups (webhook_list_memo.go), so the check costs at most one
// read every webhookListFresh.
type webhookDeliveriesDataSource struct{ meta *Meta }

type webhookDeliveriesModel struct {
	SubscriptionID types.String           `tfsdk:"subscription_id"`
	Limit          types.Int64            `tfsdk:"limit"`
	Status         types.String           `tfsdk:"status"`
	Deliveries     []webhookDeliveryModel `tfsdk:"deliveries"`
}

// webhookDeliveryModel is one delivery attempt.
type webhookDeliveryModel struct {
	ID             types.String `tfsdk:"id"`
	Status         types.String `tfsdk:"status"`
	Attempts       types.Int64  `tfsdk:"attempts"`
	ResponseCode   types.Int64  `tfsdk:"response_code"`
	LastError      types.String `tfsdk:"last_error"`
	CreatedAt      types.String `tfsdk:"created_at"`
	SettledAt      types.String `tfsdk:"settled_at"`
	NextAttemptAt  types.String `tfsdk:"next_attempt_at"`
	RetryScheduled types.Bool   `tfsdk:"retry_scheduled"`
}

// webhookDeliveryModelFrom reports `next_attempt_at` only while a retry is
// scheduled. The platform keeps a time on every delivery and doesn't clear it
// when the delivery settles: when it was queued, when a worker's claim on it
// lapses, or when its last retry was due. `retry_scheduled` is the platform's
// own rule for when the time is a retry's.
func webhookDeliveryModelFrom(row k0rdent.WebhookDelivery) webhookDeliveryModel {
	next := types.StringNull()
	if row.RetryScheduled {
		next = timeOrNull(row.NextAttemptAt)
	}
	return webhookDeliveryModel{
		ID:             types.StringValue(row.Id.String()),
		Status:         types.StringValue(string(row.Status)),
		Attempts:       types.Int64Value(int64(row.Attempts)),
		ResponseCode:   int64OrNull(row.ResponseCode),
		LastError:      stringOrNull(row.LastError),
		CreatedAt:      timeOrNull(&row.CreatedAt),
		SettledAt:      timeOrNull(row.SettledAt),
		NextAttemptAt:  next,
		RetryScheduled: types.BoolValue(row.RetryScheduled),
	}
}

func (d *webhookDeliveriesDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(webhookDeliveriesDataSourceSuffix)
}

func (d *webhookDeliveriesDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One webhook subscription's recent delivery attempts, newest first.\n\n" +
			"The subscription's own `consecutive_failures` and `failing_since` say whether the " +
			"endpoint is failing. This says which deliveries failed, what the endpoint answered, " +
			"and whether the platform will try again: a `failed` delivery with `retry_scheduled` " +
			"false has been dead-lettered and won't be retried.\n\n" +
			"`status` applies to the newest `limit` deliveries only: an older delivery with that " +
			"status isn't returned.\n\n" +
			"A delivery carries no request or response body, no headers, no signature and never " +
			"the signing secret.",
		Attributes: map[string]schema.Attribute{
			"subscription_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "The subscription's id, as the `id` of the resource or either data source reports it.",
				Validators:          []validator.String{subscriptionID{}},
			},
			"limit": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: fmt.Sprintf("How many of the newest deliveries to read, across as many "+
					"pages as that takes. %d when unset, the platform's own default page.", webhookDeliveriesDefault),
				Validators: []validator.Int64{int64validator.Between(1, webhookDeliveriesMax)},
			},
			"status": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Return only the deliveries whose `status` is this value. Filtered in " +
					"the provider, among the newest `limit` deliveries: the list operation takes no such filter.",
				Validators: []validator.String{oneOfEnum(k0rdent.WebhookDeliveryStatusValues())},
			},
			"deliveries": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The deliveries, newest first, as the platform orders them.",
				NestedObject: schema.NestedAttributeObject{Attributes: map[string]schema.Attribute{
					"id": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "The delivery's id, a UUID.",
					},
					"status": schema.StringAttribute{
						Computed: true,
						MarkdownDescription: "One of " + enumProse(k0rdent.WebhookDeliveryStatusValues()) + ". " +
							"`failed` covers a delivery still being retried and one dead-lettered: " +
							"`retry_scheduled` tells them apart.",
					},
					"attempts": schema.Int64Attribute{
						Computed: true,
						MarkdownDescription: "How many times the platform has tried this delivery, counting a try " +
							"that failed on the platform's side before anything was sent.",
					},
					"response_code": schema.Int64Attribute{
						Computed:            true,
						MarkdownDescription: "The HTTP status the endpoint answered the last attempt with. Null when none was received.",
					},
					"last_error": schema.StringAttribute{
						Computed: true,
						MarkdownDescription: "The platform's account of the last failed attempt. Null when no " +
							"attempt has failed, and once the delivery succeeds, even after failed attempts. " +
							"The platform never reads the endpoint's answer, so no response body is here. A " +
							"connection failure is reported in the network's own words, which can name the " +
							"endpoint's URL, as `url` on the subscription does, or a URL the endpoint redirected to.",
					},
					"created_at": schema.StringAttribute{
						Computed:            true,
						MarkdownDescription: "When the delivery was queued, as RFC 3339.",
					},
					"settled_at": schema.StringAttribute{
						Computed: true,
						MarkdownDescription: "When the delivery was delivered or dead-lettered, as RFC 3339. Null " +
							"while it's queued or being retried.",
					},
					"next_attempt_at": schema.StringAttribute{
						Computed: true,
						MarkdownDescription: "When the next retry is due, as RFC 3339. Null unless " +
							"`retry_scheduled` is true.",
					},
					"retry_scheduled": schema.BoolAttribute{
						Computed: true,
						MarkdownDescription: "Whether a failed delivery will be retried. False on a `failed` " +
							"delivery means it was dead-lettered and won't be tried again.",
					},
				}},
			},
		},
	}
	documentDataSourceSchema(&resp.Schema)
}

func (d *webhookDeliveriesDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *webhookDeliveriesDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data webhookDeliveriesModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() || !providerConfigured(d.meta, &resp.Diagnostics) {
		return
	}
	id := data.SubscriptionID.ValueString()
	_, found, diags := findWebhookSubscription(ctx, d.meta, id)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		// Local: the list answered without it.
		resp.Diagnostics.AddAttributeError(path.Root("subscription_id"), "No such webhook subscription",
			fmt.Sprintf("The subscriptions this credential's organization has don't include %q.", id))
		return
	}
	subscription, err := uuid.Parse(id)
	if err != nil {
		// Local: the validator lets only a UUID through, so this is a bug.
		resp.Diagnostics.AddAttributeError(path.Root("subscription_id"), "Not a subscription id", err.Error())
		return
	}
	want := webhookDeliveriesDefault
	if !data.Limit.IsNull() {
		want = int(data.Limit.ValueInt64())
	}
	rows, ok := d.newest(ctx, subscription, want, &resp.Diagnostics)
	if !ok {
		return
	}

	data.Deliveries = []webhookDeliveryModel{}
	for _, row := range rows {
		if !data.Status.IsNull() && string(row.Status) != data.Status.ValueString() {
			continue
		}
		data.Deliveries = append(data.Deliveries, webhookDeliveryModelFrom(row))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// newest reads the subscription's newest want deliveries, following the
// cursor until it has them or the history ends. On false, diags says why and
// nothing is returned.
//
// The contract doesn't promise a page holds `limit` rows, so the bound is on
// rows rather than pages: every page read has to add a delivery not already
// read, or the read stops there. That also keeps once a delivery read twice,
// which a keyset cursor the contract calls best-effort can hand back.
func (d *webhookDeliveriesDataSource) newest(ctx context.Context, id openapi_types.UUID, want int, diags *diag.Diagnostics) ([]k0rdent.WebhookDelivery, bool) {
	const noun = "webhook deliveries"
	var (
		all    []k0rdent.WebhookDelivery
		cursor *string
	)
	for len(all) < want {
		size := min(webhookDeliveryPageSize, want-len(all))
		got, err := d.meta.Client.ListWebhookDeliveriesWithResponse(ctx, id,
			&k0rdent.ListWebhookDeliveriesParams{Limit: &size, Cursor: cursor})
		if err != nil {
			addListError(diags, noun, err)
			return nil, false
		}
		if got.JSON200 == nil {
			addListError(diags, noun, unreadPage(got.HTTPResponse, got.Body))
			return nil, false
		}
		read := len(all)
		all = firstOfEach(append(all, got.JSON200.Deliveries...),
			func(row k0rdent.WebhookDelivery) string { return row.Id.String() })
		next := got.JSON200.NextCursor
		if len(all) == read || next == nil || *next == "" {
			break
		}
		cursor = next
	}
	if len(all) > want {
		all = all[:want]
	}
	return all, true
}
