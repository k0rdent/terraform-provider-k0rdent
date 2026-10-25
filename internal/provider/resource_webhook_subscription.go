package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework-validators/resourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/identityschema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

var (
	_ resource.Resource                 = (*webhookSubscriptionResource)(nil)
	_ resource.ResourceWithConfigure    = (*webhookSubscriptionResource)(nil)
	_ resource.ResourceWithImportState  = (*webhookSubscriptionResource)(nil)
	_ resource.ResourceWithIdentity     = (*webhookSubscriptionResource)(nil)
	_ resource.ResourceWithUpgradeState = (*webhookSubscriptionResource)(nil)

	_ resource.ResourceWithConfigValidators = (*webhookSubscriptionResource)(nil)
	_ resource.ResourceWithModifyPlan       = (*webhookSubscriptionResource)(nil)
)

const webhookSubscriptionResourceSuffix = "webhook_subscription"

func newWebhookSubscriptionResource() resource.Resource { return &webhookSubscriptionResource{} }

// webhookSubscriptionResource is an endpoint the platform POSTs notification
// events to, signed with a secret the endpoint verifies them by.
//
// # The secret is write-only, and that's the whole design
//
// The platform answers a signing secret once, at create or rotate, and never
// again. A provider would normally keep it in state as a sensitive computed
// attribute. This one can't: no secret may reach a state file or a saved plan,
// and credential_gate.go refuses any attribute named
// like one, or marked Sensitive, on a surface that persists. So the secret is
// the configuration's to hold, in a write-only attribute that Terraform hands
// to the provider and never writes down, and the platform's generated one is
// never kept.
//
// Write-only attributes need Terraform 1.11 and the provider's floor is 1.10,
// for the ephemeral kubeconfig. On 1.10 the resource still works without a
// secret, and Terraform refuses `secret_wo` with its own error before
// anything is sent. Raising the floor for one optional attribute would cost
// every other resource a version, so it isn't raised.
//
// # A new secret is a rotation, never a replacement
//
// Terraform can't see a write-only value change, so `secret_wo_version` says
// when it has. A change to the version calls rotateWebhookSubscriptionSecret
// with the configured secret. The contract is plain about why that matters:
// rotating keeps the subscription's id and loses nothing queued for it, and
// deleting and recreating loses everything queued in between. By default the
// previous secret is still accepted for 24 hours, so receivers can move at
// their own pace. `rotation_overlap = false` drops it at once, for a leak.
//
// # Everything else replaces
//
// The contract has create, list, delete and rotate, and no update and no read
// of one subscription. So `url` and `event_types` replace, and a read is the
// list filtered to this id. The list is one page, with no paging parameters.
type webhookSubscriptionResource struct{ meta *Meta }

type webhookSubscriptionModel struct {
	ID              types.String `tfsdk:"id"`
	URL             types.String `tfsdk:"url"`
	EventTypes      types.Set    `tfsdk:"event_types"`
	SecretWO        types.String `tfsdk:"secret_wo"`
	SecretWOVersion types.Int64  `tfsdk:"secret_wo_version"`
	RotationOverlap types.Bool   `tfsdk:"rotation_overlap"`

	SecretVersion           types.Int64  `tfsdk:"secret_version"`
	IsActive                types.Bool   `tfsdk:"is_active"`
	CreatedAt               types.String `tfsdk:"created_at"`
	RotatedAt               types.String `tfsdk:"rotated_at"`
	PreviousSecretExpiresAt types.String `tfsdk:"previous_secret_expires_at"`
}

func (r *webhookSubscriptionResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = ResourceName(webhookSubscriptionResourceSuffix)
}

// UpgradeState migrates a state written at an earlier schema version. See
// state_history.go.
func (r *webhookSubscriptionResource) UpgradeState(context.Context) map[int64]resource.StateUpgrader {
	return stateHistories[webhookSubscriptionResourceSuffix].upgraders()
}

