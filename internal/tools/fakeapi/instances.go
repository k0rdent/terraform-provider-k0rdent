package main

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Instances are the hosts an instance group holds. The contract has no create
// for one: they are materialized when a group is created or scaled. So the fake
// stores none and derives them on every read from what each group holds, the
// way the platform reads them from the infrastructure service on every request.
//
// What this follows from the contract, because the provider branches on each:
//
//   - A group holds hosts only once it has provisioned, so a group still
//     `creating` has no instances.
//   - `pageSize` is capped at 200 and a larger one is a 422, not a clamp.
//   - Each host reports one Ethernet interface, carrying `macAddress`, and one
//     InfiniBand interface, carrying `guid`, in the spellings a file store
//     grant's narrowed selector takes.
//   - A host's Ethernet address is the one a grant to its group resolves to,
//     so a grant narrowed to its MAC address grants exactly that address.
//
// ASSUMPTION: every host is `running` and `healthy`. The fake has no host
// lifecycle of its own, and nothing the provider does with an instance
// branches on either. The one exception is an instance whose group was
// deleted, which the contract keeps as a `deleted` record that only
// `showDeleted` or `inStates=deleted` returns. A group's scale can't release
// a host: the contract refuses a shrink.

type fakeInstance struct {
	id, uid       string
	group         *instanceGroup
	pool          string
	member        int // 1-based, across the group's pools in name order
	mac, guid     string
	ethernet, ibA string
	deleted       bool // the record of an instance whose group was deleted
	// powerError is the host's lastPowerActionError, "" for none.
	powerError string
}

// instancesOf derives the hosts a group holds now. The caller holds s.mu.
func (o *objects) instancesOf(g *instanceGroup) []fakeInstance {
	pools := make([]string, 0, len(g.held))
	for p := range g.held {
		pools = append(pools, p)
	}
	sort.Strings(pools)

	// The same octet resolveGroupConsumer grants from, so a host's address
	// here is one a group grant resolves to.
	octet := len(g.id)%200 + 10
	var out []fakeInstance
	member := 0
	for _, pool := range pools {
		for i := range int(g.held[pool]) {
			member++
			uid := uidFor(o.scope, "instance", fmt.Sprintf("%s/%s/%d", g.id, pool, i))
			tail := uid[len(uid)-12:]
			out = append(out, fakeInstance{
				id:       "inst-" + strings.ReplaceAll(uid, "-", ""),
				uid:      uid,
				group:    g,
				pool:     pool,
				member:   member,
				mac:      "02:" + tail[2:4] + ":" + tail[4:6] + ":" + tail[6:8] + ":" + tail[8:10] + ":" + tail[10:12],
				guid:     "0x0002c903" + tail[4:12],
				ethernet: fmt.Sprintf("10.80.%d.%d", octet, member),
				ibA:      fmt.Sprintf("10.81.%d.%d", octet, member),
			})
			last := &out[len(out)-1]
			if _, sent := o.power[last.id]; sent && g.displayName != nil && strings.Contains(*g.displayName, powerFails) {
				last.powerError = powerFailure
			}
		}
	}
	return out
}

// release keeps a deleted record of each instance a deleted group held.
// A record gets an id of its own, since a group of the same id created
// later derives the same ids for its new instances, and the platform's are
// new UUIDs each generation. The caller holds s.mu.
func (o *objects) release(g *instanceGroup) {
	for _, in := range o.instancesOf(g) {
		delete(o.power, in.id)
		in.powerError = ""
		in.uid = uidFor(o.scope, "instance", fmt.Sprintf("%s/record-%d", in.uid, len(o.released)))
		in.id = "inst-" + strings.ReplaceAll(in.uid, "-", "")
		in.deleted = true
		o.released = append(o.released, in)
	}
}

