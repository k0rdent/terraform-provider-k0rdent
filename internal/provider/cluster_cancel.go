package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// What a timed-out cluster change tells the user. Nothing is undone on a
// failed wait, so the operation keeps running. A destroy cancels it to delete
// the cluster, when `on_destroy_busy` lets it, whether it's `terraform
// destroy` or the destroy that starts an apply replacing the cluster. The
// cancel action cancels it and keeps the cluster. Nothing else does. A create
// whose wait stops is resume.go's to report.
var (
	changeKeepsRunning = "The change is still running on the platform, and the provider does not " +
		"cancel it on a failed wait. The next plan reads where it got to. `terraform destroy` cancels " +
		"it and removes the cluster, unless `on_destroy_busy` is `wait`, when it waits for the change " +
		"to finish first. The `" + ResourceName(clusterCancelOperationActionSuffix) +
		"` action cancels it and keeps the cluster, which the platform then settles `failed`."
)

// addClusterWaitError reports a failed cluster wait, with advice when it timed out.
func addClusterWaitError(diags *diag.Diagnostics, summary, advice string, err error) {
	if errors.Is(err, errWaitTimeout) {
		addAdvisedError(diags, summary, advice, err)
		return
	}
	addWaitError(diags, summary, err)
}

// warnOnBusyDestroy tells a plan that destroys a cluster, or replaces it,
// what the destroy will do with the operation the cluster is running: cancel
// it, or wait for it, as `on_destroy_busy` says.
//
// The apply says so too once it cancels, but the approval comes first. Mature
// providers wait for the operation: GKE's delete waits for the cluster to come
// to rest, and EKS's retries while an update is in progress. By default this
// one cancels, because the platform offers cancelCluster as the way to delete
// a cluster wedged mid-operation and a destroy that waits would never end on
// one. The cost is that whatever the operation was doing won't finish, so
// it's said before the approval, not after. A destroy told to wait can take as
// long as the operation does, which is said too. It reads state as of the last
// refresh, so an operation started since is only reported by the apply. It
// can't see a tainted cluster that's being replaced either: Terraform plans
// that replacement with a null prior state, so the plan looks like a create
// and only the apply's warning names the cancel.
func warnOnBusyDestroy(ctx context.Context, st tfsdk.State, diags *diag.Diagnostics) {
	var prior, id, onBusy types.String
	// Not worth failing a plan over: a state that can't be read warns nobody.
	if st.GetAttribute(ctx, path.Root("state"), &prior).HasError() ||
		st.GetAttribute(ctx, path.Root("id"), &id).HasError() ||
		st.GetAttribute(ctx, path.Root(onDestroyBusyAttribute), &onBusy).HasError() {
		return
	}
	from := k0rdent.ClusterState(prior.ValueString())
	if !cancellable(from) {
		return
	}
	if destroyBusyOf(onBusy) == destroyWaits {
		// Local: decided from prior state, no request made.
		diags.AddWarning(redactDiagnosticText(fmt.Sprintf("Destroying %s will wait for the operation it is running", id.ValueString())),
			redactDiagnosticText(fmt.Sprintf("The cluster was `%s` at the last refresh. The platform deletes a cluster "+
				"only once it is `active` or `failed`, and `on_destroy_busy` is `wait`, so the destroy waits for "+
				"the running operation to finish and then deletes the cluster. That can take as long as the "+
				"operation does, up to the delete timeout. If the operation outlasts it, nothing is deleted.", from)))
		return
	}
	// Local: decided from prior state, no request made. The id is the
	// platform's word, so it's escaped.
	diags.AddWarning(redactDiagnosticText(fmt.Sprintf("Destroying %s will cancel the operation it is running", id.ValueString())),
		redactDiagnosticText(fmt.Sprintf("The cluster was `%s` at the last refresh. The platform deletes a cluster "+
			"only once it is `active` or `failed`, so the destroy cancels the running operation first and "+
			"then deletes the cluster. Whatever that operation was doing will not finish, and a cancelled "+
			"upgrade leaves the cluster `failed`, which the platform does not upgrade again. If the "+
			"destroy then fails, running it again deletes the `failed` cluster.\n\nTo let the operation finish, wait "+
			"until the cluster reads `active` and plan again, or set `on_destroy_busy = \"wait\"` and apply it "+
			"before destroying. That apply sends nothing and doesn't wait. If the cluster is tainted, run "+
			"`terraform untaint` on it first, or the apply replaces it rather than recording the change.", from)))
}