func (r *webhookSubscriptionResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Version: stateHistories[webhookSubscriptionResourceSuffix].version(),
		MarkdownDescription: "An endpoint the platform POSTs notification events to, each one signed " +
			"with HMAC-SHA256 under a secret the endpoint verifies it with.\n\n" +
			"It takes no `region` and no `project`. A subscription lives on the global tier and " +
			"belongs to the organization the provider's credential is in, so it receives that " +
			"organization's events.\n\n" +
			"**The signing secret never reaches state.** The platform shows a secret once and never " +
			"again, and this provider keeps no secret in state or in a plan, so the secret is the " +
			"configuration's to hold: set `secret_wo`, ideally from an ephemeral source such as a " +
			"secrets manager's ephemeral resource, and bump `secret_wo_version` whenever it " +
			"changes. Creating or replacing a subscription needs `secret_wo`, and so Terraform 1.11 or " +
			"later: without it the platform would generate a secret nobody can read back, every " +
			"receiver that verifies would refuse every delivery, and the platform dead-letters a " +
			"refused delivery at once. A subscription already made can be imported and managed " +
			"without it.\n\n" +
			"**A new secret rotates the same subscription.** Bumping `secret_wo_version` calls the " +
			"platform's rotation, which keeps the id and everything queued for the endpoint. By " +
			"default the previous secret is still accepted for 24 hours, and every delivery carries " +
			"a signature under each, so receivers can move to the new one at their own pace.\n\n" +
			"Delivery health (last success, consecutive failures) is on the `" +
			ResourceName(webhookSubscriptionDataSourceSuffix) + "` data source rather than here, " +
			"because it moves with every delivery and isn't anything a configuration sets.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The subscription's id, a UUID the platform assigns. It's what an import names.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"url": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "Absolute `http` or `https` URL the platform POSTs events to. Plain " +
					"`http` is accepted for a receiver on an internal network. It must be reachable " +
					"from the platform's egress addresses, which your platform operator publishes. " +
					"There's no update for it, so a change replaces the subscription, under a new id.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
				Validators:    []validator.String{webhookURL{}},
			},
			"event_types": schema.SetAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "The event types to deliver, such as `io.k0rdent.workflow.cluster.create.v1`: " +
					"dotted lower case, at least three segments, as the event catalog names them. A name " +
					"outside that form is warned about at plan time, not refused. Leave it out to " +
					"receive every event. There's no update for it, so a change replaces the " +
					"subscription." +
					knownGap("The event catalog that lists them is provider personnel only, so a tenant "+
						"can't read the names and a configuration writes them as literals."),
				PlanModifiers: []planmodifier.Set{setplanmodifier.RequiresReplace()},
				Validators:    []validator.Set{everyEventIsUnset{}, eventTypesInCatalogForm{}},
			},
			"secret_wo": schema.StringAttribute{
				Optional:  true,
				WriteOnly: true,
				MarkdownDescription: "The signing secret, at least " +
					fmt.Sprint(k0rdent.CreateWebhookSubscriptionJSONBodySecretMinLength) + " characters. Write-only: Terraform " +
					"passes it to the provider and never stores it, so it's never in state or a plan. " +
					"Terraform can't see it change, so set `secret_wo_version` beside it and change " +
					"that whenever this changes. Required to create or replace a subscription. Needs " +
					"Terraform 1.11 or later.",
				Validators: []validator.String{stringvalidator.LengthAtLeast(k0rdent.CreateWebhookSubscriptionJSONBodySecretMinLength)},
			},
			"secret_wo_version": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Any number that changes when `secret_wo` does. A change rotates " +
					"the secret in place: the id, the endpoint and everything queued for it stay. " +
					"It can be a literal, a variable or a local, but has to be known at plan time, so " +
					"not a value only the apply works out, such as another resource's id. " +
					"Removing it, with `secret_wo`, sends nothing and leaves the platform's secret as " +
					"it is. After an import the provider can't know which secret the platform holds, " +
					"so the first apply that sets this rotates to `secret_wo`. A rotation made outside " +
					"Terraform clears it on the next refresh, so the plan after it rotates back.\n\n" +
					"Rotate once per overlap. Only the most recent previous secret is kept, so a second " +
					"rotation while `previous_secret_expires_at` is still in the future drops the secret " +
					"the first one parked, and receivers still on it start refusing deliveries. The plan " +
					"warns when that's about to happen.",
				Validators: []validator.Int64{int64validator.AtLeast(1)},
			},
			"rotation_overlap": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(true),
				MarkdownDescription: "Whether a rotation keeps the previous secret valid for the 24 " +
					"hours that follow. Defaults to `true`. Set `false` to cut over at once, for a " +
					"leaked secret: any receiver not yet on the new secret starts refusing deliveries, " +
					"and the platform reads that refusal as permanent. It applies to the next rotation " +
					"and changing it on its own sends nothing.",
			},
			// Only a rotation moves this and the two after it, and only a new
			// `secret_wo_version` rotates. Any other update sends nothing.
			"secret_version": schema.Int64Attribute{
				Computed: true,
				MarkdownDescription: "The platform's count of signing secrets, 1 for the first and one " +
					"more for each rotation. Every delivery carries it as `X-K0r-Signature-Version`, so " +
					"receivers can tell whether they've caught up. It isn't `secret_wo_version`.",
				PlanModifiers: []planmodifier.Int64{keepUnlessChanged("secret_wo_version")},
			},
			"is_active": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether the platform is delivering to this endpoint.",
				PlanModifiers:       []planmodifier.Bool{keepUnlessChanged("secret_wo_version")},
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the subscription was created, as RFC 3339.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"rotated_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "When the secret was last rotated, as RFC 3339. Null if it never has been.",
				PlanModifiers:       []planmodifier.String{keepUnlessChanged("secret_wo_version")},
			},
			// The overlap also closes on its own, so only a null is certain to
			// read the same after an update that doesn't rotate.
			"previous_secret_expires_at": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "When the previous secret stops being accepted, as RFC 3339. Only " +
					"set while a rotation's overlap is open, so a refresh after it closes reads null.",
				PlanModifiers: []planmodifier.String{keepUnlessChanged("secret_wo_version").whileNull()},
			},
		},
	}
	describeReplacement(&resp.Schema, "subscription")
}

