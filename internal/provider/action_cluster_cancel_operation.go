package provider

import (
	"context"
	"fmt"
	"net/http"
	"time"

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

// The cancel action, Terraform 1.14 and later.
//
// Before it, a cluster's operation was cancelled only as part of a destroy,
// and a wedged create, scale or upgrade could be stopped only by deleting the
// cluster. Cancelling from an ordinary apply would stop an operation the user
// asked for, so no plan or apply does it. An action is the other way: it runs
// only when a configuration invokes it by name, with `terraform apply
// -invoke`, or binds it to a resource with `action_trigger`. Nothing in a
// plan or an apply reaches it otherwise.

const clusterCancelOperationActionSuffix = "cluster_cancel_operation"

var (
	_ action.Action               = (*clusterCancelOperationAction)(nil)
	_ action.ActionWithConfigure  = (*clusterCancelOperationAction)(nil)
	_ action.ActionWithModifyPlan = (*clusterCancelOperationAction)(nil)
)

func newClusterCancelOperationAction() action.Action {
	return &clusterCancelOperationAction{}
}

type clusterCancelOperationAction struct{ meta *Meta }

type clusterCancelOperationModel struct {
	Region      types.String   `tfsdk:"region"`
	Project     types.String   `tfsdk:"project"`
	ClusterID   types.String   `tfsdk:"cluster_id"`
	ExecutionID types.String   `tfsdk:"execution_id"`
	Timeouts    timeouts.Value `tfsdk:"timeouts"`
}

func (a *clusterCancelOperationAction) Metadata(_ context.Context, _ action.MetadataRequest, resp *action.MetadataResponse) {
	resp.TypeName = ResourceName(clusterCancelOperationActionSuffix)
}

func (a *clusterCancelOperationAction) Schema(ctx context.Context, _ action.SchemaRequest, resp *action.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Cancels the operation a cluster is running: a create, a scale or an upgrade " +
			"that is still `creating` or `updating`. The cluster stays. Invoke it by name with " +
			"`terraform apply -invoke=action." + ResourceName(clusterCancelOperationActionSuffix) +
			".<name>`, or bind it to a resource with `action_trigger`. Actions need Terraform 1.14 or " +
			"later, and nothing else in this provider needs it.\n\n" +
			"A wait that times out leaves the operation running. Otherwise only a destroy cancels it, " +
			"to delete the cluster: `terraform destroy`, or the destroy that starts an apply replacing " +
			"the cluster, a tainted one included, unless `on_destroy_busy` is `wait`. This action is the " +
			"way to stop an operation and keep the cluster.\n\n" +
			"It reads the cluster and cancels the operation running then, by the `operation.executionId` " +
			"the platform reports, or only the one `execution_id` names. The platform settles a cancelled " +
			"cluster `failed`, with `operation.status` `cancelled`, and the action waits for that and " +
			"reports each state it reads as progress. Whatever the operation was doing doesn't finish. " +
			"An accepted cancel can still lose the race: when the operation finishes or fails first, the " +
			"action says so, with a warning that nothing was cancelled. " +
			"The platform admits neither a scale nor an upgrade from `failed`, so the cluster then waits " +
			"on the platform to recover it, or on a destroy.\n\n" +
			"A plan that invokes it reads the cluster and warns what it will cancel. Left unpinned, the " +
			"invoke cancels whatever is running when it runs, which can be an operation that started after " +
			"the plan. Set `execution_id` to cancel only the one the plan read.\n\n" +
			"A cluster with nothing in flight succeeds with a warning that nothing was cancelled, as does " +
			"an operation that finishes or was cancelled before the request arrives: no operation " +
			"running is the outcome the action is for, so running it twice is safe. It fails, and " +
			"cancels nothing, when the cluster doesn't exist, when it's being deleted (a delete has no " +
			"cancel), when the operation running isn't the one `execution_id` names, when the cluster " +
			"began another operation before the cancel arrived, and when the platform refuses the " +
			"cancel. It never cancels an operation it didn't read or wasn't asked about. Run it again " +
			"to cancel that one.",
		Attributes: map[string]schema.Attribute{
			"cluster_id": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "The cluster whose operation to cancel, such as a `" +
					ResourceName(clusterResourceSuffix) + "` resource's `id`.",
				Validators: []validator.String{urlSafeIdentifier{}},
			},
			// A cluster can be created outside the provider's scope, so the
			// cancel has to reach it there too, as the kubeconfig does.
			"region": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Region the cluster is in. Defaults to the provider's `region`.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"project": schema.StringAttribute{
				Optional:            true,
				MarkdownDescription: "Project the cluster is in. Defaults to the provider's `project`.",
				Validators:          []validator.String{urlSafeIdentifier{}},
			},
			"execution_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "The operation to cancel, as the cluster's `operation.executionId` " +
					"names it. When set, the action cancels this operation only, and refuses when the " +
					"cluster is running another. Left out, it cancels whatever the cluster is running " +
					"when the action reads it.",
				// Empty would read as "whatever is running" and cancel an
				// operation the configuration meant to pin.
				Validators: []validator.String{stringvalidator.LengthAtLeast(1)},
			},
		},
		Blocks: map[string]schema.Block{
			"timeouts": timeouts.Block(ctx),
		},
	}
	documentActionSchema(&resp.Schema)
}