// startDelete sends deleteCluster and reports whether there is a delete to
// wait for.
//
// What each state means after a refused delete is afterRefusedDelete's call. A
// destroy is the user asking for the cluster to go, so it cancels an
// operation in flight and asks again, or, with `on_destroy_busy = "wait"`,
// waits for the operation to finish and asks again. Only Delete calls this:
// no plan, refresh or update ever cancels.
//
// A delete is asked at most maxDeleteAsks times, but one that follows a wait
// for an operation doesn't count. Another run can start an operation between
// the cluster settling and the delete, and a destroy told to wait keeps
// waiting for as long as the delete timeout allows, pausing longer before
// each wait after the first so that it doesn't spin against a cluster that
// keeps changing.
func (r *clusterResource) startDelete(ctx context.Context, target clusterTarget, id, onBusy string, diags *diag.Diagnostics) bool {
	var refused *k0rdent.DeleteClusterResponse
	cancelled := false
	waits := 0
	// A destroy that stopped waiting left the cluster `deleting`, and this one
	// waits for that delete rather than send another, which the platform
	// answers 409. A failed read leaves it to the DELETE, as before.
	if now, err := r.clusterNow(ctx, target, id); err == nil {
		switch {
		case !now.found:
			return false // already gone
		case now.state == k0rdent.ClusterStateDeleting:
			tflog.Info(ctx, "the cluster is already being deleted, so that delete is waited out rather than sent again",
				map[string]any{"cluster": id})
			return true
		}
	}
	for asked := 0; asked < maxDeleteAsks; {
		deleted, err := r.meta.Client.DeleteClusterWithResponse(ctx, target.region, target.project, id)
		switch {
		case err != nil:
			addAPIError(diags, "delete the cluster", nil, nil, err)
			return false
		case isNotFound(deleted.HTTPResponse, deleted.Body):
			return false // already gone
		case deleted.HTTPResponse != nil && deleted.HTTPResponse.StatusCode < 300:
			return true
		case deleted.HTTPResponse == nil || deleted.HTTPResponse.StatusCode != http.StatusConflict:
			addAPIError(diags, "delete the cluster", deleted.HTTPResponse, deleted.Body, nil)
			return false
		}
		refused = deleted

		current, err := r.clusterNow(ctx, target, id)
		if err != nil {
			addWaitError(diags, "Could not read the cluster after the platform refused to delete it", err)
			return false
		}
		if !current.found {
			return false
		}
		switch afterRefusedDelete(current.state, cancelled, onBusy) {
		case deleteWaits:
			return true
		case deleteAsksAgain:
			asked++
		case deleteGivesUp:
			addAPIError(diags, "delete the cluster", refused.HTTPResponse, refused.Body, nil)
			return false
		case deleteCancelsFirst:
			if !r.cancelOperation(ctx, target, id, current, diags) {
				return false
			}
			cancelled = true
			asked++
		case deleteAwaitsOperation:
			if !r.awaitOperation(ctx, target, id, current, waits, diags) {
				return false
			}
			waits++
		default:
			addWaitError(diags, "Could not delete the cluster", unknownStateError("cluster", string(current.state)))
			return false
		}
	}
	addAPIError(diags, "delete the cluster", refused.HTTPResponse, refused.Body, nil)
	return false
}

// maxDeleteAsks is how many times a destroy sends deleteCluster before it
// reports the refusal, not counting those that follow a wait for an operation.
const maxDeleteAsks = 3

