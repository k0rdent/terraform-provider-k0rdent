package main

import (
	"fmt"
	"net/http"
	"net/netip"
	"path"
	"sort"
	"strings"
	"time"
)

// File stores and the capability classes they are provisioned from.
//
// Same rule as the rest of the fake: it must not agree with itself. A file
// store is the object in this API whose own schema documents the most ways to
// be refused, and every one of them is a branch on the provider's side that no
// test could otherwise reach. Six are modelled here: an expand-only capacity, an
// immutable class, a conditional etag, an etag-only body, a delete held open by
// listed consumers, and a consumer that fails underneath a store that is fine.
//
// The stores themselves are on `objects`, with the instance groups a consumer
// entry resolves against, and `state.mu` guards both.

// Two ceilings from the store's schema. Capacity is not enforced as a quota on
// the backend and the contract says so twice, which is exactly why the fake
// still refuses a shrink: the ceiling nobody enforces and the floor everybody
// does are different rules, and a fake that dropped both would make the
// provider's expand-only plan modifier untestable.
const (
	maxCapacityGiB = 1024 * 1024 // 1 PiB, the schema maximum
	maxConsumers   = 64
)

// Consumer markers, in the same spirit as the display-name markers in main.go.
// A group whose membership moves is repaired by the platform on its own unless
// repair is off or cannot be vouched for, and nothing a client sends turns
// repair off, so the trigger is a marker in the group's id. With
// consumerDriftsMarker repair is off: the grant reports drift until it is sent
// again. With consumerUnknownHostMarker a member is one the infrastructure
// service does not know: the grant reports drift and sending it again is a 422.
const (
	consumerDriftsMarker      = "fake-drifts"
	consumerUnknownHostMarker = "fake-unknown-host"
)

// A group whose id carries this is reported as no longer grantable: deleted,
// or its id now names a different group. Unlike drift, that is not routine.
const consumerUngrantableMarker = "fake-ungrantable"

// A store whose display name carries this fails its next expansion while
// staying `active`, which the contract says a waiter must treat as terminal.
const expansionFailsMarker = "fake-expansion-fails"

// storageError writes the same envelope as apiError with the failure class in
// `details[].reason` rather than at the top level. Every 409 and 422 the storage
// operations document puts it there, the client reads both places, and a fake
// that put it wherever was easiest would let a provider that only ever looks at
// the top level pass.
func storageError(w http.ResponseWriter, status int, reason, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":      status,
			"message":   message,
			"requestId": fakeRequestID,
			"details":   []any{map[string]any{"reason": reason}},
		},
	})
}

type fakeShareType struct {
	id             string
	displayName    string
	capabilities   map[string]string
	maxCapacityGiB int64 // 0 means the class sets no ceiling
}

// The class catalog: capability-named ids and capability-only values. No vendor
// name, model number or array identifier appears here or can be inferred from
// here, which is the one thing the schema says about this map that a client
// could get wrong.
//
// Three entries against a page size of two, for the reason the address-pool
// catalog has three: a catalog that arrived whole would let a provider that
// reads the first page and stops pass, and that provider silently loses classes
// on any region with more than a page of them.
var shareTypes = []fakeShareType{
	{
		id: "general", displayName: "General purpose",
		capabilities: map[string]string{"protocol": "nfs", "tier": "capacity", "redundancy": "replicated"},
	},
	{
		id: "high-speed", displayName: "High speed",
		capabilities:   map[string]string{"protocol": "nfs", "tier": "performance", "redundancy": "replicated"},
		maxCapacityGiB: 8192,
	},
	{
		id: "archive", displayName: "Archive",
		capabilities: map[string]string{"protocol": "nfs", "tier": "archive", "redundancy": "erasure-coded"},
	},
}

func findShareType(id string) (fakeShareType, bool) {
	for _, st := range shareTypes {
		if st.id == id {
			return st, true
		}
	}
	return fakeShareType{}, false
}