func (a *clusterCancelOperationAction) Configure(_ context.Context, req action.ConfigureRequest, resp *action.ConfigureResponse) {
	a.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

// ModifyPlan reads the cluster and says which operation the invoke will
// cancel, so it's in front of the approval. It sends no cancel, and a cluster
// it can't read yet, such as one this run creates, is left to the invoke.
func (a *clusterCancelOperationAction) ModifyPlan(ctx context.Context, req action.ModifyPlanRequest, resp *action.ModifyPlanResponse) {
	if a.meta == nil {
		return
	}
	var data clusterCancelOperationModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	for _, v := range []types.String{data.ClusterID, data.Region, data.Project, data.ExecutionID} {
		if v.IsUnknown() {
			return
		}
	}
	if data.ClusterID.IsNull() {
		return
	}
	var scope diag.Diagnostics
	target, ok := resolveScope(a.meta, data.Region, data.Project, &scope)
	if !ok {
		return // the invoke reports it
	}
	id := data.ClusterID.ValueString()
	now, err := readClusterNow(ctx, a.meta.Client, target, id)
	if err != nil || !now.found || cancelStepFor(now, data.ExecutionID.ValueString()) != cancelSends {
		return
	}
	// Local: decided from the read, no cancel sent. The ids are the
	// platform's words, so they're escaped.
	after := "An operation that finishes before the invoke runs leaves nothing to cancel, and the invoke says so."
	if data.ExecutionID.IsNull() {
		// Unpinned, the invoke reads the cluster again and cancels whatever
		// runs then, which may be an operation that started after this plan.
		resp.Diagnostics.AddWarning(
			redactDiagnosticText(fmt.Sprintf("Invoking this cancels whatever cluster %s is running, now %s", id, now.execution)),
			redactDiagnosticText(fmt.Sprintf("The cluster is `%s` as of this plan, running `%s`. With no "+
				"`execution_id`, the invoke cancels whatever the cluster is running when it runs, which "+
				"may be an operation that started after this plan rather than `%s`. To cancel only that one, "+
				"set `execution_id = %q`. Whatever the cancelled operation was doing won't finish, and the "+
				"platform settles a cancelled cluster `failed`, which it neither scales nor upgrades. %s",
				now.state, now.execution, now.execution, now.execution, after)))
		return
	}
	resp.Diagnostics.AddWarning(
		redactDiagnosticText(fmt.Sprintf("Invoking this cancels operation %s on cluster %s", now.execution, id)),
		redactDiagnosticText(fmt.Sprintf("The cluster is `%s` as of this plan, running `%s`. The invoke cancels "+
			"that operation and no other, and waits for the cluster to settle. Whatever the operation was "+
			"doing won't finish, and the platform settles a cancelled cluster `failed`, which it neither "+
			"scales nor upgrades. %s", now.state, now.execution, after)))
}

func (a *clusterCancelOperationAction) Invoke(ctx context.Context, req action.InvokeRequest, resp *action.InvokeResponse) {
	var data clusterCancelOperationModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &data)...)
	if resp.Diagnostics.HasError() {
		return
	}
	target, ok := resolveScope(a.meta, data.Region, data.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	ctx, cancel := withTimeout(ctx, data.Timeouts.Invoke, a.meta.Wait.Timeout, &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return
	}
	wait := a.meta.Wait
	wait.Raise = "the action's `timeouts.invoke`, or `operation_timeout` in the provider block"
	run := cancelRun{
		client: a.meta.Client, wait: wait, target: target,
		id: data.ClusterID.ValueString(), want: data.ExecutionID.ValueString(),
		send: resp.SendProgress, diags: &resp.Diagnostics,
	}
	run.cancel(ctx)
}

// cancelRun is one invocation of the cancel action.
type cancelRun struct {
	client *k0rdent.ClientWithResponses
	wait   waitConfig
	target clusterTarget
	id     string
	// want is the operation the configuration names, "" for whatever runs.
	want  string
	send  func(action.InvokeProgressEvent)
	diags *diag.Diagnostics
}