// awaitOperation waits for a `creating` or `updating` cluster to finish its
// operation, for a destroy told not to cancel it. It reports whether the
// delete can be asked again. waited is how many operations this destroy has
// already waited out, and each wait after the first starts after a longer,
// jittered pause.
//
// Where the operation lands isn't judged here. `active` and `failed` are both
// states the platform deletes from, and a delete another run started is
// waited for by the delete that follows.
func (r *clusterResource) awaitOperation(ctx context.Context, target clusterTarget, id string, now clusterNow, waited int, diags *diag.Diagnostics) bool {
	const summary = "The cluster's operation did not finish before the destroy timed out"
	advice := awaitTimedOutAdvice(string(now.state))
	if waited > 0 {
		if err := sleepOrCancel(ctx, fullJitter(backoffCeiling(r.meta.Wait, waited-1, time.Minute))); err != nil {
			addAdvisedError(diags, summary, advice, err)
			return false
		}
	}
	tflog.Info(ctx, "waiting for the cluster's in-flight operation to finish so that it can be deleted",
		map[string]any{"cluster": id, "state": string(now.state), "execution": now.execution, "waited": waited})
	err := waitFor(ctx, r.meta.Wait, "the cluster's operation to finish", func(ctx context.Context) (bool, error) {
		after, readErr := r.clusterNow(ctx, target, id)
		if readErr != nil || !after.found {
			return !after.found, readErr
		}
		return probeVerdict(settledAfterCancel(after.state))
	})
	if err != nil {
		addClusterWaitError(diags, summary, advice, err)
		return false
	}
	return true
}

// clusterNow is what a destroy needs to know about a cluster: whether it
// exists, its lifecycle state, and the operation it's running.
type clusterNow struct {
	found bool
	state k0rdent.ClusterState
	// execution is `operation.executionId`, the id a cancel has to name.
	execution string
	// status is `operation.status`, which reads `cancelled` once a cancel
	// has settled the cluster.
	status k0rdent.OperationStatus
}

