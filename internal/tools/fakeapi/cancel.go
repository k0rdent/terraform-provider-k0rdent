package main

import (
	"fmt"
	"net/http"
)

// neverSettles in a cluster's display name keeps its create, scale or upgrade
// in flight until it is cancelled, which is how a cluster whose create wait
// timed out, and that a destroy then meets mid-create, is reached.
const neverSettles = "fake-never-settles"

// cancelCluster models `cancelCluster`: admitted only while `creating` or
// `updating`, it settles the cluster `failed` with reason `CANCELLED` and
// `operation.status` `cancelled`, synchronously. Nothing the operation was
// doing lands, so an upgrade leaves the binding where it was.
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

	c.targets = nil
	c.upgradeTo = nil
	c.state = "failed"
	c.stateReason = "CANCELLED"
	c.stateMessage = "the in-flight operation was cancelled"
	c.finish("cancelled")
	c.reads = 0
	writeJSON(w, http.StatusOK, s.clusterBody(region, c, false))
}
