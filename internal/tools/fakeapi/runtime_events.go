package main

import (
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// fakeEvent is one line of a cluster's runtime event log.
type fakeEvent struct {
	level, component, stage, message string
}

// record adds a line to the log of the cluster's current operation. A cluster
// with no operation keeps no log.
func (c *cluster) record(level, component, stage, message string) {
	if c.op == nil {
		return
	}
	c.events = append(c.events, fakeEvent{level: level, component: component, stage: stage, message: message})
}

// listClusterRuntimeEvents models the operation's default scope: the lines
// recorded since the current or last operation started, oldest first, a page
// of `limit` at a time behind a cursor that's valid only for this cluster.
// `operation`, `stage` and `since` aren't modelled, because nothing in this
// repository sends them.
func (s *state) listClusterRuntimeEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 100
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			apiError(w, http.StatusUnprocessableEntity, "VALIDATION_FAILED", "limit must be between 1 and 500, got "+raw)
			return
		}
		limit = n
	}
	for _, level := range q["level"] {
		if !slices.Contains([]string{"ok", "info", "warn", "error"}, level) {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "level must be one of ok, info, warn, error, got "+level)
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c, _, ok := clusterKind.find(w, r, s.in(r))
	if !ok {
		return
	}
	start := 0
	if raw := q.Get("after"); raw != "" {
		id, at, found := strings.Cut(raw, ":")
		n, err := strconv.Atoi(at)
		if !found || id != c.id || err != nil || n < 0 || n > len(c.events) {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "after is not a cursor this cluster issued")
			return
		}
		start = n
	}
	page := []any{}
	next := start
	for ; next < len(c.events) && len(page) < limit; next++ {
		e := c.events[next]
		if levels := q["level"]; len(levels) > 0 && !slices.Contains(levels, e.level) {
			continue
		}
		line := map[string]any{
			"occurredAt": fixedTime, "level": e.level, "component": e.component,
			"message": e.message, "operation": c.op.executionID,
		}
		if e.stage != "" {
			line["stage"] = e.stage
		}
		page = append(page, line)
	}
	body := map[string]any{"events": page}
	if len(page) > 0 {
		body["cursor"] = fmt.Sprintf("%s:%d", c.id, next)
	}
	writeJSON(w, http.StatusOK, body)
}
