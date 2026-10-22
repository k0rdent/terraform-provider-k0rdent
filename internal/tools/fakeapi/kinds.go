package main

import (
	"fmt"
	"net/http"
	"slices"
	"sort"
)

// The object table: one entry per kind of object the platform stores, and the
// one place that says, for every handler, where a kind's rows live, which
// parent its path nests it under, how an id is made, how a read moves its
// state, how a delete ends it, and which of its states refuse new children.
//
// A handler asks its kind for the row a request names. It gets the row back,
// or it gets false with the platform's answer already written: the parent's
// 404, the row's own 404, the 409 for a parent that takes no new children, or
// the 409 for an id that's taken. What's left in the handler is what's
// particular to the kind: the body it takes, the rules on its fields, and the
// shape it answers in.
//
// The rule from main.go holds here too. A parent that isn't there answers the
// parent's 404, on every route beneath it, because that's what the k0rdent API
// v1.3.0-dev.4 answered for a subnet under a missing network and a peering
// under a missing VPC. A fake that looked the child up on its own would let a
// provider that named the wrong parent read "the child is gone" and drop it
// from state.

var (
	// projectKind is the project every other kind sits in. It lives on the
	// global tier, keyed by id alone, so its rows are the global tier's
	// whatever scope a request names. A request under a project nobody made
	// never reaches it: the gateway answers 403 first, as main.go's
	// authorized does.
	projectKind = &kind[*fakeProject]{
		name:     "project",
		teardown: teardownReads,
		// What a project being deleted does with a create beneath it is each
		// child kind's own declaration: see inDeletingProject.
		rows: func(o *objects) table[*fakeProject] { return o.projects },
		// A project has no transient state on the way in. ProjectStatus
		// declares none and the public create inserts it `active`.
		advance: func(*objects, *fakeProject) bool { return false },
	}

	clusterKind = &kind[*cluster]{
		name:     "cluster",
		idPrefix: "cluster",
		teardown: settleAfter,
		// A test that times out a destroy names the cluster deletesSlowly.
		slowTeardown: slowReads,
		// A test that needs a teardown the platform gives up on names the
		// cluster teardownFails.
		failsTeardown: clusterFailsTeardown,
		// Recorded on v1.3.0-dev.4, and as the contract documents it: no reason.
		inDeletingProject: func(w http.ResponseWriter, project string) {
			recordedConflict(w, fmt.Sprintf("conflict: project %q is being deleted and cannot accept new clusters", project), "")
		},
		rows:      func(o *objects) table[*cluster] { return o.clusters },
		advance:   (*objects).advance,
		gone:      func(o *objects, c *cluster) { delete(o.vpcs, at{id: "vpc-" + c.id}) },
		duplicate: alreadyExists("cluster", ""),
	}

	instanceGroupKind = &kind[*instanceGroup]{
		name:        "instance group",
		idPrefix:    "group",
		detailed404: true,
		teardown:    settleAfter,
		// A test that plans against a group mid-teardown names it
		// deletesSlowly, so the teardown outlasts a plan's reads.
		slowTeardown: slowTeardownReads,
		// A test that needs a teardown the platform gives up on names the
		// group teardownFails.
		failsTeardown: groupFailsTeardown,
		rows:          func(o *objects) table[*instanceGroup] { return o.groups },
		advance:       advanceGroup,
		gone: func(o *objects, g *instanceGroup) {
			delete(o.vpcs, at{id: "vpc-" + g.id})
			o.release(g)
		},
		duplicate: alreadyExists("instance group", ""),
	}

	// A network enters `deleting` on a delete, as v1.3.0-dev.4 showed, and a
	// second delete of one is the 409 a subnet's is. deleteNetwork documents
	// no state it must settle into first, so `deleting` is the only one
	// refused.
	networkKind = &kind[*fakeNetwork]{
		name:               "network",
		idPrefix:           "network",
		teardown:           settleAfter,
		refusesDeleteWhile: []string{"deleting"},
		// createSubnet: "`details[].reason` is `CONFLICT_IN_USE` for other
		// conflicts: ... the parent network is being deleted". A network
		// with no subnets is deleted at once here, so only a network
		// someone else is deleting reaches this.
		children:  childRule{refusedWhile: []string{"deleting"}, refuse: inUse},
		rows:      func(o *objects) table[*fakeNetwork] { return o.networks },
		duplicate: alreadyExists("network", ""),
	}

	// A subnet missing from a network that's there answers as a network
	// does, with no details, as recorded on v1.3.0-dev.4.
	//
	// deleteSubnet: "The subnet enters `state: deleting` while the backend
	// allocation is torn down", and one "still `creating` or already
	// `deleting`" is a CONFLICT_IN_USE 409.
	subnetKind = &kind[*fakeSubnet]{
		name:           "subnet",
		parent:         networkKind,
		parentParam:    "networkId",
		idPrefix:       "subnet",
		teardown:       settleAfter,
		deletableWhile: []string{"active", "failed"},
		rows:           func(o *objects) table[*fakeSubnet] { return o.subnets },
		duplicate:      alreadyExists("subnet", " here"),
	}

	// A VPC has no create and no delete: the cluster and instance-group
	// workflows materialize one and take it away with their owner.
	vpcKind = &kind[*fakeVPC]{
		name: "vpc",
		// createVPCPeering: CONFLICT_IN_USE "when a VPC you can already read
		// is not settled `active`: the local VPC". A binding change leaves it
		// `updating` until it settles.
		children: childRule{onlyWhile: "active", refuse: inUse},
		rows:     func(o *objects) table[*fakeVPC] { return o.vpcs },
	}

	// deleteSecurityGroup: the group "flips to `state: deleting`" and is
	// polled until it 404s. "Only `active` and `failed` are deletable", and
	// "A repeated `DELETE` on a group that is already `deleting` is an
	// idempotent `204`."
	securityGroupKind = &kind[*fakeSecurityGroup]{
		name:            "security group",
		idPrefix:        "sg",
		detailed404:     true,
		teardown:        settleAfter,
		deletableWhile:  []string{"active", "failed"},
		deletingAgainOK: true,
		rows:            func(o *objects) table[*fakeSecurityGroup] { return o.groupsSec },
		duplicate:       alreadyExists("security group", ""),
	}

	// A peering missing from a VPC that's there calls itself a vpc peering
	// and answers with no details, as recorded on v1.3.0-dev.4.
	//
	// deleteVPCPeering: "The peering transitions to `state: deleting` and is
	// removed once the fabric declaration is torn down", and one `creating`
	// or `deleting` is a CONFLICT_IN_USE 409.
	peeringKind = &kind[*fakePeering]{
		name:           "vpc peering",
		parent:         vpcKind,
		parentParam:    "vpcId",
		idPrefix:       "peering",
		teardown:       settleAfter,
		deletableWhile: []string{"active", "failed"},
		rows:           func(o *objects) table[*fakePeering] { return o.peerings },
		duplicate:      alreadyExists("peering", " here"),
	}

	fileStoreKind = &kind[*fakeFileStore]{
		name:        "file store",
		idPrefix:    "fs",
		detailed404: true,
		// Recorded on v1.3.0-dev.4: CONFLICT_IN_USE under details.
		inDeletingProject: func(w http.ResponseWriter, project string) {
			recordedConflict(w, fmt.Sprintf("conflict: project %q is being deleted and cannot accept new file stores", project),
				"CONFLICT_IN_USE")
		},
		// Teardown is asynchronous and the store is polled until it 404s, so
		// the record survives its own DELETE for a few reads. A fake that
		// removed it at once would let a provider with no delete waiter pass.
		teardown: settleAfter + 1,
		rows:     func(o *objects) table[*fakeFileStore] { return o.fileStores },
		advance:  advanceFileStore,
		observe:  observeFileStore,
		// CONFLICT_DUPLICATE, not the ALREADY_EXISTS the compute operations
		// answer with. Supplying an id is how a retried create is made safe
		// here, so this is a success path for the caller and the class it
		// branches on has to be the one the storage operations document.
		duplicate: func(w http.ResponseWriter, id string) {
			storageError(w, http.StatusConflict, "CONFLICT_DUPLICATE",
				"a file store named "+id+" already exists in this project")
		},
	}
)

