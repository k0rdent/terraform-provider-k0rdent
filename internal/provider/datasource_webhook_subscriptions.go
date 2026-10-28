package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// The two read-only forms of a webhook subscription, and the list read both
// share with the resource.
//
// The contract has no read of one subscription. listWebhookSubscriptions
// answers every subscription the caller's organization has, in one page with
// no paging parameters, so finding one is that list filtered by id. It also
// carries each endpoint's delivery health, which is what the singular form is
// for: whether the endpoint is taking deliveries, read from a configuration
// that doesn't own the subscription. None of it would belong in the
// resource, which would plan nothing from it and report it changed on every
// refresh.

var (
	_ datasource.DataSource              = (*webhookSubscriptionDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*webhookSubscriptionDataSource)(nil)
	_ datasource.DataSource              = (*webhookSubscriptionsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*webhookSubscriptionsDataSource)(nil)
)

const (
	webhookSubscriptionDataSourceSuffix  = "webhook_subscription"
	webhookSubscriptionsDataSourceSuffix = "webhook_subscriptions"
)

func newWebhookSubscriptionDataSource() datasource.DataSource {
	return &webhookSubscriptionDataSource{}
}

func newWebhookSubscriptionsDataSource() datasource.DataSource {
	return &webhookSubscriptionsDataSource{}
}

type webhookSubscriptionDataSource struct{ meta *Meta }
type webhookSubscriptionsDataSource struct{ meta *Meta }

// listWebhookSubscriptions reads every subscription the caller can see. A row
// with no id is skipped: nothing can address it, and a model can't hold it.
func listWebhookSubscriptions(ctx context.Context, meta *Meta) ([]k0rdent.WebhookSubscription, diag.Diagnostics) {
	var diags diag.Diagnostics
	got, err := meta.Client.ListWebhookSubscriptionsWithResponse(ctx)
	if err != nil {
		addAPIError(&diags, "list the webhook subscriptions", nil, nil, err)
		return nil, diags
	}
	if got.JSON200 == nil {
		addAPIError(&diags, "list the webhook subscriptions", got.HTTPResponse, got.Body, nil)
		return nil, diags
	}
	// `subscriptions` is required, and `[]` when there are none. A 200
	// without it isn't the list, and reading it as an empty one would drop
	// every subscription from state, so the next apply creates them again.
	if got.JSON200.Subscriptions == nil {
		diags.AddError("Could not list the webhook subscriptions",
			fmt.Sprintf("listWebhookSubscriptions answered %d without `subscriptions`.", got.StatusCode()))
		return nil, diags
	}
	rows := slices.DeleteFunc(got.JSON200.Subscriptions, func(s k0rdent.WebhookSubscription) bool { return s.Id == nil })
	return rows, diags
}

// findWebhookSubscription is the one subscription id names. found is false
// when the list answered and it isn't there. The list is the one this run
// shares across lookups (webhook_list_memo.go), so looking up every
// subscription in a configuration reads it at most once every
// webhookListFresh rather than once each.
func findWebhookSubscription(ctx context.Context, meta *Meta, id string) (k0rdent.WebhookSubscription, bool, diag.Diagnostics) {
	rows, diags := meta.webhookList.list(ctx, meta)
	if diags.HasError() {
		return k0rdent.WebhookSubscription{}, false, diags
	}
	for _, row := range rows {
		if row.Id.String() == id {
			return row, true, diags
		}
	}
	return k0rdent.WebhookSubscription{}, false, diags
}

// webhookSubscriptionView is everything both read-only forms report for one
// subscription.
type webhookSubscriptionView struct {
	ID                      types.String `tfsdk:"id"`
	URL                     types.String `tfsdk:"url"`
	EventTypes              types.Set    `tfsdk:"event_types"`
	SecretVersion           types.Int64  `tfsdk:"secret_version"`
	IsActive                types.Bool   `tfsdk:"is_active"`
	CreatedAt               types.String `tfsdk:"created_at"`
	RotatedAt               types.String `tfsdk:"rotated_at"`
	PreviousSecretExpiresAt types.String `tfsdk:"previous_secret_expires_at"`
	LastSuccessAt           types.String `tfsdk:"last_success_at"`
	ConsecutiveFailures     types.Int64  `tfsdk:"consecutive_failures"`
	FailingSince            types.String `tfsdk:"failing_since"`
}