func shareTypeJSON(region string, st fakeShareType) map[string]any {
	out := map[string]any{
		"id":           st.id,
		"displayName":  st.displayName,
		"capabilities": st.capabilities,
		"selfLink":     "/v1/regions/" + region + "/storage/share-types/" + st.id,
		"state":        "active",
		"reconciling":  false,
		"etag":         "rev-1",
		"createdAt":    fixedTime,
		"updatedAt":    fixedTime,
	}
	if st.maxCapacityGiB > 0 {
		out["maxCapacityGiB"] = st.maxCapacityGiB
	}
	return out
}

func (s *state) listShareTypes(w http.ResponseWriter, r *http.Request) {
	region := r.PathValue("region")
	items := make([]any, 0, len(shareTypes))
	for _, st := range shareTypes {
		items = append(items, shareTypeJSON(region, st))
	}
	shareTypeList.serve(w, r, items)
}

func (s *state) getShareType(w http.ResponseWriter, r *http.Request) {
	st, ok := findShareType(r.PathValue("id"))
	if !ok {
		apiError(w, http.StatusNotFound, "NOT_FOUND", "no such share type")
		return
	}
	writeJSON(w, http.StatusOK, shareTypeJSON(r.PathValue("region"), st))
}

type fakeConsumerNetwork struct {
	Type       string  `json:"type"`
	MacAddress *string `json:"macAddress,omitempty"`
	GUID       *string `json:"guid,omitempty"`
}

type fakeConsumer struct {
	Ref         string               `json:"ref"`
	Type        *string              `json:"type,omitempty"`
	AccessLevel *string              `json:"accessLevel,omitempty"`
	MountPath   *string              `json:"mountPath,omitempty"`
	Network     *fakeConsumerNetwork `json:"network,omitempty"`
}

// kind is the entry's type with the documented default applied. The default is
// applied here and never written back into the entry: the schema says a body
// with no `type` is read exactly as it was written, so echoing `ipAddress` into
// a response the client never sent it in would put a permanent diff in front of
// any provider that records what it reads.
func (c fakeConsumer) kind() string {
	if c.Type == nil {
		return "ipAddress"
	}
	return *c.Type
}

type fakeConsumerStatus struct {
	ref       string
	addresses []string
	state     string
	target    string // where this entry settles once it has been read enough times
	reason    string // FileStoreConsumerStateReason token, on a settled failure
	reads     int
	// narrowed is a group grant whose selector names one interface by its
	// hardware identifier, so its addresses are that interface's and not one
	// per member.
	narrowed bool
}

type fakeFileStore struct {
	id          string
	uid         string
	shareType   string
	capacityGiB int64
	displayName *string
	description *string
	consumers   []fakeConsumer
	statuses    []*fakeConsumerStatus // a superset of consumers, keyed by ref
	rev         int
	seq         int // creation order, for the newest-first list
	createdAt   time.Time
	updatedAt   time.Time
	state       string
	reads       int

	// An accepted expansion the backend has not caught up with, and the reads
	// it has been polled for. A failed one stays `active` with a reason.
	expanding    bool
	expandReads  int
	expandFailed bool

	// refusedBusy says an update was refused while the store was `creating`.
	// See busyUntilRefused.
	refusedBusy bool
}

// advanceFileStore moves a store a step through its transient state, as
// settle does for every kind, with two differences. A store named for
// busyUntilRefused stays `creating` until an update has been refused. And a
// store that settles from `creating` takes a new etag.
//
// ASSUMPTION: the contract calls the etag an "opaque concurrency token for the
// object's current revision", and says an update with an old one is refused
// "if the store has been modified since you read it". It doesn't say whether
// the platform settling a store is a modification, so either answer fits it.
// The fake takes the stricter one, for every store and not only one carrying
// a marker, for two reasons. A client can't tell the two apart from outside,
// so it has to be correct under both, and only this one tests that: an
// update that carries the etag read while the store was `creating` is refused
// with CONFLICT_VERSION once it settles, so a provider that waits out the
// CONFLICT_IN_USE has to read the etag again. And a move that only marked
// stores made would leave every other test running against the easier
// reading. Every other path in the provider reads a store after it settles,
// so none of them depends on which reading holds.
func advanceFileStore(_ *objects, fs *fakeFileStore) bool {
	if holdsUntilRefused(fs.state, fs.displayName, fs.refusedBusy) {
		return false
	}
	was := fs.state
	settle(&fs.state, &fs.reads, fs.displayName)
	if was == "creating" && fs.state != was {
		fs.rev++
	}
	return false
}