// webhookKind is a notification webhook subscription. It lives on the global
// tier like a project, keyed by the UUID the platform gave it, and nothing in
// its path names a project or a parent. It has no lifecycle state, so a read
// moves nothing, and a delete takes it away at once: the contract answers 204
// with no teardown to wait out. A subscription that isn't there answers the
// notification service's 404, with no details.
// ASSUMPTION: the wording of that 404. The contract gives only the envelope.
var webhookKind = &kind[*fakeWebhook]{
	name:    "webhook subscription",
	rows:    func(o *objects) table[*fakeWebhook] { return o.webhooks },
	advance: func(*objects, *fakeWebhook) bool { return false },
}

// row is what the table needs from every object: where it keeps its state and
// its reads since the last transition, and the display name a marker in
// main.go can ride in, nil where there's none.
type row interface {
	life() (state *string, reads *int, name *string)
}

// at is where a row sits in its kind's table: under the parent its path names,
// "" for a kind that sits directly in its scope, and by its own id.
type at struct{ parent, id string }

// table is one kind's rows in one scope.
type table[T row] map[at]T

// kind is one entry in the object table. Every field is a declaration. The
// methods below are the only code that reads them.
type kind[T row] struct {
	// name is the kind as the platform's messages spell it: "not found:
	// instance group \"x\"".
	name string
	// parent is the kind the path nests this one under, named by the path
	// value parentParam. nil for a kind that sits directly in the project.
	parent      parentKind
	parentParam string
	// idPrefix makes the id for a create that sent none: <prefix>-<n>.
	idPrefix string
	// detailed404 is whether the platform's 404 repeats itself under
	// `details[]` with the reason NOT_FOUND. Which kinds do is recorded, not
	// reasoned: the same service answers both ways.
	detailed404 bool
	// teardown is the read after an accepted delete that finds the row gone.
	// Zero means the delete removes it at once.
	teardown int
	// slowTeardown, when set, replaces teardown for a row whose display name
	// carries deletesSlowly.
	slowTeardown int
	// failsTeardown, when set, runs as a row's teardown reads finish, and
	// reports whether the teardown failed instead, having moved the row to
	// the state it settles in. nil means every teardown succeeds.
	failsTeardown func(T) bool
	// children is when a row takes a create beneath it, and the 409 it
	// answers when it doesn't.
	children childRule
	// inDeletingProject writes the 409 for a create of this kind in a project
	// being deleted. nil means the create is made. On v1.3.0-dev.4 only the
	// cluster and the file store refused one: a network, subnet, security
	// group and instance group were made, and the group went `active`.
	// ASSUMPTION: a peering is made too. Its VPC needs a cluster, which the
	// project refuses, so none was tried.
	inDeletingProject func(w http.ResponseWriter, project string)
	// deletableWhile are the states admitDelete lets a delete start from,
	// refusing the rest with CONFLICT_IN_USE. nil leaves it to the handler.
	deletableWhile []string
	// refusesDeleteWhile is the other way to say it, for a kind whose
	// contract names the states it refuses rather than those it admits.
	refusesDeleteWhile []string
	// deletingAgainOK answers a delete of a row already `deleting` with the
	// idempotent 204 rather than the 409.
	deletingAgainOK bool

	// rows is where the kind's table is in a scope.
	rows func(*objects) table[T]
	// advance is one read's step for a row that isn't being torn down, and
	// reports whether the read finds it gone. nil means settle.
	advance func(*objects, T) bool
	// observe runs on every read a row survives, after its step.
	observe func(*objects, T)
	// gone runs as a row leaves its table, for whatever goes with it.
	gone func(*objects, T)
	// duplicate is the 409 for a create naming an id that's taken.
	duplicate func(w http.ResponseWriter, id string)
}

