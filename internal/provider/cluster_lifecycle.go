package provider

import (
	"net/http"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// What may happen to a cluster in each state, and what a `cluster_type` edit
// asks of it.
//
// Every caller that has to decide something from a cluster's state asks here:
// plan and apply for a `cluster_type` edit, a scale, a destroy, a cancel, the
// cancel action and a kubeconfig read. The waits ask clusterStates, the cluster's settle and
// delete tables below, through the settle module. The functions here take
// what was read and return a decision. The callers make the requests and word
// the diagnostics.
//
// Keeping plan and apply on one classification is the point. Before, each
// copied the same predicate, and a fix to one would have planned an upgrade
// that the apply then ran as a scale, or the reverse.

// clusterStates is what a cluster's waits make of each state it reports.
var clusterStates = stateKind[k0rdent.Cluster]{
	name:     "cluster",
	resource: clusterResourceSuffix,
	long:     true,
	// DeleteCluster soft-deletes the node pools as it moves the cluster to
	// `deleting`, and a failed teardown leaves them gone. See teardown_read.go.
	released: []string{string(k0rdent.ClusterStateDeleting), string(k0rdent.ClusterStateFailed),
		string(k0rdent.ClusterStateDeleted)},
	state: func(c k0rdent.Cluster) (string, bool) {
		if c.State == nil {
			return "", false
		}
		return string(*c.State), true
	},
	detail: func(c k0rdent.Cluster) string {
		return stateDetail(c.StateMessage, reasonString(c.StateReason))
	},
	settle: stateTable{
		// Required by the contract, absent in practice: there is nothing to
		// wait on, so treat the read as settled rather than spin.
		missing: stateRule{verdict: settled},
		states: map[string]stateRule{
			string(k0rdent.ClusterStateActive):   {verdict: settled},
			string(k0rdent.ClusterStateCreating): {verdict: keepWaiting},
			string(k0rdent.ClusterStateUpdating): {verdict: keepWaiting},
			string(k0rdent.ClusterStateDeleting): {verdict: keepWaiting},
			// The contract says `failed` "is not a stop condition for a poller
			// ... a watchdog-force-failed cluster may still return to `active`
			// with no user action". Waiting through it for good would mean a
			// `terraform apply` that hangs for half an hour on a cluster the
			// platform has already given up on, so it is held for the
			// provisional window and then reported. Nobody has watched the real
			// sequence. The transition trace is the first thing to record when
			// a landscape exists.
			string(k0rdent.ClusterStateFailed):  {verdict: failsProvisionally, detail: true},
			string(k0rdent.ClusterStateDeleted): {verdict: failsTerminally},
		},
	},
	delete: stateTable{
		missing: stateRule{verdict: keepWaiting},
		states: map[string]stateRule{
			string(k0rdent.ClusterStateDeleting): {verdict: keepWaiting},
			// The contract describes `deleted` as "a short-lived tombstone
			// returned until the row is garbage-collected" and says "clients
			// should treat a subsequent 404 as the canonical end state". So 404
			// is what is waited for, not the tombstone.
			string(k0rdent.ClusterStateDeleted): {verdict: keepWaiting},
			// Reading the body and then ignoring `state` made every answer
			// "keep polling", so a delete the platform refused sat in `failed`
			// for the full operation_timeout and reported a timeout instead of
			// the refusal.
			string(k0rdent.ClusterStateFailed):   {verdict: failsTerminally, detail: true},
			string(k0rdent.ClusterStateActive):   {verdict: failsTerminally, says: afterDelete},
			string(k0rdent.ClusterStateCreating): {verdict: failsTerminally, says: afterDelete},
			string(k0rdent.ClusterStateUpdating): {verdict: failsTerminally, says: afterDelete},
		},
	},
}

// refChange is what moving `cluster_type` from the bound reference to the
// configured one asks of a cluster.
type refChange int

const (
	// refUnchanged is the same binding, perhaps spelled differently: an origin
	// added or dropped, or the head of the bound version. It needs no call.
	refUnchanged refChange = iota
	// refUpgrade is another version of the same ClusterType, which the
	// platform reaches in place. Whether that version can be reached needs the
	// version list, which the caller reads.
	refUpgrade
	// refRefused is an upgrade the cluster's state does not admit.
	refRefused
	// refReplace is another ClusterType, which only a new cluster can have.
	refReplace
)

// classifyRefChange says what configured asks of a cluster bound to bound in
// state, and for a refusal, why. The replacement guard has no state to give
// and passes "", which never refuses.
func classifyRefChange(configured, bound string, state k0rdent.ClusterState) (refChange, string) {
	switch {
	case clusterTypeRefEquivalent(configured, bound) != refMismatch:
		return refUnchanged, ""
	case !sameClusterType(configured, bound):
		return refReplace, ""
	}
	if refusal := upgradeRefusal(state); refusal != "" {
		return refRefused, refusal
	}
	return refUpgrade, ""
}

// upgradeRefusal says why a cluster in state cannot start an upgrade, or ""
// when it can. A cluster that is busy can: the upgrade waits for it to settle
// first.
func upgradeRefusal(state k0rdent.ClusterState) string {
	if state == k0rdent.ClusterStateFailed {
		return "The cluster is `failed`, and the platform does not retry an upgrade from `failed`: it admits " +
			"one only from `active`, and has no call to resume one."
	}
	return ""
}

// scaleMayRetry says whether a refused scale is worth sending again. A 409
// with no reason on an active cluster is the few seconds its quota accounting
// takes to catch up, and the contract says to retry. The same 409 on a cluster
// that is busy or failed, or one with a reason such as QUOTA_EXCEEDED, is the
// answer. state reads the cluster, and is called only for a refusal that could
// be the window. Its ok is false when the read did not say.
func scaleMayRetry(refusal *k0rdent.APIError, state func() (k0rdent.ClusterState, bool)) bool {
	bare := refusal.StatusCode == http.StatusConflict && refusal.Reason == "" && len(refusal.DetailReasons()) == 0
	if !bare {
		return false
	}
	now, ok := state()
	return ok && now == k0rdent.ClusterStateActive
}

// deleteStep is what a destroy does once the platform has refused the delete
// with a 409 and the cluster has been read again.
type deleteStep int

const (
	// deleteUnrecognised is a state the contract does not name.
	deleteUnrecognised deleteStep = iota
	// deleteWaits is a delete already running, which the destroy waits for.
	deleteWaits
	// deleteAsksAgain is a cluster that has settled since the refusal.
	deleteAsksAgain
	// deleteCancelsFirst is an operation in flight. The platform deletes a
	// cluster only from `active` or `failed`, and its 409 names cancelCluster
	// as the remedy.
	deleteCancelsFirst
	// deleteGivesUp is an operation in flight after one was already
	// cancelled, so the refusal is the answer.
	deleteGivesUp
	// deleteAwaitsOperation is an operation in flight that the destroy was
	// told to wait for, with `on_destroy_busy = "wait"`, rather than cancel.
	deleteAwaitsOperation
)

// afterRefusedDelete says what a destroy does with a cluster read in state
// after a refused delete. cancelled says whether this destroy has cancelled
// an operation already, and onBusy is what `on_destroy_busy` asks of an
// operation in flight: destroyCancels or destroyWaits.
func afterRefusedDelete(state k0rdent.ClusterState, cancelled bool, onBusy string) deleteStep {
	switch state {
	case k0rdent.ClusterStateDeleting, k0rdent.ClusterStateDeleted:
		return deleteWaits
	case k0rdent.ClusterStateActive, k0rdent.ClusterStateFailed:
		return deleteAsksAgain
	case k0rdent.ClusterStateCreating, k0rdent.ClusterStateUpdating:
		switch {
		case onBusy == destroyWaits:
			return deleteAwaitsOperation
		case cancelled:
			return deleteGivesUp
		}
		return deleteCancelsFirst
	default:
		return deleteUnrecognised
	}
}

// cancellable says whether a cluster in state is running an operation a
// cancel stops. A refused cancel on a cluster that no longer is means the
// operation ended on its own, or another delete started.
func cancellable(state k0rdent.ClusterState) bool {
	return state == k0rdent.ClusterStateCreating || state == k0rdent.ClusterStateUpdating
}

// cancelStep is what the cancel action does with a cluster it has read.
type cancelStep int

const (
	// cancelUnrecognised is a state the contract does not name.
	cancelUnrecognised cancelStep = iota
	// cancelSends is an operation in flight that the action was asked to
	// stop, named by the cluster's `operation.executionId`.
	cancelSends
	// cancelNothingRunning is a settled cluster. That's the outcome a cancel
	// is for, so there is nothing to do but say nothing was cancelled.
	cancelNothingRunning
	// cancelAlreadyCancelled is a cluster a cancel has settled already: the
	// operation asked about, or the last one when none was named, reads
	// `cancelled`.
	cancelAlreadyCancelled
	// cancelOtherOperation is an operation in flight other than the one the
	// action was asked to stop.
	cancelOtherOperation
	// cancelNoExecution is an operation in flight that the cluster doesn't
	// name, which the platform can't cancel without an id.
	cancelNoExecution
	// cancelDeleting is a delete in flight, which has no cancel.
	cancelDeleting
)

// cancelStepFor says what the cancel action does with a cluster read as now,
// when it was asked to stop the operation named want, or whatever is running
// when want is "".
//
// It only ever stops the operation it was asked about, or the one it read.
// The destroy chases an operation that moved on, because a destroy is the
// user asking for the cluster to go and anything running is in its way. The
// action is the user asking for one operation to stop, and an operation that
// started since is one nobody has looked at yet.
func cancelStepFor(now clusterNow, want string) cancelStep {
	switch now.state {
	case k0rdent.ClusterStateCreating, k0rdent.ClusterStateUpdating:
		switch {
		case now.execution == "":
			return cancelNoExecution
		case want != "" && want != now.execution:
			return cancelOtherOperation
		}
		return cancelSends
	case k0rdent.ClusterStateActive, k0rdent.ClusterStateFailed:
		if now.status == k0rdent.OperationStatusCancelled && (want == "" || want == now.execution) {
			return cancelAlreadyCancelled
		}
		return cancelNothingRunning
	case k0rdent.ClusterStateDeleting, k0rdent.ClusterStateDeleted:
		return cancelDeleting
	default:
		return cancelUnrecognised
	}
}

// settledAfterCancel is what the wait after a cancel makes of state, and the
// wait for an operation a destroy was told not to cancel. Unlike a settle,
// `failed` ends it, because a cancelled operation leaves the cluster `failed`
// and so does one that failed on its own, and so does a delete, which the
// destroy then waits for. Either way the delete can be asked again.
func settledAfterCancel(state k0rdent.ClusterState) (verdict, error) {
	switch state {
	case k0rdent.ClusterStateActive, k0rdent.ClusterStateFailed,
		k0rdent.ClusterStateDeleting, k0rdent.ClusterStateDeleted:
		return settled, nil
	case k0rdent.ClusterStateCreating, k0rdent.ClusterStateUpdating:
		return keepWaiting, nil
	default:
		return unrecognised, unknownStateError("cluster", string(state))
	}
}

// kubeconfigOutlook is what a cluster's state says about a kubeconfig that a
// read did not find.
type kubeconfigOutlook int

const (
	// kubeconfigUnrecognised is a state the contract does not name.
	kubeconfigUnrecognised kubeconfigOutlook = iota
	// kubeconfigIssuing is an active cluster. The platform issues once, on
	// the settle that made the cluster active, within thirty seconds, and
	// drops a failed issue until the next lifecycle change. So a long wait is
	// not issuance, and the caller bounds it.
	kubeconfigIssuing
	// kubeconfigAfterProvisioning is a cluster still creating or updating,
	// which is issued one once it is active.
	kubeconfigAfterProvisioning
	// kubeconfigNotWhileFailed is a failed cluster, which may yet return to
	// active.
	kubeconfigNotWhileFailed
	// kubeconfigNever is a cluster being deleted.
	kubeconfigNever
)

// kubeconfigOutlookFor says what a cluster in state means for its kubeconfig.
func kubeconfigOutlookFor(state k0rdent.ClusterState) kubeconfigOutlook {
	switch state {
	case k0rdent.ClusterStateActive:
		return kubeconfigIssuing
	case k0rdent.ClusterStateCreating, k0rdent.ClusterStateUpdating:
		return kubeconfigAfterProvisioning
	case k0rdent.ClusterStateFailed:
		return kubeconfigNotWhileFailed
	case k0rdent.ClusterStateDeleting, k0rdent.ClusterStateDeleted:
		return kubeconfigNever
	default:
		return kubeconfigUnrecognised
	}
}