// say streams one progress line to Terraform, which shows it as it arrives,
// and logs it too.
func (c cancelRun) say(ctx context.Context, format string, args ...any) {
	msg := redactDiagnosticText(fmt.Sprintf(format, args...))
	tflog.Info(ctx, msg, map[string]any{"cluster": c.id})
	if c.send != nil {
		c.send(action.InvokeProgressEvent{Message: msg})
	}
}

// nothingToCancel ends a run that found no operation to stop. It's a
// success, because that's what the action is for, and a warning, because the
// user asked for a cancel and none was sent.
func (c cancelRun) nothingToCancel(ctx context.Context, detail string) {
	c.say(ctx, "Nothing to cancel on cluster %s.", c.id)
	c.diags.AddWarning(redactDiagnosticText("Nothing to cancel on cluster "+c.id), redactDiagnosticText(detail))
}

func (c cancelRun) fail(detail string) {
	c.diags.AddError("Could not cancel the cluster's operation", redactDiagnosticText(detail))
}

func (c cancelRun) cancel(ctx context.Context) {
	now, err := readClusterNow(ctx, c.client, c.target, c.id)
	if err != nil {
		addWaitError(c.diags, "Could not read the cluster to cancel its operation", err)
		return
	}
	if !now.found {
		c.fail(fmt.Sprintf("The platform answered 404 for cluster `%s` in project `%s`, region `%s`, so there "+
			"is no operation to cancel. Check `cluster_id`, and `region` and `project` or the provider's.",
			c.id, c.target.project, c.target.region))
		return
	}
	if now.execution != "" {
		c.say(ctx, "Cluster %s is %s, and its operation %s reads %s.", c.id, now.state, now.execution, now.status)
	} else {
		c.say(ctx, "Cluster %s is %s.", c.id, now.state)
	}
	if !c.decide(ctx, now, c.want) {
		return
	}

	c.say(ctx, "Cancelling operation %s.", now.execution)
	got, err := c.client.CancelClusterWithResponse(ctx, c.target.region, c.target.project, c.id,
		k0rdent.CancelClusterJSONRequestBody{ExecutionId: now.execution})
	switch {
	case err != nil:
		// No answer: the request was cut off, by Ctrl-C, the invoke timeout
		// or the connection, and may have reached the platform before it was.
		addAdvisedError(c.diags, "Could not cancel the cluster's operation", c.mayHaveLanded(now), err)
		return
	case got.JSON200 != nil, got.JSON202 != nil:
		// 202 is the stop requested and the run still winding down, 200 a
		// cancel that had already settled. settle waits out either.
		c.settle(ctx, now)
		return
	case isNotFound(got.HTTPResponse, got.Body):
		c.nothingToCancel(ctx, fmt.Sprintf("Cluster `%s` was deleted before the cancel arrived, so the "+
			"platform answered 404 and there's no operation left to cancel.", c.id))
		return
	}

	refusal := k0rdent.CheckResponse(got.HTTPResponse, got.Body)
	if refusal == nil {
		refusal = answeredWithout(got.HTTPResponse, "cluster")
	}
	advice := c.refusalAdvice(now, got.HTTPResponse)
	if got.HTTPResponse != nil && got.HTTPResponse.StatusCode == http.StatusConflict {
		// The operation may have finished on its own since the read, a delete
		// may have started, or the cluster may have begun another operation,
		// which the id read before can't name. Reading again tells which.
		again, readErr := readClusterNow(ctx, c.client, c.target, c.id)
		switch {
		case readErr != nil:
		case !again.found:
			c.nothingToCancel(ctx, fmt.Sprintf("Cluster `%s` was deleted before the cancel arrived, so "+
				"there's no operation left to cancel.", c.id))
			return
		case cancelStepFor(again, now.execution) == cancelNothingRunning:
			c.nothingToCancel(ctx, fmt.Sprintf("Operation `%s` finished before the cancel arrived: the "+
				"platform refused the cancel with 409, and cluster `%s` now reads `%s` with nothing in flight. "+
				"Nothing was cancelled.", now.execution, c.id, again.state))
			return
		case cancelStepFor(again, now.execution) == cancelAlreadyCancelled:
			c.nothingToCancel(ctx, fmt.Sprintf("Operation `%s` was already cancelled when this cancel arrived, "+
				"and cluster `%s` reads `%s`.", now.execution, c.id, again.state))
			return
		case cancelStepFor(again, now.execution) == cancelOtherOperation:
			advice = fmt.Sprintf("The cancel named `%s`, and cluster `%s` began another operation, `%s`, before "+
				"it arrived. The platform cancels only the operation a request names, and this action cancels "+
				"only the one it read, so `%s` is still running. Nothing was cancelled. To cancel `%s` too, "+
				"invoke the action again.", now.execution, c.id, again.execution, again.execution, again.execution)
		}
	}
	addAdvisedError(c.diags, "Could not cancel the cluster's operation", advice, refusal)
}