// childRule is when a row takes a new child. A row refuses one while it's in
// any of refusedWhile, and, when onlyWhile is set, in any state but that one.
type childRule struct {
	onlyWhile    string
	refusedWhile []string
	// refuse writes the 409, which the kinds don't spell alike.
	refuse func(w http.ResponseWriter, message string)
}

// bareConflict is a 409 with no reason anywhere, as the contract gives the
// project's.
func bareConflict(w http.ResponseWriter, message string) {
	errorEnvelope(w, http.StatusConflict, "", message, nil)
}

// parentKind is what a child kind needs from the kind above it, whatever that
// kind stores.
type parentKind interface {
	notFound(w http.ResponseWriter, id string)
	missingOnCreate(w http.ResponseWriter, id string)
	stateIn(o *objects, id string) (string, bool)
	refuseChild(w http.ResponseWriter, id, state string) bool
}

// notFound writes the platform's 404 for one of this kind's ids.
func (k *kind[T]) notFound(w http.ResponseWriter, id string) {
	k.missing(w, id, k.detailed404)
}

// missingOnCreate is the 404 for a parent a create names that isn't there.
// On v1.3.0-dev.4 it carries NOT_FOUND under `details` for a network and a
// VPC, which their read's 404 doesn't. ASSUMPTION: every parent kind does
// the same, and a project is never one: the gateway answers for it.
func (k *kind[T]) missingOnCreate(w http.ResponseWriter, id string) {
	k.missing(w, id, true)
}