func (r *webhookSubscriptionResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

// ConfigValidators keeps the secret and its version together. A secret with
// no version could never be told apart from the last one, and a version with
// no secret would rotate to one the platform makes and nobody keeps.
func (r *webhookSubscriptionResource) ConfigValidators(context.Context) []resource.ConfigValidator {
	return []resource.ConfigValidator{
		resourcevalidator.RequiredTogether(path.MatchRoot("secret_wo"), path.MatchRoot("secret_wo_version")),
	}
}

// refuseUnknownVersion refuses a secret version that's still unknown at plan
// time. The version is the only thing a plan can see of a secret change, so a
// plan with it unknown can't say whether the apply rotates. It runs in
// ModifyPlan and not ValidateConfig, because Terraform validates with every
// variable unknown, and a version from a variable is fine.
func refuseUnknownVersion(version types.Int64, diags *diag.Diagnostics) {
	if version.IsUnknown() {
		// Local: configuration check, no request made.
		diags.AddAttributeError(path.Root("secret_wo_version"),
			"secret_wo_version has to be known at plan time",
			"It's what tells a plan whether the signing secret rotates, so it can't come from a value "+
				"only the apply works out, such as another resource's id. Use a literal, a variable, "+
				"or a local built from them.")
	}
}

func (r *webhookSubscriptionResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan webhookSubscriptionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	// A write-only value is null in the plan. Only the configuration has it.
	var secret types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secret_wo"), &secret)...)
	if resp.Diagnostics.HasError() {
		return
	}

	body := k0rdent.CreateWebhookSubscriptionJSONRequestBody{
		Url:    plan.URL.ValueString(),
		Secret: optionalString(secret),
	}
	if !plan.EventTypes.IsNull() {
		var events []string
		resp.Diagnostics.Append(plan.EventTypes.ElementsAs(ctx, &events, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
		body.EventTypes = &events
	}

	// What was there before, so an answer that can't be read can still be
	// matched to the subscription it made. A failed list costs only that.
	before, _ := listWebhookSubscriptions(ctx, r.meta)

	created, err := r.meta.Client.CreateWebhookSubscriptionWithResponse(ctx, body)
	r.meta.webhookList.forget()
	// A body that doesn't decode comes back with its error and is found
	// below. Only a create with no answer at all is a transport failure.
	if created == nil {
		addAPIErrorHiding(&resp.Diagnostics, secret.ValueString(), "create the webhook subscription", nil, nil, err)
		addLostCreateAdvice(&resp.Diagnostics)
		return
	}
	// The answer's signing_secret goes no further than this function. It's
	// the configured secret, and no secret may be kept.
	var id string
	switch status := created.StatusCode(); {
	case created.JSON201 != nil:
		id = created.JSON201.Id.String()
		plan.CreatedAt = timeOrNull(&created.JSON201.CreatedAt)
	case status >= 200 && status < 300:
		// Accepted, with an answer the client couldn't decode. The
		// subscription exists, so it's found and recorded rather than
		// reported as a failure the next apply would make again.
		var found bool
		if id, found = createdSubscriptionID(ctx, r.meta, created.Body, before, plan.URL.ValueString()); !found {
			addAPIErrorHiding(&resp.Diagnostics, secret.ValueString(), "create the webhook subscription", created.HTTPResponse, created.Body, nil)
			addLostCreateAdvice(&resp.Diagnostics)
			return
		}
		plan.CreatedAt = types.StringNull()
	default:
		addAPIErrorHiding(&resp.Diagnostics, secret.ValueString(), "create the webhook subscription", created.HTTPResponse, created.Body, nil)
		if status >= http.StatusInternalServerError {
			addLostCreateAdvice(&resp.Diagnostics)
		}
		return
	}
	// Recorded before the read, so a read that fails leaves the subscription
	// in state to be read again rather than made twice.
	plan.ID = types.StringValue(id)
	plan.SecretWO = types.StringNull()
	// Unknown until the read, and state can't hold an unknown. The read
	// fills them, and a refresh does if the read fails.
	plan.SecretVersion, plan.IsActive = types.Int64Null(), types.BoolNull()
	plan.RotatedAt, plan.PreviousSecretExpiresAt = types.StringNull(), types.StringNull()
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)

	out, found, diags := r.read(ctx, id, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		// Local: the provider's conclusion from two calls.
		resp.Diagnostics.AddError("The new webhook subscription isn't listed",
			fmt.Sprintf("createWebhookSubscription answered %s, and the list of subscriptions read straight "+
				"after doesn't have it. It's kept in state so the next refresh looks again.", id))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}

// addLostCreateAdvice follows an error on a create whose answer may have been
// lost. The platform picks a subscription's id, so a create can't be read back
// by a name the provider chose, as the cluster's can, and a second apply makes
// a second subscription. The data source finds one, and an import adopts it.
func addLostCreateAdvice(diags *diag.Diagnostics) {
	diags.AddWarning("The subscription may have been created anyway",
		fmt.Sprintf("When a create fails this way the platform may still have made the subscription, and "+
			"there's no id to look it up by. Read the `%s` data source with this `url`, and import the "+
			"one you find rather than applying again, or the endpoint receives every event twice.",
			ResourceName(webhookSubscriptionsDataSourceSuffix)))
}

func (r *webhookSubscriptionResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State, &req.State)
	var prior webhookSubscriptionModel
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() {
		return
	}
	out, found, diags := r.read(ctx, prior.ID.ValueString(), prior)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		// Deleted outside the provider. Absent, not an error: the next plan
		// proposes to create it.
		resp.State.RemoveResource(ctx)
		return
	}
	if rotatedElsewhere(prior.SecretVersion, out.SecretVersion) && !out.SecretWOVersion.IsNull() {
		// Every rotation this provider makes lands in state with its new
		// secret_version, so a higher one is somebody else's, to a secret
		// the configuration doesn't hold. Forgetting the version makes the
		// next plan rotate back to the configured one.
		out.SecretWOVersion = types.Int64Null()
		resp.Diagnostics.AddWarning("The signing secret was rotated outside Terraform",
			fmt.Sprintf("Subscription %s is at secret_version %d, and state last held %d. The next apply "+
				"rotates it back to `secret_wo`.", out.ID.ValueString(), out.SecretVersion.ValueInt64(),
				prior.SecretVersion.ValueInt64()))
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}

// Update rotates the secret when `secret_wo_version` changed and names one.
// Anything else that reaches here, `rotation_overlap` or a version removed,
// only needs writing to state, because `url` and `event_types` replace.
func (r *webhookSubscriptionResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	defer syncIdentity(ctx, r.meta, resp.Identity, &resp.Diagnostics, &resp.State)
	var plan, state webhookSubscriptionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	var secret types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secret_wo"), &secret)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	plan.ID, plan.CreatedAt, plan.SecretWO = state.ID, state.CreatedAt, types.StringNull()
	// What the platform reported last, until the read below says otherwise. A
	// rotation overwrites the parts it moves.
	plan.SecretVersion, plan.IsActive = state.SecretVersion, state.IsActive
	plan.RotatedAt, plan.PreviousSecretExpiresAt = state.RotatedAt, state.PreviousSecretExpiresAt

	if rotates(state.SecretWOVersion, plan.SecretWOVersion) {
		subscription, err := uuid.Parse(id)
		if err != nil {
			// Local: state holds an id the API can't be asked about.
			resp.Diagnostics.AddError("The subscription's id isn't a UUID",
				fmt.Sprintf("State holds %q, and the platform names a subscription by UUID.", id))
			return
		}
		overlap := plan.RotationOverlap.ValueBool()
		rotated, err := r.meta.Client.RotateWebhookSubscriptionSecretWithResponse(ctx, subscription,
			k0rdent.RotateWebhookSubscriptionSecretJSONRequestBody{Secret: optionalString(secret), Overlap: &overlap})
		r.meta.webhookList.forget()
		// A body that doesn't decode comes back with its error, and a 2xx is
		// still a rotation. A nil answer is a failure: the request didn't
		// complete, or its body couldn't be read.
		if rotated == nil {
			addAPIErrorHiding(&resp.Diagnostics, secret.ValueString(), "rotate the webhook subscription's secret", nil, nil, err)
			return
		}
		status := rotated.StatusCode()
		if rotated.JSON200 == nil && (status < 200 || status > 299) {
			addAPIErrorHiding(&resp.Diagnostics, secret.ValueString(), "rotate the webhook subscription's secret", rotated.HTTPResponse, rotated.Body, nil)
			return
		}
		// Recorded before the read. If the read fails, state still holds the
		// version this rotation applied, so the next apply doesn't rotate
		// again: a second rotation would move the receivers' current secret
		// out of the overlap. A 2xx the client couldn't decode is a rotation
		// all the same, and the read fills in what it moved. Until then
		// secret_version is null: the one state had would make the next
		// refresh take this rotation for one made outside Terraform, and
		// rotate again.
		if rotated.JSON200 != nil {
			plan.SecretVersion = types.Int64Value(int64(rotated.JSON200.SecretVersion))
			plan.RotatedAt = timeOrNull(&rotated.JSON200.RotatedAt)
			plan.PreviousSecretExpiresAt = timeOrNull(rotated.JSON200.PreviousSecretExpiresAt)
		} else {
			plan.SecretVersion, plan.RotatedAt, plan.PreviousSecretExpiresAt =
				types.Int64Null(), types.StringNull(), types.StringNull()
			// Local: a 2xx whose body the provider couldn't decode.
			resp.Diagnostics.AddWarning("The rotation's answer couldn't be read",
				fmt.Sprintf("The platform accepted the rotation with %d, and its body didn't decode, so the "+
					"new secret_version is read back from the subscription list instead.", status))
		}
		resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
	}

	out, found, diags := r.read(ctx, id, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !found {
		// Local: the provider's conclusion from the list.
		resp.Diagnostics.AddError("The webhook subscription disappeared during the update",
			fmt.Sprintf("The list of subscriptions read after the change doesn't have %q.", id))
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &out)...)
}

// rotatedElsewhere is whether the platform's secret_version moved past the
// one state last held.
func rotatedElsewhere(prior, now types.Int64) bool {
	return !prior.IsNull() && !prior.IsUnknown() && !now.IsNull() && now.ValueInt64() > prior.ValueInt64()
}

// ModifyPlan refuses a secret version only the apply knows and a new
// subscription with no secret, and warns before a rotation that would drop a
// secret receivers may still be on.
func (r *webhookSubscriptionResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan, state webhookSubscriptionModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	var secret types.String
	resp.Diagnostics.Append(req.Config.GetAttribute(ctx, path.Root("secret_wo"), &secret)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if refuseUnknownVersion(plan.SecretWOVersion, &resp.Diagnostics); resp.Diagnostics.HasError() {
		return
	}
	creating := req.State.Raw.IsNull()
	if !creating {
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
		creating = !plan.URL.Equal(state.URL) || !plan.EventTypes.Equal(state.EventTypes)
	}
	if creating && secret.IsNull() {
		resp.Diagnostics.AddAttributeError(path.Root("secret_wo"), "A new subscription needs a signing secret",
			"Set `secret_wo` and `secret_wo_version`. Without them the platform generates a secret and "+
				"answers it once, and this provider keeps no secret in state, so nobody could read it: a "+
				"receiver that verifies signatures would refuse every delivery, and the platform "+
				"dead-letters a refused delivery at once. `secret_wo` is write-only, which needs "+
				"Terraform 1.11 or later. A subscription made some other way can be imported without one.")
		return
	}
	if creating || !rotates(state.SecretWOVersion, plan.SecretWOVersion) {
		return
	}
	if until, ok := parkedSecretDropped(state.PreviousSecretExpiresAt, plan.RotationOverlap, time.Now()); ok {
		resp.Diagnostics.AddAttributeWarning(path.Root("secret_wo_version"),
			"This rotation drops the secret the last one parked",
			fmt.Sprintf("The last rotation's overlap is open until %s, and the platform keeps only the most "+
				"recent previous secret. Rotating now replaces it, so receivers still on the secret from "+
				"before the last rotation start refusing deliveries at once. Wait until then if any are.", until))
	}
}

// parkedSecretDropped reports whether a rotation now would drop a previous
// secret the platform is still accepting, and until when it would have.
// Either overlap setting drops it: a cutover discards it, and an overlapping
// rotation parks the current secret in its place.
func parkedSecretDropped(previousUntil types.String, _ types.Bool, now time.Time) (string, bool) {
	if previousUntil.IsNull() || previousUntil.IsUnknown() {
		return "", false
	}
	until, err := time.Parse(time.RFC3339, previousUntil.ValueString())
	if err != nil || !until.After(now) {
		return "", false
	}
	return previousUntil.ValueString(), true
}

// rotates is whether a plan's secret version asks for a rotation: it names
// one, and it isn't the one already applied. A version taken away asks for
// nothing, since there's no secret beside it to rotate to.
func rotates(prior, planned types.Int64) bool {
	return !planned.IsNull() && !planned.Equal(prior)
}

func (r *webhookSubscriptionResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state webhookSubscriptionModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	subscription, err := uuid.Parse(state.ID.ValueString())
	if err != nil {
		// Local: nothing was sent.
		resp.Diagnostics.AddError("The subscription's id isn't a UUID",
			fmt.Sprintf("State holds %q, and the platform names a subscription by UUID.", state.ID.ValueString()))
		return
	}
	deleted, err := r.meta.Client.DeleteWebhookSubscriptionWithResponse(ctx, subscription)
	r.meta.webhookList.forget()
	if err != nil {
		addAPIError(&resp.Diagnostics, "delete the webhook subscription", nil, nil, err)
		return
	}
	// Synchronous: the contract answers 204 with nothing left to wait for. A
	// 404 is a subscription somebody else already deleted, which is the end
	// this was asking for.
	if deleted.StatusCode() == http.StatusNoContent || isNotFound(deleted.HTTPResponse, deleted.Body) {
		return
	}
	addAPIError(&resp.Diagnostics, "delete the webhook subscription", deleted.HTTPResponse, deleted.Body, nil)
}

// ImportState takes the subscription's id, from the string form or the
// identity. It's checked here, because an import id reaches state without
// passing the schema, and it becomes a request path segment.
func (r *webhookSubscriptionResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	raw := req.ID
	if raw == "" && req.Identity != nil && !req.Identity.Raw.IsNull() {
		var id types.String
		resp.Diagnostics.Append(req.Identity.GetAttribute(ctx, path.Root("id"), &id)...)
		raw = id.ValueString()
	}
	if _, err := uuid.Parse(raw); err != nil || !canonicalUUID.MatchString(raw) {
		// Local: the import id is checked before any request is made.
		resp.Diagnostics.AddError("The import id is not in the expected form",
			fmt.Sprintf("Importing a %s needs the subscription's id, a lower-case UUID, and %q isn't one. "+
				"The `%s` data source lists the subscriptions and their ids.",
				ResourceName(webhookSubscriptionResourceSuffix), raw, ResourceName(webhookSubscriptionsDataSourceSuffix)))
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), raw)...)
}

// IdentitySchema is what an import block's `identity` takes: the id alone,
// since a subscription has no region, project or organization in its path.
func (r *webhookSubscriptionResource) IdentitySchema(_ context.Context, _ resource.IdentitySchemaRequest, resp *resource.IdentitySchemaResponse) {
	resp.IdentitySchema = identityschema.Schema{Attributes: map[string]identityschema.Attribute{
		"id": identityschema.StringAttribute{
			RequiredForImport: true,
			Description:       "The subscription's id, a UUID.",
		},
	}}
}

// read finds one subscription in the list. found is false when the list
// answered and it isn't there.
func (r *webhookSubscriptionResource) read(ctx context.Context, id string, prior webhookSubscriptionModel) (webhookSubscriptionModel, bool, diag.Diagnostics) {
	row, found, diags := findWebhookSubscription(ctx, r.meta, id)
	if diags.HasError() || !found {
		return webhookSubscriptionModel{}, false, diags
	}
	out, d := webhookSubscriptionStateFrom(ctx, row, prior)
	diags.Append(d...)
	return out, true, diags
}

// webhookSubscriptionStateFrom maps a listed subscription onto the model. The
// configuration-only attributes come from prior: the platform knows nothing of
// the version a configuration numbers its secret with, or of the overlap it
// wants for the next rotation.
//
// An empty filter is "every event", which is also what an unset one means, so
// a configuration that left event_types out reads null rather than an empty
// set it would plan to remove.
func webhookSubscriptionStateFrom(ctx context.Context, row k0rdent.WebhookSubscription, prior webhookSubscriptionModel) (webhookSubscriptionModel, diag.Diagnostics) {
	var diags diag.Diagnostics
	out := webhookSubscriptionModel{
		ID:              types.StringValue(row.Id.String()),
		URL:             stringOrNull(row.Url),
		EventTypes:      types.SetNull(types.StringType),
		SecretWO:        types.StringNull(),
		SecretWOVersion: prior.SecretWOVersion,
		RotationOverlap: prior.RotationOverlap,

		SecretVersion:           int64OrNull(row.SecretVersion),
		IsActive:                boolOrNull(row.IsActive),
		CreatedAt:               timeOrNull(row.CreatedAt),
		RotatedAt:               timeOrNull(row.RotatedAt),
		PreviousSecretExpiresAt: timeOrNull(row.PreviousSecretExpiresAt),
	}
	if out.RotationOverlap.IsNull() {
		// An import, which has no configuration to take it from yet. The
		// schema's default, so the plan after it is empty.
		out.RotationOverlap = types.BoolValue(true)
	}
	if out.CreatedAt.IsNull() {
		out.CreatedAt = prior.CreatedAt
	}
	if row.EventTypes != nil && len(*row.EventTypes) > 0 {
		out.EventTypes, diags = stringSetOrNull(ctx, row.EventTypes)
	}
	return out, diags
}

// eventTypeMaxLength is the catalog's limit on an event type, which the
// public contract doesn't carry.
const eventTypeMaxLength = 255

// eventTypePattern is the catalog's event_type pattern. A subscription's
// filter declares none of its own, but it matches a delivered event's `type`,
// and the catalog is what refuses a type outside this form, so a filter
// outside it can never match anything.
var eventTypePattern = regexp.MustCompile(`^[a-z0-9]+(?:\.[a-z0-9_]+){2,}$`)

// canonicalUUID is the form the platform writes ids in. uuid.Parse also takes
// upper case, braces and a urn: prefix, and any of those in state would make a
// plan that reads the platform's spelling back propose a replacement.
var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// webhookURL is the contract's "absolute http(s) endpoint", checked at plan
// time rather than met as a 422.
type webhookURL struct{}

func (webhookURL) Description(context.Context) string {
	return "must be an absolute http or https URL with a host"
}

func (v webhookURL) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (webhookURL) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	raw := req.ConfigValue.ValueString()
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		resp.Diagnostics.AddAttributeError(req.Path, "Not a URL",
			fmt.Sprintf("%q doesn't parse as a URL: %v.", raw, err))
	case !u.IsAbs() || u.Host == "":
		resp.Diagnostics.AddAttributeError(req.Path, "Not an absolute URL",
			fmt.Sprintf("%q has to be an absolute URL, scheme and host included, such as "+
				"https://hooks.example.com/k0rdent. The platform POSTs to it from outside this configuration.", raw))
	case u.Scheme != "http" && u.Scheme != "https":
		resp.Diagnostics.AddAttributeError(req.Path, "Not an http or https URL",
			fmt.Sprintf("%q uses %q, and the platform only delivers over http or https.", raw, u.Scheme))
	case u.User != nil:
		// The URL is stored in state as written, so a password in it would
		// sit there in the clear. The signature is how a receiver knows a
		// delivery is genuine, and it needs nothing in the URL.
		resp.Diagnostics.AddAttributeError(req.Path, "A URL with credentials in it",
			"The URL carries a user name or password, and it's stored in state as written. Take them "+
				"out: a receiver knows a delivery is genuine from its signature, under `secret_wo`.")
	}
}