func (fs *fakeFileStore) etag() string { return fmt.Sprintf("rev-%d", fs.rev) }

// reconciling is deliberately still true on a `failed` store.
//
// The schema is explicit that this is not an oversight: the desired state
// genuinely was never delivered, and reporting `false` there would hide a failed
// expansion behind a green object. It is the whole reason a waiter must break on
// `state` and never on this flag, so the fake has to reproduce it or that
// reasoning is unverifiable.
//
// A consumer reported as drifted does NOT hold it open. Drift means the grant is
// intact and still enforced, and a store whose flag never cleared after a
// routine instance-group scale-up would hang every caller that waits on it.
func (fs *fakeFileStore) reconciling() bool {
	if fs.state != "active" || fs.expanding || fs.expandFailed {
		return true
	}
	return fs.applying()
}

// applying reports a grant still in flight. Unlike reconciling it ignores a
// failed expansion, which no amount of waiting clears.
func (fs *fakeFileStore) applying() bool {
	for _, cs := range fs.statuses {
		if cs.state == "creating" || cs.state == "deleting" {
			return true
		}
	}
	return false
}

func (s *state) listFileStores(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	if want := query.Get("state"); want != "" {
		switch want {
		case "creating", "active", "deleting", "failed":
		default:
			// Refused rather than answered with an empty page, so a typo
			// cannot be mistaken for "no stores exist".
			storageError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
				"state must be creating, active, deleting or failed, got "+want)
			return
		}
	}
	if want := query.Get("shareType"); want != "" {
		if _, ok := findShareType(want); !ok {
			storageError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
				"no share type named "+want+" in this region")
			return
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	stores, _ := fileStoreKind.list(w, r, s.in(r))

	matched := []*fakeFileStore{}
	for _, fs := range stores {
		if want := query.Get("state"); want != "" && fs.state != want {
			continue
		}
		if want := query.Get("shareType"); want != "" && fs.shareType != want {
			continue
		}
		// Matched against the entry `ref` only, never against the addresses a
		// group resolved to. The two are different questions, and a fake that
		// searched the resolved addresses as well would answer the one it was
		// not asked.
		if want := query.Get("consumer"); want != "" && !holdsConsumer(fs, want) {
			continue
		}
		matched = append(matched, fs)
	}
	// Newest first, as the operation documents. The ids sort alphabetically
	// above only so the comparison below has a stable starting order.
	sort.SliceStable(matched, func(i, j int) bool { return matched[i].seq > matched[j].seq })

	region, project := r.PathValue("region"), r.PathValue("project")
	items := make([]any, 0, len(matched))
	for _, fs := range matched {
		items = append(items, fileStoreJSON(region, project, fs))
	}
	fileStoreList.serve(w, r, items)
}

func holdsConsumer(fs *fakeFileStore, ref string) bool {
	for _, c := range fs.consumers {
		if c.Ref == ref || c.Ref == normalizeAddress(ref) {
			return true
		}
	}
	return false
}