// cancelOperation cancels the operation a `creating` or `updating` cluster is
// running and waits for the cluster to settle. It reports whether the delete
// can be asked again.
//
// The platform cancels only the operation the request names, by the
// execution id read from the cluster just before. A cluster that began
// another operation since then refuses the stale id with a 409, so a destroy
// reads it again and cancels the operation now running, once. An operation
// that moves on a second time is left to the user rather than chased.
func (r *clusterResource) cancelOperation(ctx context.Context, target clusterTarget, id string, now clusterNow, diags *diag.Diagnostics) bool {
	from := now.state
	fields := map[string]any{"cluster": id, "state": string(from), "execution": now.execution}
	tflog.Info(ctx, "cancelling the cluster's in-flight operation so that it can be deleted", fields)

	refusedOnce := false
	for {
		if now.execution == "" {
			// The contract requires an id, and a cancel without one is a 400.
			addAdvisedError(diags, "Could not cancel the cluster's in-flight operation", fmt.Sprintf(
				"The cluster is `%s`, and the platform deletes a cluster only once it is `active` or `failed`, "+
					"so the destroy cancels the running operation first. The platform cancels an operation only "+
					"by the id in the cluster's `operation.executionId`, and the cluster names none, so there "+
					"was nothing to cancel. Nothing was deleted and the cluster stays in state. Once the "+
					"cluster names its operation, or settles, the destroy can be run again.", from),
				fmt.Errorf("the cluster is %s and reports no operation.executionId", from))
			return false
		}
		got, err := r.meta.Client.CancelClusterWithResponse(ctx, target.region, target.project, id,
			k0rdent.CancelClusterJSONRequestBody{ExecutionId: now.execution})
		if err != nil {
			addAPIError(diags, "cancel the cluster's in-flight operation", nil, nil, err)
			return false
		}
		if got.JSON200 != nil {
			break
		}
		if isNotFound(got.HTTPResponse, got.Body) {
			return true // gone; the next delete says so
		}
		advice := fmt.Sprintf("The cluster is `%s`, and the platform deletes a cluster only once it is `active` "+
			"or `failed`, so the destroy cancels the running operation first. The platform refused the cancel. "+
			"Nothing was deleted and the cluster stays in state, so the destroy can be run again.", from)
		if got.HTTPResponse != nil && got.HTTPResponse.StatusCode == http.StatusConflict {
			// The operation may have finished on its own since the delete was
			// refused, another delete may have started, or the cluster may
			// have begun another operation, which the stale id can't name.
			again, readErr := r.clusterNow(ctx, target, id)
			if readErr == nil && (!again.found || !cancellable(again.state)) {
				tflog.Info(ctx, "the operation ended before it could be cancelled", fields)
				return true
			}
			if readErr == nil && again.execution != "" && again.execution != now.execution {
				if !refusedOnce {
					tflog.Info(ctx, "the cluster began another operation before the cancel, cancelling that one",
						map[string]any{"cluster": id, "was": now.execution, "now": again.execution})
					refusedOnce = true
					now = again
					from = again.state
					continue
				}
				advice = fmt.Sprintf("The cluster is `%s`. The destroy cancels the running operation first, and "+
					"the platform cancels only the operation a request names. The cluster began another "+
					"operation twice while the provider was cancelling: it now runs `%s`. Nothing was deleted "+
					"and the cluster stays in state. Wait for the cluster to settle, then run the destroy "+
					"again.", again.state, again.execution)
			}
		}
		refusal := k0rdent.CheckResponse(got.HTTPResponse, got.Body)
		if refusal == nil {
			refusal = answeredWithout(got.HTTPResponse, "cluster")
		}
		addAdvisedError(diags, "Could not cancel the cluster's in-flight operation", advice, refusal)
		return false
	}

	diags.AddWarning("Cancelled the cluster's in-flight operation to destroy it", redactDiagnosticText(fmt.Sprintf(
		"The cluster was `%s`, and the platform deletes a cluster only once nothing is running on it, "+
			"so the provider cancelled that operation (`%s`) before deleting the cluster. Whatever the "+
			"operation was doing did not finish.", from, now.execution)))
	tflog.Info(ctx, "cancelled the cluster's in-flight operation", fields)

	err := waitFor(ctx, r.meta.Wait, "the cancelled cluster to settle", func(ctx context.Context) (bool, error) {
		after, readErr := r.clusterNow(ctx, target, id)
		if readErr != nil || !after.found {
			return !after.found, readErr
		}
		return probeVerdict(settledAfterCancel(after.state))
	})
	if err != nil {
		addWaitError(diags, "The cluster did not settle after its operation was cancelled", err)
		return false
	}
	return true
}

// clusterNow reads the cluster's lifecycle state and the operation it's
// running, reporting a 404 as not found.
func (r *clusterResource) clusterNow(ctx context.Context, target clusterTarget, id string) (clusterNow, error) {
	return readClusterNow(ctx, r.meta.Client, target, id)
}

// readClusterNow is clusterNow for any caller holding a client: the destroy's
// cancel and the cancel action read a cluster the same way.
func readClusterNow(ctx context.Context, client *k0rdent.ClientWithResponses, target clusterTarget, id string) (clusterNow, error) {
	got, err := client.GetClusterWithResponse(ctx, target.region, target.project, id, nil)
	if err != nil {
		return clusterNow{}, err
	}
	if isNotFound(got.HTTPResponse, got.Body) {
		return clusterNow{}, nil
	}
	if got.JSON200 == nil {
		if err := k0rdent.CheckResponse(got.HTTPResponse, got.Body); err != nil {
			return clusterNow{}, err
		}
		return clusterNow{}, answeredWithout(got.HTTPResponse, "cluster")
	}
	out := clusterNow{found: true}
	if got.JSON200.State != nil {
		out.state = *got.JSON200.State
	}
	if op := got.JSON200.Operation; op != nil {
		if op.ExecutionId != nil {
			out.execution = *op.ExecutionId
		}
		if op.Status != nil {
			out.status = *op.Status
		}
	}
	return out, nil
}