func (k *kind[T]) missing(w http.ResponseWriter, id string, detailed bool) {
	message := fmt.Sprintf("not found: %s %q", k.name, id)
	var details []errorDetail
	if detailed {
		details = []errorDetail{{Reason: "NOT_FOUND", Message: message}}
	}
	writeBody(w, http.StatusNotFound, errorBody{Error: errorFields{
		Code: http.StatusNotFound, Message: message, RequestID: fakeRequestID, Details: details,
	}})
}

// stateIn is the state of a row that sits directly in o, or false when there's
// none. A parent always does.
func (k *kind[T]) stateIn(o *objects, id string) (string, bool) {
	got, ok := k.rows(o)[at{id: id}]
	if !ok {
		return "", false
	}
	state, _, _ := got.life()
	return *state, true
}

// refuseChild writes the 409 for a create beneath a row in state, in the
// kind's own spelling, and reports whether it did. ASSUMPTION: the message.
// No contract gives one.
func (k *kind[T]) refuseChild(w http.ResponseWriter, id, state string) bool {
	c := k.children
	if !slices.Contains(c.refusedWhile, state) && (c.onlyWhile == "" || state == c.onlyWhile) {
		return false
	}
	c.refuse(w, fmt.Sprintf("conflict: %s %q is %s and takes nothing new", k.name, id, state))
	return true
}

// parentOf is the parent a request's path names, or false with the parent's
// 404 written.
func (k *kind[T]) parentOf(w http.ResponseWriter, r *http.Request, o *objects) (string, bool) {
	if k.parent == nil {
		return "", true
	}
	id := r.PathValue(k.parentParam)
	if _, ok := k.parent.stateIn(o, id); !ok {
		k.parent.notFound(w, id)
		return "", false
	}
	return id, true
}

// find is the row a request's path names, and where it sits, or false with the
// 404 written: the parent's when there's no parent, and the row's own when
// there's no row. The caller holds s.mu.
func (k *kind[T]) find(w http.ResponseWriter, r *http.Request, o *objects) (T, at, bool) {
	var none T
	parent, ok := k.parentOf(w, r, o)
	if !ok {
		return none, at{}, false
	}
	where := at{parent, r.PathValue("id")}
	got, ok := k.rows(o)[where]
	if !ok {
		k.notFound(w, where.id)
		return none, at{}, false
	}
	return got, where, true
}

// read is find for a GET, which moves the row a step as the platform's own
// reconciliation would have between two polls. A row whose teardown that step
// finishes leaves its table, and the read answers the 404 the next one would.
func (k *kind[T]) read(w http.ResponseWriter, r *http.Request, o *objects) (T, bool) {
	got, where, ok := k.find(w, r, o)
	if !ok || !k.survives(w, o, where, got) {
		var none T
		return none, false
	}
	return got, true
}