func webhookSubscriptionViewFrom(ctx context.Context, row k0rdent.WebhookSubscription) (webhookSubscriptionView, diag.Diagnostics) {
	events, diags := stringSetOrNull(ctx, row.EventTypes)
	if row.EventTypes != nil && len(*row.EventTypes) == 0 {
		// Every event, as an unset filter on the resource reads.
		events = types.SetNull(types.StringType)
	}
	return webhookSubscriptionView{
		ID:                      types.StringValue(row.Id.String()),
		URL:                     stringOrNull(row.Url),
		EventTypes:              events,
		SecretVersion:           int64OrNull(row.SecretVersion),
		IsActive:                boolOrNull(row.IsActive),
		CreatedAt:               timeOrNull(row.CreatedAt),
		RotatedAt:               timeOrNull(row.RotatedAt),
		PreviousSecretExpiresAt: timeOrNull(row.PreviousSecretExpiresAt),
		LastSuccessAt:           timeOrNull(row.LastSuccessAt),
		ConsecutiveFailures:     int64OrNull(row.ConsecutiveFailures),
		FailingSince:            timeOrNull(row.FailingSince),
	}, diags
}

// webhookSubscriptionViewAttributes are the reported attributes, computed in
// both forms. The singular form makes id required on top of them.
func webhookSubscriptionViewAttributes() map[string]schema.Attribute {
	return map[string]schema.Attribute{
		"id": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "The subscription's id, a UUID.",
		},
		"url": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "The endpoint the platform POSTs events to.",
		},
		"event_types": schema.SetAttribute{
			Computed:            true,
			ElementType:         types.StringType,
			MarkdownDescription: "The event types delivered. Null means every event.",
		},
		"secret_version": schema.Int64Attribute{
			Computed: true,
			MarkdownDescription: "The platform's count of signing secrets, which every delivery carries as " +
				"`X-K0r-Signature-Version`.",
		},
		"is_active": schema.BoolAttribute{
			Computed: true,
			MarkdownDescription: "Whether the platform delivers to this endpoint. It reads `false` once the " +
				"platform has turned the subscription off after sustained delivery failure, which can't be " +
				"undone: only deleting it and creating it again gets deliveries going.",
		},
		"created_at": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "When the subscription was created, as RFC 3339.",
		},
		"rotated_at": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "When the secret was last rotated. Null if it never has been.",
		},
		"previous_secret_expires_at": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "When the previous secret stops being accepted. Only set while a " +
				"rotation's overlap is open.",
		},
		"last_success_at": schema.StringAttribute{
			Computed:            true,
			MarkdownDescription: "When the endpoint last accepted a delivery with a 2xx. Null if it never has.",
		},
		"consecutive_failures": schema.Int64Attribute{
			Computed: true,
			MarkdownDescription: "Deliveries dead-lettered since `last_success_at`, not counting one still " +
				"being retried. Zero for a healthy endpoint.",
		},
		"failing_since": schema.StringAttribute{
			Computed: true,
			MarkdownDescription: "When the current run of failures began. Null when " +
				"`consecutive_failures` is 0. Read the two together to tell a blip from an endpoint " +
				"that's been down for days.",
		},
	}
}

// ---------------------------------------------------------------------------
// One subscription
// ---------------------------------------------------------------------------

func (d *webhookSubscriptionDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(webhookSubscriptionDataSourceSuffix)
}

func (d *webhookSubscriptionDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	attrs := webhookSubscriptionViewAttributes()
	attrs["id"] = schema.StringAttribute{
		Required:            true,
		MarkdownDescription: "The subscription's id, which must be a lower-case UUID.",
		Validators:          []validator.String{subscriptionID{}},
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "One webhook subscription, read rather than managed, with the delivery " +
			"health the resource leaves out.\n\n" +
			"Use it from a configuration that doesn't own the subscription, to check the endpoint " +
			"is taking deliveries: `consecutive_failures` and `failing_since` say whether it's " +
			"failing and for how long. The signing secret is never readable here or anywhere else " +
			"after the call that set it.\n\n" +
			"The platform has no read of one subscription, so this reads the list and picks the id " +
			"out of it.",
		Attributes: attrs,
	}
}

