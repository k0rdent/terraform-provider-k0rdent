package main

import (
	"fmt"
	"net/http"
)

// neverSettles in a cluster's display name keeps its create, scale or upgrade
// in flight until it is cancelled, which is how a cluster whose create wait
// timed out, and that a destroy then meets mid-create, is reached.
const neverSettles = "fake-never-settles"

// cancelLoses in a cluster's display name lets a cancel lose the race to the
// operation it names. The cancel is still answered 202 `cancelling`, and then
// the operation lands anyway: the cluster settles `active` and the operation
// `succeeded`. Recorded on v1.3.0-rc.2 in MOCK_MODE, where the mock
// provisioner finishes a create the cancel reached too late to stop.
const cancelLoses = "fake-cancel-loses"

// cancelCluster models `cancelCluster`: admitted only while `creating` or
// `updating`, it answers 202 with the cluster still in that state and
// `operation.status` `cancelling`. The cluster settles `failed` with reason
// `CANCELLED` and `operation.status` `cancelled` settleAfter reads later, and
// a delete in between is refused like any other while it is in flight. One
// carrying cancelLoses settles as if the cancel never arrived.
// Nothing the operation was doing lands, so an upgrade leaves the binding
// where it was.
func (s *state) cancelCluster(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")

	s.mu.Lock()
	defer s.mu.Unlock()
	c, _, ok := clusterKind.find(w, r, s.in(r))
	if !ok {
		return
	}
	// v1.3.0-dev.4 pins a cancel to the operation the caller read, so one
	// acting on a stale view can't stop a newer operation.
	var body struct {
		ExecutionID string `json:"executionId"`
	}
	if !decode(w, r, &body) {
		return
	}
	if body.ExecutionID == "" {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "executionId is required")
		return
	}
	running := ""
	if c.op != nil {
		running = c.op.executionID
	}
	// Retrying a cancel that already landed answers the settled cluster.
	if c.state == "failed" && c.op != nil && c.op.status == "cancelled" && running == body.ExecutionID {
		writeJSON(w, http.StatusOK, s.clusterBody(region, c, false))
		return
	}
	// Retrying one still winding down answers it as it stands.
	if c.op != nil && c.op.status == "cancelling" && running == body.ExecutionID {
		writeJSON(w, http.StatusAccepted, s.clusterBody(region, c, false))
		return
	}
	if (c.state == "creating" || c.state == "updating") && running != body.ExecutionID {
		errorEnvelope(w, http.StatusConflict, "", fmt.Sprintf(
			"cluster %q is running operation %q, not %q; re-read the cluster and cancel its current operation",
			c.id, running, body.ExecutionID), []any{map[string]any{"reason": "CONFLICT_VERSION"}})
		return
	}
	if c.state != "creating" && c.state != "updating" {
		apiError(w, http.StatusConflict, "CONFLICT", fmt.Sprintf(
			"cluster %q is %s; cancel requires an operation in flight (creating or updating)", c.id, c.state))
		return
	}

	c.op.status = "cancelling"
	c.reads = 0
	writeJSON(w, http.StatusAccepted, s.clusterBody(region, c, false))
}

// settleCancel ends a cancel that is winding down, settleAfter reads after it
// was asked for. It reports whether it handled the read.
func (c *cluster) settleCancel() bool {
	if c.op == nil || c.op.status != "cancelling" {
		return false
	}
	c.reads++
	if c.reads < settleAfter {
		return true
	}
	c.reads = 0
	if nameCarries(c.displayName, cancelLoses) {
		c.complete()
		return true
	}
	// As v1.3.0-rc.3's failed settle does: the old count and version stay.
	c.targets, c.upgradeTo = nil, nil
	c.state = "failed"
	c.stateReason = "CANCELLED"
	c.stateMessage = "the in-flight operation was cancelled"
	c.finish("cancelled")
	return true
}
