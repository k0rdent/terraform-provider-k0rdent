package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// churnsOnEveryRead is the marker that makes an object read back differently
// every time. An object whose id carries it is varied, on every GET, in the
// fields the contract says the platform moves on its own, and in nothing else.
//
// The rest of this file follows the rule at the top of main.go: the fake must
// not agree with itself. Everywhere else the fake answers the same bytes for an
// unchanged object, so a plan after a refresh can only fail on something the
// provider did to the value it sent. A live object doesn't hold still like
// that. Its timestamps move, its health is re-sampled, a pool's ready count is
// read live from the reservation, and lists the contract gives no order for
// come back in whatever order the server built them. A provider that let any
// of those reach an attribute the configuration sets would plan a change
// nobody made, and only a fake that moves them can show it doesn't.
//
// An id rather than a display name, unlike the other markers, because every
// object the provider manages has an id and not every one has a display name.
// A webhook subscription's id is the platform's to choose, so for one the
// marker is in its endpoint URL.
// The VPC binding has neither of its own, and churns through the cluster that
// owns the VPC, whose id the VPC's carries.
const churnsOnEveryRead = "fake-churns"

// churning wraps the routes and varies every GET answer that names a marked
// object. Everything else passes through byte for byte, so nothing that
// isn't marked can tell this is here.
func churning(next http.Handler) http.Handler {
	var reads atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, "/v1/") {
			next.ServeHTTP(w, r)
			return
		}
		rec := &heldResponse{header: w.Header(), status: http.StatusOK}
		next.ServeHTTP(rec, r)

		body := rec.body.Bytes()
		if rec.status == http.StatusOK && bytes.Contains(body, []byte(churnsOnEveryRead)) {
			if varied, ok := churnBody(body, reads.Add(1)); ok {
				body = varied
			}
		}
		w.WriteHeader(rec.status)
		if _, err := w.Write(body); err != nil {
			log.Printf("fakeapi: write response: %v", err)
		}
	})
}

// heldResponse keeps a handler's answer back until churning has looked at it.
// Headers go straight through, since nothing here changes one.
type heldResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (h *heldResponse) Header() http.Header         { return h.header }
func (h *heldResponse) WriteHeader(status int)      { h.status = status }
func (h *heldResponse) Write(b []byte) (int, error) { return h.body.Write(b) }

// churnBody varies every marked object in a JSON answer, a single read or a
// list, and reports whether it found one. read is this answer's place in the
// sequence of churned answers, which is what each varied field is a function
// of, so consecutive reads take different shapes and a short run sees them all.
func churnBody(body []byte, read int64) ([]byte, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // counts stay integers rather than becoming floats
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, false
	}
	if !churnWalk(doc, read) {
		return nil, false
	}
	var out bytes.Buffer
	if err := json.NewEncoder(&out).Encode(doc); err != nil {
		return nil, false
	}
	return out.Bytes(), true
}

// churnWalk finds every object whose id carries the marker, wherever it sits.
func churnWalk(v any, read int64) bool {
	found := false
	switch v := v.(type) {
	case map[string]any:
		if id, ok := v["id"].(string); ok && strings.Contains(id, churnsOnEveryRead) {
			churnObject(v, read)
			found = true
		} else if owner, ok := v["serviceAccountId"].(string); ok && strings.Contains(owner, churnsOnEveryRead) {
			// A credential's id is the server's, so it can't carry the
			// marker, and it churns through its service account's.
			churnCredential(v, read)
			found = true
		}
		// The platform picks a webhook subscription's id, so its endpoint
		// carries the marker instead.
		if endpoint, ok := v["url"].(string); ok && strings.Contains(endpoint, churnsOnEveryRead) {
			churnWebhook(v, read)
			found = true
		}
		for _, child := range v {
			found = churnWalk(child, read) || found
		}
	case []any:
		for _, child := range v {
			found = churnWalk(child, read) || found
		}
	}
	return found
}