func (d *webhookSubscriptionDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *webhookSubscriptionDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config webhookSubscriptionView
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() || !providerConfigured(d.meta, &resp.Diagnostics) {
		return
	}
	id := config.ID.ValueString()
	row, found, diags := findWebhookSubscription(ctx, d.meta, id)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		// Local: the list answered without it.
		resp.Diagnostics.AddAttributeError(path.Root("id"), "No such webhook subscription",
			fmt.Sprintf("The subscriptions this credential's organization has don't include %q.", id))
		return
	}
	view, diags := webhookSubscriptionViewFrom(ctx, row)
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.State.Set(ctx, &view)...)
}

// ---------------------------------------------------------------------------
// Every subscription
// ---------------------------------------------------------------------------

type webhookSubscriptionsModel struct {
	URL           types.String `tfsdk:"url"`
	IsActive      types.Bool   `tfsdk:"is_active"`
	Subscriptions types.List   `tfsdk:"subscriptions"`
}

func (d *webhookSubscriptionsDataSource) Metadata(_ context.Context, _ datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = ResourceName(webhookSubscriptionsDataSourceSuffix)
}

func (d *webhookSubscriptionsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every webhook subscription the credential's organization has, with each " +
			"endpoint's delivery health.\n\n" +
			"Set `url` to find the subscriptions to one endpoint. That's the way back from a create " +
			"whose answer was lost: the platform picks a subscription's id, so the list is the only " +
			"place to find it, and an import adopts it.",
		Attributes: map[string]schema.Attribute{
			"url": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Only the subscriptions to this endpoint, compared exactly as the " +
					"platform reports it.",
			},
			// A subscription's state is this one flag, so this is its state
			// filter, under the name the subscription itself uses.
			"is_active": schema.BoolAttribute{
				Optional: true,
				MarkdownDescription: "Only the subscriptions whose `is_active` is this value: `false` " +
					"finds the endpoints the platform has stopped delivering to. " + filteredInProvider,
			},
			"subscriptions": schema.ListNestedAttribute{
				Computed:            true,
				MarkdownDescription: "The subscriptions, in id order.",
				NestedObject:        schema.NestedAttributeObject{Attributes: webhookSubscriptionViewAttributes()},
			},
		},
	}
}

func (d *webhookSubscriptionsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (d *webhookSubscriptionsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var data webhookSubscriptionsModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() || !providerConfigured(d.meta, &resp.Diagnostics) {
		return
	}
	rows, diags := listWebhookSubscriptions(ctx, d.meta)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	// The contract gives the list no order, and a list that came back in a
	// new one would show as a change on every refresh.
	slices.SortFunc(rows, func(a, b k0rdent.WebhookSubscription) int { return strings.Compare(a.Id.String(), b.Id.String()) })

	views := []webhookSubscriptionView{}
	for _, row := range rows {
		if !data.URL.IsNull() && deref(row.Url) != data.URL.ValueString() {
			continue
		}
		if !data.IsActive.IsNull() && (row.IsActive == nil || *row.IsActive != data.IsActive.ValueBool()) {
			continue
		}
		view, d := webhookSubscriptionViewFrom(ctx, row)
		resp.Diagnostics.Append(d...)
		views = append(views, view)
	}
	if resp.Diagnostics.HasError() {
		return
	}
	list, diags := types.ListValueFrom(ctx, types.ObjectType{AttrTypes: webhookSubscriptionViewTypes()}, views)
	resp.Diagnostics.Append(diags...)
	data.Subscriptions = list
	resp.Diagnostics.Append(resp.State.Set(ctx, &data)...)
}

// webhookSubscriptionViewTypes is the object type of one listed subscription.
func webhookSubscriptionViewTypes() map[string]attr.Type {
	out := map[string]attr.Type{}
	for name, a := range webhookSubscriptionViewAttributes() {
		out[name] = a.GetType()
	}
	return out
}

// subscriptionID is a subscription id as the platform writes one: a
// lower-case UUID. It becomes a request path segment, and any other spelling
// of the same UUID would read back as a different id.
type subscriptionID struct{}

func (subscriptionID) Description(context.Context) string { return "must be a lower-case UUID" }

func (v subscriptionID) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (subscriptionID) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if !canonicalUUID.MatchString(req.ConfigValue.ValueString()) {
		resp.Diagnostics.AddAttributeError(req.Path, "Not a subscription id",
			fmt.Sprintf("%q isn't a lower-case UUID, which is how the platform names a subscription.",
				req.ConfigValue.ValueString()))
	}
}