// allInstances is every host in the scope, sorted by id, and with deleted,
// the deleted records too. The caller holds s.mu.
func (o *objects) allInstances(deleted ...bool) []fakeInstance {
	var out []fakeInstance
	for _, g := range instanceGroupKind.under(o, "") {
		out = append(out, o.instancesOf(g)...)
	}
	if len(deleted) > 0 && deleted[0] {
		out = append(out, o.released...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// instanceState is every host's state here. Nothing in the fake stops,
// degrades or loses a host, so the one state it reports is the settled one.
const instanceState = "running"

func (in fakeInstance) state() string {
	if in.deleted {
		return "deleted"
	}
	return instanceState
}

func (in fakeInstance) body(region, project string) map[string]any {
	slug := fmt.Sprintf("srv-%s-%s-%d", in.group.id, in.pool, in.member)
	out := map[string]any{
		"id":                in.id,
		"uid":               in.uid,
		"instanceGroupId":   in.group.id,
		"instanceGroupUuid": in.group.uid,
		"nodePool":          in.pool,
		"serverSlug":        slug,
		"hostname":          slug,
		"state":             in.state(),
		"health":            map[string]any{"state": "healthy"},
		"createdAt":         fixedTime,
		"selfLink":          "/v1/regions/" + region + "/projects/" + project + "/compute/instances/" + in.id,
		"inventory":         map[string]any{},
		"networkConfiguration": []any{
			map[string]any{"type": "ethernet", "macAddress": in.mac, "ipAddresses": []any{in.ethernet}},
			map[string]any{"type": "infiniband", "guid": in.guid, "ipAddresses": []any{in.ibA}},
		},
	}
	if in.deleted {
		out["deletedAt"] = fixedTime
	}
	if in.powerError != "" {
		out["lastPowerActionError"] = in.powerError
	}
	return out
}

func (s *state) listInstances(w http.ResponseWriter, r *http.Request) {
	region, project := r.PathValue("region"), r.PathValue("project")
	q := r.URL.Query()

	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	group, pool := q.Get("instanceGroupId"), q.Get("nodePool")
	// `inStates` repeats, one state each, and a host in any of them matches.
	// Asking for `deleted` turns `showDeleted` on, as the contract says.
	states := q["inStates"]
	showDeleted := q.Get("showDeleted") == "true" || slices.Contains(states, "deleted")
	var matched []fakeInstance
	for _, in := range o.allInstances(showDeleted) {
		if group != "" && in.group.id != group {
			continue
		}
		if pool != "" && in.pool != pool {
			continue
		}
		if len(states) > 0 && !slices.Contains(states, in.state()) {
			continue
		}
		matched = append(matched, in)
	}

	items := make([]any, 0, len(matched))
	for _, in := range matched {
		items = append(items, in.body(region, project))
	}
	instanceList.serve(w, r, items)
}

func (s *state) getInstance(w http.ResponseWriter, r *http.Request) {
	region, project, id := r.PathValue("region"), r.PathValue("project"), r.PathValue("id")

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, in := range s.in(r).allInstances(true) {
		if in.id != id {
			continue
		}
		// A deleted record is a 410 unless asked for, as the contract says.
		if in.deleted && r.URL.Query().Get("showDeleted") != "true" {
			instanceGone(w, id)
			return
		}
		writeJSON(w, http.StatusOK, in.body(region, project))
		return
	}
	apiError(w, http.StatusNotFound, "NOT_FOUND", "not found: instance \""+id+"\"")
}

// instanceGone is the service's answer for an instance that was deleted: an
// id is never reused, so it's a 410 and not a 404.
func instanceGone(w http.ResponseWriter, id string) {
	apiError(w, http.StatusGone, "GONE", "instance \""+id+"\" was deleted")
}

// narrowedAddresses resolves a selector that names one interface by its
// hardware identifier. The contract checks it group-wide: it must match an
// interface somewhere in the group, and a member it does not match contributes
// nothing. The caller holds s.mu.
func (o *objects) narrowedAddresses(g *instanceGroup, n fakeConsumerNetwork) ([]string, bool) {
	var out []string
	for _, in := range o.instancesOf(g) {
		switch {
		case n.Type == "ethernet" && n.MacAddress != nil && strings.EqualFold(*n.MacAddress, in.mac):
			out = append(out, in.ethernet)
		case n.Type == "infiniband" && n.GUID != nil && strings.EqualFold(*n.GUID, in.guid):
			out = append(out, in.ibA)
		}
	}
	return out, len(out) > 0
}

// What the fake follows from executeInstancePowerAction's contract, because
// the provider branches on each:
//
//   - A 202 is "submitted, not settled", and answers the instance as it was
//     when the action was submitted. Nothing in the fake changes the host's
//     state afterwards, as nothing on the platform models machine power yet.
//   - A second action on the same machine inside the guard interval is a 409
//     with `POWER_ACTION_IN_FLIGHT`, the remaining wait in `Retry-After` and
//     in details[0].metadata.retryAfterSeconds.
//   - A failure the platform learns of later is the instance's
//     `lastPowerActionError`. A host whose group's display name carries
//     powerFails reports one once an action has been accepted for it.
//   - A machine with no power control is a permanent 409 with
//     `POWER_ACTION_UNSUPPORTED`. A group whose display name carries
//     powerUnsupported holds only such machines.
//   - An instance that isn't there is the service's 404, and one that was
//     deleted its 410, which the service answers although the contract
//     doesn't list it for this operation.
//
// The guard interval is the service's default. The contract makes it
// configurable and gives none.
const (
	powerGuard       = 10 * time.Minute
	powerFails       = "fake-power-fails"
	powerUnsupported = "fake-power-unsupported"
)

// powerFailure is the lastPowerActionError a powerFails host reports.
// ASSUMPTION: the wording. The contract says only that it explains why.
const powerFailure = "the machine's power controller refused the action"

// powerActions is the contract's single-machine power vocabulary.
var powerActions = []string{"powerOn", "powerOff", "powerCycle", "gracefulShutdown", "gracefulRestart", "forceRestart"}

// powered is the last power action accepted for one host.
type powered struct {
	action string
	at     time.Time
}

// executeInstancePowerAction accepts a power action for one instance.
// ASSUMPTION: an action outside the vocabulary is a 400, as the contract's
// only 4xx for a request it can't read.
func (s *state) executeInstancePowerAction(w http.ResponseWriter, r *http.Request) {
	region, project, id, verb := r.PathValue("region"), r.PathValue("project"), r.PathValue("id"), r.PathValue("action")
	if !slices.Contains(powerActions, verb) {
		apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "action must be one of "+strings.Join(powerActions, ", "))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)
	var found *fakeInstance
	for _, in := range o.allInstances(true) {
		if in.id == id {
			found = &in
			break
		}
	}
	if found == nil {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "not found: instance \""+id+"\"")
		return
	}
	if found.deleted {
		instanceGone(w, id)
		return
	}
	if found.group.displayName != nil && strings.Contains(*found.group.displayName, powerUnsupported) {
		errorEnvelope(w, http.StatusConflict, "", "this instance's server has no power control",
			[]any{map[string]any{"reason": "POWER_ACTION_UNSUPPORTED"}})
		return
	}
	now := time.Now()
	if last, ok := o.power[id]; ok && now.Sub(last.at) < powerGuard {
		wait := int((powerGuard - now.Sub(last.at)).Seconds()) + 1
		w.Header().Set("Retry-After", strconv.Itoa(wait))
		errorEnvelope(w, http.StatusConflict, "", "a power action was already accepted for this instance's server",
			[]any{map[string]any{"reason": "POWER_ACTION_IN_FLIGHT", "metadata": map[string]any{"retryAfterSeconds": wait}}})
		return
	}
	o.power[id] = powered{action: verb, at: now}
	writeJSON(w, http.StatusAccepted, found.body(region, project))
}