// churnEpoch is fixedTime as a time, the instant churned timestamps count from.
var churnEpoch = time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)

// churnTime is a timestamp that moves forward one second per churned read.
func churnTime(read int64) string {
	return churnEpoch.Add(time.Duration(read) * time.Second).Format(time.RFC3339)
}

// churnObject varies one object. Each rule names the contract text that says
// the live API moves the field without anybody asking it to.
func churnObject(obj map[string]any, read int64) {
	odd := read%2 == 1

	// Every managed object's schema declares `updatedAt`, and the file store's
	// says it covers "server-driven state transitions", so it moves with no
	// write from the tenant. So does every nested entry's own, a node pool's
	// for one, which is no less a timestamp for sitting one level down.
	obj["updatedAt"] = churnTime(read)
	for key, child := range obj {
		if key != "health" && key != "controlPlane" {
			churnNestedTimestamps(child, read)
		}
	}

	// A file store's `etag` names "the object's current revision", and a
	// server-driven transition is a revision like any other.
	if _, ok := obj["etag"]; ok {
		obj["etag"] = "rev-churn-" + strconv.FormatInt(read, 10)
	}

	// "A watchdog-force-failed cluster may still return to `active` with no
	// user action", so between two reads of a settled cluster it can be
	// `failed`, with a `stateMessage` that is "not stable across releases",
	// and then `active` again with none. Only a cluster carries `health`.
	if _, isCluster := obj["health"]; isCluster && obj["state"] == "active" && read%4 == 2 {
		obj["state"] = "failed"
		obj["stateMessage"] = "the platform watchdog marked the cluster failed (read " + strconv.FormatInt(read, 10) + ")"
	}

	// "Timestamp of the latest persisted in-flight per-pool progress."
	if _, ok := obj["progressUpdatedAt"]; ok {
		obj["progressUpdatedAt"] = churnTime(read)
	}

	// A cluster's health is re-sampled, never driven by a write: "observability-
	// only and never reflects or drives the lifecycle `state`", and `unknown`
	// "whenever the record has gone stale (read-time substitution ...)".
	if _, ok := obj["health"].(map[string]any); ok {
		// `expiresAt` is the "staleness boundary" an observation carries, so
		// it moves with every new one.
		switch read % 3 {
		case 0:
			obj["health"] = map[string]any{"state": "unknown", "expiresAt": nil}
		case 1:
			obj["health"] = map[string]any{
				"state": "healthy", "observedAt": churnTime(read), "expiresAt": churnTime(read + 300),
			}
		default:
			obj["health"] = map[string]any{
				"state": "degraded", "reason": "1 of 3 nodes is not ready",
				"observedAt": churnTime(read), "expiresAt": churnTime(read + 300),
			}
		}
	}

	// The hosted control plane is observed state, stamped when it was seen,
	// and a replica can drop out of ready between two observations.
	if cp, ok := obj["controlPlane"].(map[string]any); ok {
		if hosted, ok := cp["hosted"].(map[string]any); ok {
			hosted["observedAt"] = churnTime(read)
			churnReplicas(hosted, read)
		}
	}

	// "On `getCluster` and `listClusters`, presence is the signal": the field
	// comes and goes as the catalog publishes and retires versions, which
	// happens with no change to the cluster.
	if !odd {
		delete(obj, "nextAvailableClusterTypeVersion")
	}

	if pools, ok := obj["nodePools"].([]any); ok {
		// The contract gives no order for a cluster's or a group's pools.
		if odd {
			slices.Reverse(pools)
		}
		for _, p := range pools {
			if pool, ok := p.(map[string]any); ok {
				churnHostCounts(pool, read)
				churnPoolTarget(pool, read)
			}
		}
	}

	// "Mount targets published by the platform for this store", with no order
	// given for them.
	if locations, ok := obj["exportLocations"].([]any); ok && odd {
		slices.Reverse(locations)
	}

	// Observed per-consumer progress, "keyed by `ref`", so the list's order
	// carries nothing, and a group's resolved addresses are the addresses
	// "that entry actually authored", in no stated order either.
	if statuses, ok := obj["consumerStatus"].([]any); ok && odd {
		slices.Reverse(statuses)
		for _, s := range statuses {
			if status, ok := s.(map[string]any); ok {
				if addresses, ok := status["resolvedAddresses"].([]any); ok {
					slices.Reverse(addresses)
				}
			}
		}
	}
}