func (s *state) createFileStore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID          *string         `json:"id"`
		ShareType   string          `json:"shareType"`
		CapacityGiB int64           `json:"capacityGiB"`
		DisplayName *string         `json:"displayName"`
		Description *string         `json:"description"`
		Consumers   *[]fakeConsumer `json:"consumers"`
	}
	if !decode(w, r, &body) {
		return
	}
	// Server-owned fields are refused rather than ignored, which the struct
	// above gets for free from decode's DisallowUnknownFields: a provider that
	// posted back a store it had read learns so here.
	class, ok := findShareType(body.ShareType)
	if !ok {
		storageError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			"no share type named "+body.ShareType+" in this region")
		return
	}
	if body.CapacityGiB <= 0 || body.CapacityGiB > maxCapacityGiB {
		storageError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			fmt.Sprintf("capacityGiB must be between 1 and %d, got %d", maxCapacityGiB, body.CapacityGiB))
		return
	}
	if class.maxCapacityGiB > 0 && body.CapacityGiB > class.maxCapacityGiB {
		storageError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
			fmt.Sprintf("capacityGiB %d exceeds the %s class's maxCapacityGiB of %d",
				body.CapacityGiB, class.id, class.maxCapacityGiB))
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o, _, ok := fileStoreKind.admit(s, w, r)
	if !ok {
		return
	}
	id, ok := fileStoreKind.claim(s, w, o, "", body.ID)
	if !ok {
		return
	}

	consumers, statuses := []fakeConsumer{}, []*fakeConsumerStatus{}
	if body.Consumers != nil {
		var refusal string
		consumers, statuses, refusal = o.resolveConsumers(*body.Consumers)
		if refusal != "" {
			// Every entry is resolved before the store is accepted, so an
			// unresolvable one fails the whole create rather than producing a
			// store with a partial export policy.
			storageError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", refusal)
			return
		}
	}

	s.seq++
	now := time.Now().UTC()
	fs := &fakeFileStore{
		id: id, uid: uidFor(o.scope, "file_store", id), shareType: body.ShareType, capacityGiB: body.CapacityGiB,
		displayName: body.DisplayName, description: body.Description,
		consumers: consumers, statuses: statuses,
		rev: 1, seq: s.seq, createdAt: now, updatedAt: now,
		state: "creating",
	}
	fileStoreKind.put(o, "", id, fs)

	writeJSON(w, http.StatusCreated, fileStoreJSON(r.PathValue("region"), r.PathValue("project"), fs))
}

func (s *state) getFileStore(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fs, ok := fileStoreKind.read(w, r, s.in(r)); ok {
		writeJSON(w, http.StatusOK, fileStoreJSON(r.PathValue("region"), r.PathValue("project"), fs))
	}
}

// observeFileStore is what a read of a store moves besides the store's own
// state, on every read the store survives, teardown included: its grants, the
// platform's freshness sweep of them, and an expansion the backend is catching
// up with.
func observeFileStore(o *objects, fs *fakeFileStore) {
	advanceConsumerStatuses(fs)
	o.sweepGroupGrants(fs)
	if fs.expanding {
		fs.expandReads++
		if fs.expandReads > settleAfter+2 {
			fs.expanding = false
			fs.expandFailed = nameCarries(fs.displayName, expansionFailsMarker)
		}
	}
}