// everyEventIsUnset refuses an empty filter. The platform reads it as every
// event, the same as no filter, and reads either back as an empty list, so
// only leaving the attribute out plans clean after the apply.
type everyEventIsUnset struct{}

func (everyEventIsUnset) Description(context.Context) string {
	return "must name at least one event type, or be left out to receive every event"
}

func (v everyEventIsUnset) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (everyEventIsUnset) ValidateSet(_ context.Context, req validator.SetRequest, resp *validator.SetResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() || len(req.ConfigValue.Elements()) > 0 {
		return
	}
	resp.Diagnostics.AddAttributeError(req.Path, "An empty event filter",
		"An empty `event_types` subscribes to every event, and so does leaving it out. Leave it out: "+
			"the platform answers both as an empty list, and only the attribute left out reads back "+
			"the way it was written.")
}

// eventTypesInCatalogForm warns about an event type outside the form the
// catalog names them in. The subscription's filter declares no pattern of its
// own, so a name outside it isn't refused: the platform might take it. It
// can't match a delivered event while the catalog holds every event type to
// that form, which is what the warning says.
// ASSUMPTION: the platform accepts a filter outside the form.
type eventTypesInCatalogForm struct{}

func (eventTypesInCatalogForm) Description(context.Context) string {
	return "warns about an event type outside the catalog's form"
}

