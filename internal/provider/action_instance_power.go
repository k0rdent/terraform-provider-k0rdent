package provider

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/action/timeouts"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/action"
	"github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// The instance power action, Terraform 1.14 and later.
//
// Powering a machine off, on or through a restart is something to do once,
// when asked, and never something a plan converges on, so it's an action and
// not an attribute of the instance group: it runs only when a configuration
// invokes it by name, or binds it to a resource with `action_trigger`.
//
// # Why it doesn't wait
//
// The cancel action waits for the cluster to settle, because the cluster
// reports where it got to. An instance doesn't. The contract says a 202 is
// "submitted, not settled", and that it's the last word the API ever gives
// on an action that succeeds: the hardware controller behind it exposes no
// power state and no task status to poll. The instance's `state` has
// `stopping` and `stopped`, and the contract says nothing reaches them yet.
// So a wait would have nothing to wait for, and every success would end in
// a timeout. The action reports the 202 and says what it does and doesn't
// mean. A failure the platform learns of later is the instance's
// `last_power_action_error`.

const instancePowerActionSuffix = "instance_power"

var (
	_ action.Action               = (*instancePowerAction)(nil)
	_ action.ActionWithConfigure  = (*instancePowerAction)(nil)
	_ action.ActionWithModifyPlan = (*instancePowerAction)(nil)
)

func newInstancePowerAction() action.Action {
	return &instancePowerAction{}
}

type instancePowerAction struct{ meta *Meta }

type instancePowerModel struct {
	Region     types.String   `tfsdk:"region"`
	Project    types.String   `tfsdk:"project"`
	InstanceID types.String   `tfsdk:"instance_id"`
	Action     types.String   `tfsdk:"action"`
	Timeouts   timeouts.Value `tfsdk:"timeouts"`
}

// powerActions are the contract's single-machine power verbs, in its order.
// The generated client types the path's `action` as a plain string, so the
// enum is written out here, and the spec constraint test holds it to the
// contract's.
var powerActions = []string{"powerOn", "powerOff", "powerCycle", "gracefulShutdown", "gracefulRestart", "forceRestart"}

// The details[0].reason values a refusal of the action carries, from the
// contract's 404 and 409.
const (
	powerTargetUnreachable  = "POWER_TARGET_UNREACHABLE"
	powerActionInFlight     = "POWER_ACTION_IN_FLIGHT"
	powerTargetUnidentified = "POWER_TARGET_UNIDENTIFIED"
	powerActionUnsupported  = "POWER_ACTION_UNSUPPORTED"
	reservationDrifted      = "RESERVATION_DRIFTED"
)

func (a *instancePowerAction) Metadata(_ context.Context, _ action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = ResourceName(instancePowerActionSuffix)
}

func (a *instancePowerAction) Schema(ctx context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Powers the machine an instance occupies on, off, or through a restart. Invoke " +
			"it by name with `terraform apply -invoke=action." + ResourceName(instancePowerActionSuffix) +
			".<name>`, or bind it to a resource with `action_trigger`. Actions need Terraform 1.14 or later.\n\n" +
			"The platform accepts the action and dispatches it to the machine, and the action ends there. " +
			"It doesn't wait for the machine to get there, because nothing reports when it has: the " +
			"platform's answer is the instance as it was when the action was submitted, and that's the last " +
			"word it gives on an action that succeeds. A failure the platform learns of afterwards, such as " +
			"the machine's controller refusing it, shows on the `" + ResourceName(instanceDataSourceSuffix) +
			"` data source as `last_power_action_error`.\n\n" +
			"A plan that invokes it reads the instance and warns what it will do. The platform refuses a " +
			"second power action on the same machine for a while after one is accepted, and the action " +
			"then fails, says how long to wait, and sends nothing. It's never sent again on its own: a " +
			"power action twice isn't the same as once. It also fails, having done nothing, when the " +
			"instance doesn't exist or was deleted, when the platform can't confirm the machine is still " +
			"this instance's, and when the machine has no power control at all.\n\n" +
			"Bound to a resource with `action_trigger`, it runs each time one of the trigger's `events` " +
			"happens to that resource, not once. With `after_update` it runs on every apply that changes " +
			"the resource in place, so a power cycle bound to an instance group cycles the machine on each " +
			"such change to the group. With `after_create` it runs on a create and on every replacement. " +
			"It doesn't run on an apply that leaves the resource unchanged. Before Terraform 1.16 no event " +
			"fires on a destroy. From 1.16, `before_destroy` and `after_destroy` run it on a destroy and on " +
			"every replacement. A `condition` on the trigger narrows any of these.",
		Attributes: map[string]schema.Attribute{
			"instance_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The instance whose machine to act on, as the `" +
					ResourceName(instancesDataSourceSuffix) + "` data source reports its `id`.",
				Validators: []validator.String{urlSafeIdentifier{}, stringvalidator.RegexMatches(instanceIDPattern,
					"must be inst- and 32 lower-case hex digits, as the instances data source reports it")},
			},
			"action": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "What to do to the machine: " +
					enumProse(powerActions) + ". The graceful ones ask " +
					"the operating system to shut down or restart, and the others act on the power itself.",
				Validators: []validator.String{stringvalidator.OneOf(powerActions...)},
			},
			// An instance can be in another scope than the provider's, so the
			// action has to reach it there, as the instance data source does.
			"region": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Region the instance is in. Defaults to the provider's `region`.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"project": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Project the instance is in. Defaults to the provider's `project`.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx),
		},
	}
	documentActionSchema(&resp.Schema)
}