// survives runs one read's step on a row found at where, and reports whether
// it's still there, with the 404 written when it isn't.
func (k *kind[T]) survives(w http.ResponseWriter, o *objects, where at, got T) bool {
	state, reads, name := got.life()
	gone := false
	switch {
	case k.teardown > 0 && *state == "deleting":
		// The contract describes a short-lived tombstone for some kinds and
		// says "clients should treat a subsequent 404 as the canonical end
		// state". The fake goes straight to 404 once the teardown has had its
		// reads, which is the end state a provider has to wait for either way.
		*reads++
		lasts := k.teardown
		if k.slowTeardown > 0 && nameCarries(name, deletesSlowly) {
			lasts = k.slowTeardown
		}
		gone = *reads >= lasts
		if gone && k.failsTeardown != nil && k.failsTeardown(got) {
			gone, *reads = false, 0
		}
	case k.advance != nil:
		gone = k.advance(o, got)
	default:
		settle(state, reads, name)
	}
	if gone {
		k.drop(o, where, got)
		k.notFound(w, where.id)
		return false
	}
	if k.observe != nil {
		k.observe(o, got)
	}
	return true
}

// list is every row under the parent a request's path names, in id order, or
// false with the parent's 404 written.
//
// The list answers the parent's 404, as v1.3.0-dev.4 did for subnets and
// peerings. ASSUMPTION: it does so before it reads its paging parameters. A
// list that answered an empty page under a parent that isn't there would read
// as "this network has no subnets".
func (k *kind[T]) list(w http.ResponseWriter, r *http.Request, o *objects) ([]T, bool) {
	parent, ok := k.parentOf(w, r, o)
	if !ok {
		return nil, false
	}
	return k.under(o, parent), true
}

// under is every row beneath one parent, in id order. Go randomises map
// iteration, and a list whose order changed between two reads would make
// every plan noisy for reasons that have nothing to do with the provider.
func (k *kind[T]) under(o *objects, parent string) []T {
	var ids []string
	rows := k.rows(o)
	for where := range rows {
		if where.parent == parent {
			ids = append(ids, where.id)
		}
	}
	sort.Strings(ids)
	out := make([]T, 0, len(ids))
	for _, id := range ids {
		out = append(out, rows[at{parent, id}])
	}
	return out
}

// admit resolves where a create goes: into a project that exists and takes new
// work, under a parent that does too. It answers each refusal the way the
// platform does, and reports false when it wrote one. The caller holds s.mu.
//
// Through the routes, a project nobody made is the gateway's 403 before this
// runs. The 404 here is what a caller that skips the gateway gets, as the
// contract documents for createCluster and createFileStore.
func (k *kind[T]) admit(s *state, w http.ResponseWriter, r *http.Request) (*objects, string, bool) {
	project := r.PathValue("project")
	state, ok := projectKind.stateIn(s.global(), project)
	if !ok {
		projectKind.notFound(w, project)
		return nil, "", false
	}
	if state == "deleting" && k.inDeletingProject != nil {
		k.inDeletingProject(w, project)
		return nil, "", false
	}
	o := s.in(r)
	if k.parent == nil {
		return o, "", true
	}
	parent := r.PathValue(k.parentParam)
	state, ok = k.parent.stateIn(o, parent)
	if !ok {
		k.parent.missingOnCreate(w, parent)
		return nil, "", false
	}
	if k.parent.refuseChild(w, parent, state) {
		return nil, "", false
	}
	return o, parent, true
}

// claim is the id a create gets, the one it sent or one made for it, or false
// with the duplicate's 409 written when that id is taken under this parent.
// The caller holds s.mu.
func (k *kind[T]) claim(s *state, w http.ResponseWriter, o *objects, parent string, sent *string) (string, bool) {
	id := ""
	if sent != nil && *sent != "" {
		id = *sent
	} else {
		s.seq++
		id = fmt.Sprintf("%s-%d", k.idPrefix, s.seq)
	}
	if _, taken := k.rows(o)[at{parent, id}]; taken {
		k.duplicate(w, id)
		return "", false
	}
	return id, true
}

// put files a new row.
func (k *kind[T]) put(o *objects, parent, id string, v T) {
	k.rows(o)[at{parent, id}] = v
}