// churnHostCounts varies an instance-group pool's ready and not-ready counts.
// They are "read LIVE from the pool's RESERVATION", "absent when that read is
// unavailable", and a host that is "acquiring, failed, mid-transition" counts
// as not ready, so between two reads of an unchanged group one host can drop
// out of ready, or the count can go missing altogether. `desiredCount` is "the
// tenant's number: only a create or a scale changes it", so it never moves.
func churnHostCounts(pool map[string]any, read int64) {
	ready, ok := pool["readyCount"].(json.Number)
	if !ok {
		return
	}
	switch read % 3 {
	case 0:
		delete(pool, "readyCount")
		delete(pool, "notReadyCount")
	case 2:
		n, err := ready.Int64()
		if err != nil || n == 0 {
			return
		}
		m := int64(0)
		if notReady, ok := pool["notReadyCount"].(json.Number); ok {
			if m, err = notReady.Int64(); err != nil {
				return
			}
		}
		pool["readyCount"], pool["notReadyCount"] = n-1, m+1
	}
}

// churnNestedTimestamps moves `updatedAt` on every object nested under a
// marked one.
func churnNestedTimestamps(v any, read int64) {
	switch v := v.(type) {
	case map[string]any:
		if _, ok := v["updatedAt"]; ok {
			v["updatedAt"] = churnTime(read)
		}
		for _, child := range v {
			churnNestedTimestamps(child, read)
		}
	case []any:
		for _, child := range v {
			churnNestedTimestamps(child, read)
		}
	}
}

// churnReplicas takes one hosted control-plane replica out of ready on every
// third read, keeping ready and unavailable adding up to the desired count.
func churnReplicas(hosted map[string]any, read int64) {
	desired, ok := hosted["desiredReplicas"].(json.Number)
	if !ok || read%3 != 2 {
		return
	}
	n, err := desired.Int64()
	if err != nil || n == 0 {
		return
	}
	hosted["readyReplicas"], hosted["unavailableReplicas"] = n-1, 1
	hosted["message"] = strconv.FormatInt(n-1, 10) + " of " + strconv.FormatInt(n, 10) +
		" control-plane replicas are ready"
}

// churnPoolTarget gives a cluster pool a `targetNodeCount` on odd reads. It's
// present "on a pool whose scale was force-failed until the retry window
// retires", which a refresh can catch with nothing asked for, and here it's
// the settled count, so the pool isn't moving anywhere.
func churnPoolTarget(pool map[string]any, read int64) {
	count, ok := pool["nodeCount"]
	if !ok || read%2 == 0 {
		return
	}
	if _, pending := pool["targetNodeCount"]; !pending {
		pool["targetNodeCount"] = count
	}
}

// churnCredential varies a service account credential. `lastUsedAt` is "the
// last successful token mint using this credential" and `lastUsedIp` where it
// came from, so both move every time a pipeline authenticates with it, and
// both are absent until it first does. Nothing else on a credential moves
// without a write but `status`, and that only once `expiresAt` has passed.
func churnCredential(obj map[string]any, read int64) {
	if read%2 == 0 {
		delete(obj, "lastUsedAt")
		delete(obj, "lastUsedIp")
		return
	}
	obj["lastUsedAt"] = churnTime(read)
	obj["lastUsedIp"] = "203.0.113." + strconv.FormatInt(read%250+1, 10)
}