func (a *instancePowerAction) Configure(_ context.Context, req action.ConfigureRequest, resp *action.ConfigureResponse) {
	a.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

// ModifyPlan reads the instance and says what the invoke will do to it, so
// it's in front of the approval. It sends nothing, and an instance it can't
// read yet, such as one this run creates, is left to the invoke.
func (a *instancePowerAction) ModifyPlan(ctx context.Context, req action.ModifyPlanRequest, resp *action.ModifyPlanResponse) {
	if a.meta == nil {
		return
	}
	var data instancePowerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for _, v := range []types.String{data.InstanceID, data.Action, data.Region, data.Project} {
		if v.IsUnknown() {
			return
		}
	}
	if data.InstanceID.IsNull() || data.Action.IsNull() {
		return
	}
	var scope diag.Diagnostics
	target, ok := resolveScope(a.meta, data.Region, data.Project, &scope)
	if !ok {
		return // the invoke reports it
	}
	id := data.InstanceID.ValueString()
	got, err := a.meta.Client.GetInstanceWithResponse(ctx, target.region, target.project, id, nil)
	if err != nil || got.JSON200 == nil {
		return
	}
	// Local: decided from the read, nothing sent. The hostname and state
	// are the platform's words, so they're escaped.
	resp.Diagnostics.AddWarning(
		redactDiagnosticText(fmt.Sprintf("Invoking this sends %s to instance %s", data.Action.ValueString(), id)),
		redactDiagnosticText(fmt.Sprintf("Instance `%s` is `%s` on host `%s` as of this plan. The invoke "+
			"sends `%s` to the machine and doesn't wait for it to get there: the platform doesn't report "+
			"when it has.", id, enumOrNull(got.JSON200.State).ValueString(), deref(got.JSON200.Hostname),
			data.Action.ValueString())))
}

func (a *instancePowerAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	var data instancePowerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(a.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	// The action ends at the answer, so this bounds the one request, which
	// otherwise waits as long as the platform holds it open.
	ctx, cancel := withTimeout(ctx, data.Timeouts.Invoke, a.meta.Wait.Timeout, &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}
	run := powerRun{
		meta: a.meta, target: target,
		id: data.InstanceID.ValueString(), action: data.Action.ValueString(),
		send: resp.SendProgress, diags: &resp.Diagnostics,
	}
	run.power(ctx)
}

// powerRun is one invocation of the power action.
type powerRun struct {
	meta   *Meta
	target clusterTarget
	id     string
	action string
	send   func(action.InvokeProgressEvent)
	diags  *diag.Diagnostics
}

// say streams one progress line to Terraform, which shows it as it arrives,
// and logs it too.
func (p powerRun) say(ctx context.Context, format string, args ...any) {
	msg := redactDiagnosticText(fmt.Sprintf(format, args...))
	tflog.Info(ctx, msg, map[string]any{"instance": p.id})
	if p.send != nil {
		p.send(action.InvokeProgressEvent{Message: msg})
	}
}

const powerFailed = "Could not send the power action"

func (p powerRun) power(ctx context.Context) {
	p.say(ctx, "Sending %s to instance %s.", p.action, p.id)
	got, err := p.meta.Client.ExecuteInstancePowerActionWithResponse(ctx, p.target.region, p.target.project, p.id, p.action)
	switch {
	case err != nil:
		// No answer: the request was cut off, by Ctrl-C, the invoke timeout
		// or the connection, and may have reached the platform before it was.
		addAdvisedError(p.diags, powerFailed, p.mayHaveLanded(), err)
		return
	case got.JSON202 != nil:
		state := "an unreported state"
		if got.JSON202.State != nil {
			state = "`" + string(*got.JSON202.State) + "`"
		}
		p.say(ctx, "The platform accepted %s for instance %s, which was %s when it was submitted. "+
			"The platform doesn't report when the machine gets there.", p.action, p.id, state)
		return
	}

	refusal := k0rdent.CheckResponse(got.HTTPResponse, got.Body)
	if refusal == nil {
		refusal = answeredWithout(got.HTTPResponse, "instance")
	}
	addAdvisedError(p.diags, powerFailed, p.refusalAdvice(got.HTTPResponse, got.Body), refusal)
}

// refusalAdvice says what a refused power action means, by who refused it
// and why. Only a 5xx can have changed anything.
func (p powerRun) refusalAdvice(resp *http.Response, body []byte) string {
	apiErr := apiErrorOf(resp, body)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	has := func(reason string) bool { return apiErr != nil && apiErr.HasReason(reason) }
	switch {
	case status >= 500:
		return p.mayHaveLanded()
	case has(powerActionInFlight):
		wait := "the platform's guard interval"
		if s, ok := retryAfterSeconds(resp, apiErr); ok {
			wait = fmt.Sprintf("about %d seconds", s)
		}
		return fmt.Sprintf("Another power action on this instance's machine was accepted a short while ago, "+
			"and the platform takes no other until its guard interval passes. `%s` wasn't sent. Invoke the "+
			"action again in %s if it's still wanted: the provider doesn't send it again on its own, since a "+
			"power action sent twice isn't the same as once.", p.action, wait)
	case has(reservationDrifted):
		return fmt.Sprintf("The platform can't confirm that the machine instance `%s` records is still its "+
			"own, so it refused rather than act on a machine that might not be. Nothing was sent. Read the "+
			"instance's `state` and `state_message`. If they don't explain it, it's for whoever runs the "+
			"platform.", p.id)
	case has(powerTargetUnidentified):
		return "The platform doesn't know this machine's hardware identity yet, so it has nothing to send " +
			"the action to. Nothing was sent. Invoke the action again later."
	case has(powerActionUnsupported):
		return "The machine this instance occupies has no power control, and that won't change for this " +
			"machine. Nothing was sent, and invoking again won't help."
	case has(powerTargetUnreachable):
		return fmt.Sprintf("Instance `%s` exists, but the platform no longer knows the machine it records. "+
			"Nothing was sent. Read the instance's `state` and `state_message`.", p.id)
	case status == http.StatusGone:
		return fmt.Sprintf("Instance `%s` was deleted, so there's no machine to act on, and nothing was sent. "+
			"Remove the action or point `instance_id` at an instance that exists.", p.id)
	case status == http.StatusNotFound:
		return fmt.Sprintf("There's no instance `%s` in project `%s`, region `%s`, so nothing was sent. Check "+
			"`instance_id`, and `region` and `project` or the provider's.", p.id, p.target.project, p.target.region)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "The platform refused the credential. Nothing was sent. A power action needs a role that may " +
			"change instances in this project."
	}
	return ""
}

// mayHaveLanded is the advice for an action that got no answer, or a 5xx: it
// may have reached the platform. The platform's guard claims every action it
// accepts, and keeps the claim when it can't tell whether the dispatch
// started, so a second invoke can be refused although nothing ran. And with
// the guard off, its own dedup is a fixed 60-second bucket, so a repeat isn't
// reliably dropped either. Neither way is invoking again known to be safe.
func (p powerRun) mayHaveLanded() string {
	return fmt.Sprintf("`%s` may have reached the platform before the request failed, so it may or may not "+
		"have run. The platform can refuse another power action on this machine for up to its guard "+
		"interval even if this one never ran. Before invoking again, read the instance's "+
		"`last_power_action_error` and check the host itself.", p.action)
}

// retryAfterSeconds is how long an in-flight refusal says to wait, from the
// Retry-After header or, failing that, details[0].metadata.retryAfterSeconds.
func retryAfterSeconds(resp *http.Response, apiErr *k0rdent.APIError) (int, bool) {
	if resp != nil {
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n >= 0 {
			return n, true
		}
	}
	if apiErr == nil {
		return 0, false
	}
	for _, d := range apiErr.Details {
		meta, ok := d["metadata"].(map[string]any)
		if !ok {
			continue
		}
		if n, ok := meta["retryAfterSeconds"].(float64); ok && n >= 0 {
			return int(n), true
		}
	}
	return 0, false
}
