package main

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
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
	for _, in := range s.in(r).allInstances() {
		if in.id == id {
			writeJSON(w, http.StatusOK, in.body(region, project))
			return
		}
	}
	apiError(w, http.StatusNotFound, "NOT_FOUND", "not found: instance \""+id+"\"")
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