func (s *state) updateFileStore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DisplayName *string         `json:"displayName"`
		Description *string         `json:"description"`
		CapacityGiB *int64          `json:"capacityGiB"`
		Consumers   *[]fakeConsumer `json:"consumers"`
		Etag        *string         `json:"etag"`
		// Declared so that sending it is diagnosed rather than rejected as an
		// unknown field. The schema carries it for the same reason.
		ShareType *string `json:"shareType"`
	}
	if !decode(w, r, &body) {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	fs, _, ok := fileStoreKind.find(w, r, o)
	if !ok {
		return
	}

	// Shape first, then state, then the conditional, then the values. A caller
	// that sent an immutable field learns that whatever else is wrong with the
	// body, rather than being told about the etag on one run and the class on
	// the next.
	if body.ShareType != nil {
		storageError(w, http.StatusConflict, "CONFLICT_IMMUTABLE",
			"shareType is fixed for the life of the store; moving between classes is a data migration")
		return
	}
	if body.DisplayName == nil && body.Description == nil && body.CapacityGiB == nil && body.Consumers == nil {
		storageError(w, http.StatusBadRequest, "INVALID_ARGUMENT",
			"an etag-only body is not an update; send displayName, description, capacityGiB or consumers")
		return
	}
	if fs.state != "active" {
		fs.refusedBusy = fs.refusedBusy || fs.state == "creating"
		storageError(w, http.StatusConflict, "CONFLICT_IN_USE",
			"the store is "+fs.state+" and does not accept updates")
		return
	}
	// The etag rides in the body, not in an If-Match header. Modelled as the
	// contract declares it because a provider that reached for the header would
	// be silently unconditional here, and would clobber a concurrent edit on a
	// real server while passing every test against a fake that ignored both.
	if body.Etag != nil && *body.Etag != fs.etag() {
		storageError(w, http.StatusConflict, "CONFLICT_VERSION",
			"the store has changed since it was read; re-read it and retry")
		return
	}
	if body.CapacityGiB != nil {
		if *body.CapacityGiB < fs.capacityGiB {
			storageError(w, http.StatusConflict, "CONFLICT_IMMUTABLE",
				fmt.Sprintf("capacity is expand-only: %d is below the store's current %d",
					*body.CapacityGiB, fs.capacityGiB))
			return
		}
		if *body.CapacityGiB > maxCapacityGiB {
			storageError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
				fmt.Sprintf("capacityGiB must be at most %d, got %d", maxCapacityGiB, *body.CapacityGiB))
			return
		}
		if class, ok := findShareType(fs.shareType); ok && class.maxCapacityGiB > 0 &&
			*body.CapacityGiB > class.maxCapacityGiB {
			storageError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
				fmt.Sprintf("capacityGiB %d exceeds the %s class's maxCapacityGiB of %d",
					*body.CapacityGiB, class.id, class.maxCapacityGiB))
			return
		}
	}

	if body.Consumers != nil {
		for _, c := range *body.Consumers {
			for _, cs := range fs.statuses {
				if cs.ref == c.Ref && cs.state == "failed" && strings.Contains(c.Ref, consumerUnknownHostMarker) {
					storageError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
						"instance group "+c.Ref+" has member host(s) the infrastructure service does not know (srv-ghost)")
					return
				}
			}
		}
	}
	var consumers []fakeConsumer
	var statuses []*fakeConsumerStatus
	if body.Consumers != nil {
		var refusal string
		consumers, statuses, refusal = o.resolveConsumers(*body.Consumers)
		if refusal != "" {
			storageError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", refusal)
			return
		}
	}

	// "Send an empty string to clear it."
	fs.displayName = patchString(fs.displayName, body.DisplayName)
	fs.description = patchString(fs.description, body.Description)
	if body.CapacityGiB != nil {
		// No transitional state: the declared capacity changes as soon as the
		// request is accepted and the store stays `active` while the backing
		// storage catches up.
		if *body.CapacityGiB > fs.capacityGiB {
			fs.expanding, fs.expandReads, fs.expandFailed = true, 0, false
		}
		fs.capacityGiB = *body.CapacityGiB
	}
	if body.Consumers != nil {
		// Replaced wholesale. It is desired state and not a patch, which is the
		// reading that costs a caller an export policy: a provider that sent
		// only the entry it had added would detach every other grant here,
		// exactly as it would upstream.
		fs.consumers = consumers
		fs.statuses = mergeConsumerStatuses(fs.statuses, statuses)
	}
	fs.rev++
	fs.updatedAt = time.Now().UTC()

	writeJSON(w, http.StatusOK, fileStoreJSON(r.PathValue("region"), r.PathValue("project"), fs))
}