func (v eventTypesInCatalogForm) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (eventTypesInCatalogForm) ValidateSet(ctx context.Context, req validator.SetRequest, resp *validator.SetResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	var names []types.String
	resp.Diagnostics.Append(req.ConfigValue.ElementsAs(ctx, &names, false)...)
	var odd []string
	for _, n := range names {
		if n.IsUnknown() || n.IsNull() {
			continue
		}
		if v := n.ValueString(); len(v) > eventTypeMaxLength || !eventTypePattern.MatchString(v) {
			odd = append(odd, fmt.Sprintf("%q", v))
		}
	}
	if len(odd) == 0 {
		return
	}
	resp.Diagnostics.AddAttributeWarning(req.Path, "An event type the catalog wouldn't name",
		fmt.Sprintf("%s isn't in the form the event catalog names event types in: dotted lower case, at "+
			"least three segments and at most %d characters, such as io.k0rdent.workflow.cluster.create.v1. The "+
			"platform may accept it, and no delivered event is named that way, so it would match nothing.",
			strings.Join(odd, ", "), eventTypeMaxLength))
}

// createdSubscriptionID finds the subscription a create made when its answer
// couldn't be decoded: from the id in the body when there is one, and
// otherwise from the list, as the one subscription to this endpoint that
// before didn't have. Nothing from the body is ever reported.
func createdSubscriptionID(ctx context.Context, meta *Meta, body []byte, before []k0rdent.WebhookSubscription, endpoint string) (string, bool) {
	var answer struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &answer) == nil && canonicalUUID.MatchString(answer.ID) {
		return answer.ID, true
	}
	if before == nil {
		return "", false
	}
	known := map[string]bool{}
	for _, row := range before {
		known[row.Id.String()] = true
	}
	after, diags := listWebhookSubscriptions(ctx, meta)
	if diags.HasError() {
		return "", false
	}
	var made []string
	for _, row := range after {
		if !known[row.Id.String()] && deref(row.Url) == endpoint {
			made = append(made, row.Id.String())
		}
	}
	if len(made) != 1 {
		return "", false
	}
	return made[0], true
}