// refusalAdvice says what a refused cancel means, by who refused it and why.
// Only a 5xx can have changed anything.
func (c cancelRun) refusalAdvice(now clusterNow, resp *http.Response) string {
	was := fmt.Sprintf("The cluster was `%s`, running `%s`. ", now.state, now.execution)
	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	switch {
	case status >= 500:
		return c.mayHaveLanded(now)
	case status == http.StatusBadRequest:
		return was + "The platform refused the cancel as malformed. Nothing was cancelled. The request is the " +
			"provider's, not the configuration's, so invoking again won't help."
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return was + "The platform refused the credential. Nothing was cancelled. Cancelling needs a role " +
			"that may change clusters in this project."
	case status == http.StatusConflict:
		return was + "The platform refused the cancel for the cluster's state. Nothing was cancelled. " +
			"Read the cluster before invoking the action again."
	}
	return was + "The platform refused the cancel. Nothing was cancelled."
}

// mayHaveLanded is the advice for a cancel that got no answer, or a 5xx: it
// may have reached the platform, so the cluster says what happened.
func (c cancelRun) mayHaveLanded(now clusterNow) string {
	return fmt.Sprintf("The cluster was `%s`, running `%s`. The cancel may have landed before the request "+
		"failed. Read the cluster before invoking the action again: `failed` with `operation.status` "+
		"`cancelled` means it did. Invoking again is safe either way, since an operation already cancelled "+
		"ends in a warning, not another cancel.", now.state, now.execution)
}

// decide says whether to send the cancel for a cluster read as now, and
// otherwise ends the run with what it found.
func (c cancelRun) decide(ctx context.Context, now clusterNow, want string) bool {
	switch cancelStepFor(now, want) {
	case cancelSends:
		return true
	case cancelNothingRunning:
		named := ""
		if want != "" {
			named = fmt.Sprintf(" Operation `%s` isn't running.", want)
		}
		c.nothingToCancel(ctx, fmt.Sprintf("Cluster `%s` is `%s` and has no operation in flight, so there was "+
			"nothing to cancel and no cancel was sent.%s", c.id, now.state, named))
	case cancelAlreadyCancelled:
		c.nothingToCancel(ctx, fmt.Sprintf("Operation `%s` on cluster `%s` was already cancelled: the cluster "+
			"is `%s` and its operation reads `cancelled`. No cancel was sent.", now.execution, c.id, now.state))
	case cancelOtherOperation:
		c.fail(fmt.Sprintf("Cluster `%s` is `%s`, running operation `%s`, and this action was asked to cancel "+
			"`%s`. It cancels only the operation it's asked about, so `%s` is still running. Nothing was "+
			"cancelled. To cancel `%s`, set `execution_id` to it, or leave `execution_id` out.",
			c.id, now.state, now.execution, want, now.execution, now.execution))
	case cancelNoExecution:
		c.fail(fmt.Sprintf("Cluster `%s` is `%s`, and the platform cancels an operation only by the id in the "+
			"cluster's `operation.executionId`. The cluster names none, so nothing was cancelled. Once it "+
			"names its operation, invoking the action again cancels it.", c.id, now.state))
	case cancelDeleting:
		c.fail(fmt.Sprintf("Cluster `%s` is `%s`. A delete has no cancel on the platform, so nothing was "+
			"cancelled.", c.id, now.state))
	default:
		addWaitError(c.diags, "Could not cancel the cluster's operation", unknownStateError("cluster", string(now.state)))
	}
	return false
}