func (s *state) deleteFileStore(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "true"

	s.mu.Lock()
	defer s.mu.Unlock()
	o := s.in(r)

	fs, where, ok := fileStoreKind.find(w, r, o)
	if !ok {
		return
	}

	// `creating` refuses both kinds of delete. A purge must not race an
	// in-flight create either, which is the one place `force` does not get the
	// caller past a 409 and the reason it cannot be the provider's blanket
	// answer to a stuck destroy.
	if fs.state == "creating" {
		storageError(w, http.StatusConflict, "CONFLICT_IN_USE",
			"the store is still creating; poll until it is active or failed, then delete")
		return
	}
	if force {
		fileStoreKind.drop(o, where, fs)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if fs.state == "deleting" {
		// Not a conflict: a repeated plain DELETE on a store already tearing
		// down is the idempotent 204.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	// The consumer check is on `active` alone, as documented. A failed store's
	// grants were never enforced, so there is nothing for teardown to revoke and
	// nothing to hold the delete open.
	if fs.state == "active" && len(fs.consumers) > 0 {
		storageError(w, http.StatusConflict, "CONFLICT_IN_USE",
			fmt.Sprintf("the store still lists %d consumer(s); clear them with a PATCH first, or delete with force=true",
				len(fs.consumers)))
		return
	}
	// "A store is also refused while a change to it is still being applied:
	// wait until `reconciling` is false and retry." A failed expansion keeps
	// `reconciling` true and is not being applied, and the contract does not
	// say which rule wins, so here it does not hold the delete.
	if fs.state == "active" && (fs.expanding || fs.applying()) {
		storageError(w, http.StatusConflict, "CONFLICT_IN_USE",
			"a change to the store is still being applied; wait until reconciling is false and retry")
		return
	}

	fileStoreKind.tearDown(o, where, fs)
	for _, cs := range fs.statuses {
		cs.state, cs.reads = "deleting", 0
	}
	w.WriteHeader(http.StatusNoContent)
}

// resolveConsumers checks a desired list and works out what each entry grants.
// It returns the normalized entries, one status per entry, and the refusal
// message when the list cannot be accepted.
func (o *objects) resolveConsumers(list []fakeConsumer) ([]fakeConsumer, []*fakeConsumerStatus, string) {
	if len(list) > maxConsumers {
		return nil, nil, fmt.Sprintf("a store holds at most %d consumers, got %d", maxConsumers, len(list))
	}

	consumers := make([]fakeConsumer, 0, len(list))
	statuses := make([]*fakeConsumerStatus, 0, len(list))
	seen := map[string]bool{}

	for _, entry := range list {
		if entry.AccessLevel != nil {
			// `ro` is in the enum and cannot be granted. Refusing it is the
			// point: accepting it would report a restriction no backend
			// applies and leave the mount read-write.
			if *entry.AccessLevel != "rw" {
				return nil, nil, "accessLevel " + *entry.AccessLevel + " cannot be granted; only rw can"
			}
		}
		if entry.MountPath != nil && !cleanAbsolutePath(*entry.MountPath) {
			return nil, nil, "mountPath must be a clean absolute POSIX path, got " + *entry.MountPath
		}

		status := &fakeConsumerStatus{state: "creating", target: "active"}
		switch entry.kind() {
		case "ipAddress":
			if entry.Network != nil {
				return nil, nil, "network is not valid on an ipAddress entry; ref is already the address"
			}
			address := normalizeAddress(entry.Ref)
			if address == "" {
				return nil, nil, "ref " + entry.Ref + " is not a routable literal IPv4 host address"
			}
			// Normalized on write, so two spellings of one address compare
			// equal here and in the duplicate check below.
			entry.Ref = address
			// The identity resolution. It is not a special case bolted on: it
			// is the substrate the group kind resolves down to.
			status.addresses = []string{address}
		case "instanceGroup":
			refusal := o.resolveGroupConsumer(entry, status)
			if refusal != "" {
				return nil, nil, refusal
			}
		default:
			return nil, nil, "type must be ipAddress or instanceGroup, got " + entry.kind()
		}

		if seen[entry.Ref] {
			return nil, nil, "two consumers normalize to the same ref: " + entry.Ref
		}
		seen[entry.Ref] = true
		status.ref = entry.Ref
		consumers = append(consumers, entry)
		statuses = append(statuses, status)
	}
	return consumers, statuses, ""
}

// resolveGroupConsumer resolves one instance-group entry to the addresses it
// grants, and fills in where its status settles.
func (o *objects) resolveGroupConsumer(entry fakeConsumer, status *fakeConsumerStatus) string {
	if strings.Contains(entry.Ref, "/") {
		return "an instanceGroup ref is a bare id, not a URI: " + entry.Ref
	}
	// The operator names the fabric and the platform never infers it, so an
	// entry that omits it is refused rather than having an interface chosen on
	// its behalf.
	if entry.Network == nil {
		return "an instanceGroup entry must name the network its grant is mounted over"
	}
	switch entry.Network.Type {
	case "ethernet":
		if entry.Network.GUID != nil {
			return "guid goes with type infiniband, not ethernet"
		}
	case "infiniband":
		if entry.Network.MacAddress != nil {
			return "macAddress goes with type ethernet, not infiniband"
		}
	default:
		return "network.type must be ethernet or infiniband, got " + entry.Network.Type
	}

	group, ok := o.groups[at{id: entry.Ref}]
	if !ok {
		// A 422 and never a 404: the addressed resource is the store, and the
		// store was found.
		return "no instance group named " + entry.Ref + " in this project"
	}
	members := 0
	for _, size := range group.pools {
		members += int(size)
	}
	if members == 0 {
		return "instance group " + entry.Ref + " has no members to grant"
	}

	// A selector narrowed by a MAC address or a GUID grants only the interface
	// it names, and one that names no interface in the group is a 422.
	if entry.Network.MacAddress != nil || entry.Network.GUID != nil {
		addresses, ok := o.narrowedAddresses(group, *entry.Network)
		if !ok {
			return "the selector on " + entry.Ref + " matches no interface on any member of the group"
		}
		status.addresses, status.narrowed = addresses, true
		return ""
	}

	// One address per member machine on the selected interface, which is what
	// makes `resolvedAddresses` the record of which machines the grant was
	// derived from rather than a restatement of the ref.
	octet := len(entry.Ref)%200 + 10
	for i := 1; i <= members; i++ {
		status.addresses = append(status.addresses, fmt.Sprintf("10.80.%d.%d", octet, i))
	}

	if strings.Contains(entry.Ref, consumerUngrantableMarker) {
		status.target, status.reason = "failed", "GROUP_NO_LONGER_GRANTABLE"
	}
	return ""
}

// sweepGroupGrants compares each settled group grant with the group's current
// membership, as the platform's freshness sweep does.
func (o *objects) sweepGroupGrants(fs *fakeFileStore) {
	if fs.state != "active" {
		return
	}
	for _, cs := range fs.statuses {
		group, ok := o.groups[at{id: cs.ref}]
		if !ok || cs.state != "active" || cs.narrowed {
			continue
		}
		members := 0
		for _, size := range group.pools {
			members += int(size)
		}
		if members == len(cs.addresses) {
			continue
		}
		if strings.Contains(cs.ref, consumerDriftsMarker) || strings.Contains(cs.ref, consumerUnknownHostMarker) {
			// The grant is intact and still enforced for the old members.
			cs.state, cs.target, cs.reason, cs.reads = "failed", "failed", "GROUP_MEMBERSHIP_DRIFTED", 0
			continue
		}
		// Repaired on the platform's own initiative: `creating` with no reason
		// while it is under way.
		octet := len(cs.ref)%200 + 10
		cs.addresses = cs.addresses[:0]
		for i := 1; i <= members; i++ {
			cs.addresses = append(cs.addresses, fmt.Sprintf("10.80.%d.%d", octet, i))
		}
		cs.state, cs.target, cs.reason, cs.reads = "creating", "active", "", 0
	}
}

// mergeConsumerStatuses keeps the entries a replaced list no longer names.
//
// `consumerStatus` is a superset of `consumers`: a revocation leaves the desired
// list at once and stays here as `deleting` until the backend confirms it. A
// fake that dropped it with the entry would let a provider treat a removal as
// instantaneous, and the grant is not gone until this says so.
func mergeConsumerStatuses(old, next []*fakeConsumerStatus) []*fakeConsumerStatus {
	wanted := map[string]bool{}
	for _, cs := range next {
		wanted[cs.ref] = true
	}

	merged := make([]*fakeConsumerStatus, 0, len(next)+len(old))
	merged = append(merged, next...)
	for _, cs := range old {
		if wanted[cs.ref] {
			continue
		}
		cs.state, cs.reads = "deleting", 0
		merged = append(merged, cs)
	}
	return merged
}

// advanceConsumerStatuses settles each entry two reads after `settle` would
// settle an object, and drops a revocation once it has been reported. The lag
// is the contract's case of an `active` store still applying a grant.
func advanceConsumerStatuses(fs *fakeFileStore) {
	kept := fs.statuses[:0]
	for _, cs := range fs.statuses {
		cs.reads++
		if cs.reads > settleAfter+2 {
			if cs.state == "deleting" {
				continue
			}
			if cs.state != cs.target {
				cs.state, cs.reads = cs.target, 0
			}
		}
		kept = append(kept, cs)
	}
	fs.statuses = kept
}

// normalizeAddress returns the canonical spelling of a grantable IPv4 host
// address, or "" for anything that cannot name one host that could mount the
// share. `0.0.0.0` is refused along with the rest because it is the
// conventional NFS export wildcard, and accepting it would turn a per-host
// grant into a world-writable export this API would then report as enforced.
func normalizeAddress(ref string) string {
	ip, err := netip.ParseAddr(ref)
	// "A single literal IPv4 address": an IPv4-mapped IPv6 spelling is not one.
	if err != nil || !ip.Is4() {
		return ""
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() || ip.IsLinkLocalUnicast() ||
		ip == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return ""
	}
	return ip.String()
}

func cleanAbsolutePath(p string) bool {
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return false
	}
	return !strings.ContainsFunc(p, func(r rune) bool { return r < 0x20 || r == 0x7f })
}

func fileStoreJSON(region, project string, fs *fakeFileStore) map[string]any {
	base := "/v1/regions/" + region + "/projects/" + project + "/storage/file-stores/"

	out := map[string]any{
		"id":          fs.id,
		"shareType":   fs.shareType,
		"capacityGiB": fs.capacityGiB,
		"state":       fs.state,
		"reconciling": fs.reconciling(),
		"etag":        fs.etag(),
		"selfLink":    base + fs.id,
		"uid":         fs.uid,
		"createdBy":   wantClientID,
		"createdAt":   fs.createdAt.Format(time.RFC3339),
		"updatedAt":   fs.updatedAt.Format(time.RFC3339),
	}
	// Not in the schema's required set, and an empty list is left out rather
	// than sent as [].
	if len(fs.consumers) > 0 {
		out["consumers"] = fs.consumers
	}
	// `usage` is absent on every store, deliberately: no backend publishes a
	// per-store consumption figure the platform can sample, and a provider that
	// waited for one would wait forever.
	if fs.displayName != nil {
		out["displayName"] = *fs.displayName
	}
	if fs.description != nil {
		out["description"] = *fs.description
	}
	if fs.state == "failed" {
		// Not the shared failureDetail: the storage vocabulary is disjoint from
		// the one the networking objects report, so the token that fits a
		// fabric rejection is not a value this field can take.
		out["stateReason"] = "STORAGE_FAILED"
		out["stateMessage"] = "storage_failed (ref: fake-execution-1)"
		out["statusReason"] = "storage_failed (ref: fake-execution-1)"
	}
	if fs.state == "active" && fs.expandFailed {
		out["stateReason"] = "STORAGE_ERROR"
		out["stateMessage"] = "storage_error (ref: fake-execution-2)"
		out["statusReason"] = "storage_error (ref: fake-execution-2)"
	}
	// Mount targets are published by the time the store is active and never
	// while it is creating or failed. A provider that recorded them from the
	// create response would record nothing and never correct it.
	if fs.state == "active" || fs.state == "deleting" {
		out["exportLocations"] = []any{
			map[string]any{
				"protocol": "nfs", "server": "10.96.0.20", "path": "/exports/" + fs.id,
				"options": "vers=4.1,hard", "preferred": true,
			},
			map[string]any{
				"protocol": "nfs", "server": "10.97.0.20", "path": "/exports/" + fs.id,
				"adminOnly": true,
			},
		}
	}

	statuses := make([]any, 0, len(fs.statuses))
	for _, cs := range fs.statuses {
		entry := map[string]any{
			"ref":               cs.ref,
			"state":             cs.state,
			"resolvedAddresses": cs.addresses,
		}
		if cs.state == "failed" && cs.reason != "" {
			entry["stateReason"] = cs.reason
			entry["stateMessage"] = "(ref: fake-execution-1)"
			entry["statusReason"] = strings.ToLower(cs.reason) + " (ref: fake-execution-1)"
		}
		statuses = append(statuses, entry)
	}
	out["consumerStatus"] = statuses

	return out
}