// drop takes a row out of its table, and whatever goes with it.
func (k *kind[T]) drop(o *objects, where at, v T) {
	delete(k.rows(o), where)
	if k.gone != nil {
		k.gone(o, v)
	}
}

// admitDelete reports whether a delete may start on a row, and writes the
// answer when it may not: the idempotent 204 for a kind that takes a second
// delete, or CONFLICT_IN_USE for a row in a state its kind doesn't delete
// from.
func (k *kind[T]) admitDelete(w http.ResponseWriter, where at, v T) bool {
	if k.deletableWhile == nil && k.refusesDeleteWhile == nil {
		return true
	}
	state, _, _ := v.life()
	switch {
	case *state == "deleting" && k.deletingAgainOK:
		w.WriteHeader(http.StatusNoContent)
		return false
	case slices.Contains(k.refusesDeleteWhile, *state),
		k.deletableWhile != nil && !slices.Contains(k.deletableWhile, *state):
		// Recorded on v1.3.0-dev.4 for a second delete of a network, a
		// subnet and a peering. ASSUMPTION: a delete of one still `creating`
		// answers the same.
		recordedConflict(w, fmt.Sprintf("conflict: %s %q cannot be deleted in current state", k.name, where.id),
			"CONFLICT_IN_USE")
		return false
	}
	return true
}

// tearDown starts an accepted delete: at once for a kind with no teardown, and
// otherwise `deleting` until reads finish it.
func (k *kind[T]) tearDown(o *objects, where at, v T) {
	if k.teardown == 0 {
		k.drop(o, where, v)
		return
	}
	state, reads, _ := v.life()
	*state, *reads = "deleting", 0
}

// alreadyExists is the compute and networking 409 for a taken id.
func alreadyExists(name, where string) func(http.ResponseWriter, string) {
	article := "a"
	if name[0] == 'i' {
		article = "an"
	}
	return func(w http.ResponseWriter, id string) {
		apiError(w, http.StatusConflict, "ALREADY_EXISTS", article+" "+name+" named "+id+" already exists"+where)
	}
}

// recordedConflict writes a 409 field for field in the platform's order, with
// reason as its one `details[].reason`, or no details when reason is "".
func recordedConflict(w http.ResponseWriter, message, reason string) {
	var details []errorDetail
	if reason != "" {
		details = []errorDetail{{Reason: reason, Message: message}}
	}
	writeBody(w, http.StatusConflict, errorBody{Error: errorFields{
		Code: http.StatusConflict, Message: message, RequestID: fakeRequestID, Details: details,
	}})
}

// The errors the table writes, field for field and in the platform's order,
// so a recorded body compares byte for byte apart from its request id.
type errorBody struct {
	Error errorFields `json:"error"`
}

type errorFields struct {
	Code      int           `json:"code"`
	Message   string        `json:"message"`
	RequestID string        `json:"requestId"`
	Details   []errorDetail `json:"details,omitempty"`
}

type errorDetail struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (c *cluster) life() (*string, *int, *string) { return &c.state, &c.reads, c.displayName }
func (g *instanceGroup) life() (*string, *int, *string) {
	return &g.state, &g.reads, g.displayName
}
func (n *fakeNetwork) life() (*string, *int, *string) { return &n.state, &n.reads, n.displayName }
func (sub *fakeSubnet) life() (*string, *int, *string) {
	return &sub.state, &sub.reads, sub.displayName
}

// A VPC is materialized, not created, so it carries no display name and can
// never be steered anywhere but active.
func (v *fakeVPC) life() (*string, *int, *string) { return &v.state, &v.reads, nil }
func (g *fakeSecurityGroup) life() (*string, *int, *string) {
	return &g.state, &g.reads, g.displayName
}
func (p *fakePeering) life() (*string, *int, *string) { return &p.state, &p.reads, p.displayName }
func (fs *fakeFileStore) life() (*string, *int, *string) {
	return &fs.state, &fs.reads, fs.displayName
}
func (p *fakeProject) life() (*string, *int, *string) {
	return &p.status, &p.readsSinceDelete, nil
}