// settle waits for the cluster to come to rest after an accepted cancel,
// saying each state it reads and, during a long wait, that it's still
// waiting.
func (c cancelRun) settle(ctx context.Context, was clusterNow) {
	c.say(ctx, "The platform accepted the cancel of %s.", was.execution)
	var last k0rdent.ClusterState
	var lastSaid time.Time
	var final clusterNow
	gone := false
	err := waitFor(ctx, c.wait, "the cluster whose operation was cancelled", func(ctx context.Context) (bool, error) {
		after, readErr := readClusterNow(ctx, c.client, c.target, c.id)
		if readErr != nil {
			return false, readErr
		}
		final = after
		if !after.found {
			gone = true
			return true, nil
		}
		if after.state != last || time.Since(lastSaid) >= cancelProgressEvery {
			c.say(ctx, "Cluster %s is %s.", c.id, after.state)
			last, lastSaid = after.state, time.Now()
		}
		return probeVerdict(settledAfterCancel(after.state))
	})
	if err != nil {
		addAdvisedError(c.diags, "The cluster did not settle after its operation was cancelled",
			fmt.Sprintf("The platform accepted the cancel of `%s`, and that isn't undone. The cluster hadn't "+
				"settled when the action stopped waiting. Read it again before invoking the action again.",
				was.execution), err)
		return
	}
	switch {
	case gone:
		// Nothing is left to read the operation from, so the action can't say
		// the cancel stopped it.
		c.say(ctx, "Cluster %s is gone, so how the cancel of %s ended can't be read.", c.id, was.execution)
		c.diags.AddWarning(redactDiagnosticText("Can't tell whether the cancel of "+was.execution+" stopped it"),
			redactDiagnosticText(fmt.Sprintf("The platform accepted the cancel of `%s`. %s", was.execution,
				unknownCancelEnd(final))))
	case last == k0rdent.ClusterStateDeleting || last == k0rdent.ClusterStateDeleted:
		// Something else started a delete after the cancel was accepted. The
		// action doesn't stop a delete, which has no cancel, so it says so
		// rather than call the cluster settled, and says what the cancel did
		// only when the operation still reads it.
		ending := "The cluster now reports the delete, so how the cancel ended can't be read from it."
		if cancelEndOf(was, final) == cancelEndCancelled {
			ending = "The operation reads `cancelled`: whatever it was doing did not finish."
			c.say(ctx, "Cancelled operation %s. Cluster %s is now being deleted.", was.execution, c.id)
		} else {
			c.say(ctx, "Cluster %s is now being deleted, so how the cancel of %s ended can't be read.", c.id, was.execution)
		}
		c.diags.AddWarning(redactDiagnosticText("Cluster "+c.id+" is being deleted"),
			redactDiagnosticText(fmt.Sprintf("The platform accepted the cancel of `%s`, and cluster `%s` then "+
				"read `%s`: a delete started after the cancel. %s A delete has no cancel, so the action leaves "+
				"it running.", was.execution, c.id, last, ending)))
	default:
		c.ended(ctx, was, final)
	}
}

// ended says how an accepted cancel ended, from the operation the settled
// cluster reports. Only `cancelled` is a cancel that stopped anything. One
// that finished or failed first succeeds with a warning that nothing was
// cancelled, as an operation that ended before the cancel was sent does.
func (c cancelRun) ended(ctx context.Context, was, after clusterNow) {
	switch cancelEndOf(was, after) {
	case cancelEndCancelled:
		c.say(ctx, "Cancelled operation %s. Cluster %s is %s, with nothing in flight.", was.execution, c.id, after.state)
	case cancelEndFinished:
		c.say(ctx, "Operation %s finished before the cancel took effect. Cluster %s is %s.", was.execution, c.id, after.state)
		c.diags.AddWarning(redactDiagnosticText("Operation "+was.execution+" finished before the cancel took effect"),
			redactDiagnosticText(fmt.Sprintf("The platform accepted the cancel of `%s`, but the operation reads "+
				"`succeeded` and cluster `%s` is `%s`: what it was doing landed. Nothing was cancelled.",
				was.execution, c.id, after.state)))
	case cancelEndFailed:
		c.say(ctx, "Operation %s failed before the cancel took effect. Cluster %s is %s.", was.execution, c.id, after.state)
		c.diags.AddWarning(redactDiagnosticText("Operation "+was.execution+" failed before the cancel took effect"),
			redactDiagnosticText(fmt.Sprintf("The platform accepted the cancel of `%s`, but the operation reads "+
				"`failed` and cluster `%s` is `%s`: it failed on its own first. Nothing was cancelled. The "+
				"cluster's `stateReason` on the platform says why.", was.execution, c.id, after.state)))
	default:
		c.say(ctx, "Cluster %s is %s.", c.id, after.state)
		c.diags.AddWarning(redactDiagnosticText("Can't tell whether the cancel of "+was.execution+" stopped it"),
			redactDiagnosticText("The platform accepted the cancel of `"+was.execution+"`. "+unknownCancelEnd(after)))
	}
}

// cancelProgressEvery is how often a wait with nothing new to say still says
// it's waiting, so a long wait doesn't look hung.
const cancelProgressEvery = 30 * time.Second
