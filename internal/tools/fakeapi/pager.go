package main

import (
	"math"
	"net/http"
	"net/url"
	"strconv"
)

// The pager: one way every list in the fake pages, and one declaration per
// list of how its route does it. A handler builds the rows it would answer
// with, all of them and in order, and hands them to its route's pager. The
// pager reads the query and answers the page.
//
// The lists don't page alike, and that's on purpose. Each declaration below
// follows its operation's contract, or the real handler where the two were
// measured to differ, and a fake that made them agree would let a provider
// that pages one route like another pass. What they share is the rule from
// main.go: a list with more rows than a page has its later rows only behind
// the cursor or the offset, so a provider that reads the first page and stops
// fails here.

var (
	// The networking lists page on a small maximum, as the real server pages
	// on its own, so a provider that read only the first page fails an
	// acceptance test with a handful of objects in it.
	networkList       = pager{key: "networks", offset: offsetRule{offsetContract, refuseBare}, cursor: &networkingCursor}
	subnetList        = pager{key: "subnets", offset: offsetRule{offsetContract, refuseBare}, cursor: &networkingCursor}
	securityGroupList = pager{key: "securityGroups", offset: offsetRule{offsetContract, refuseWithReason}, cursor: &networkingCursor}
	peeringList       = pager{key: "peerings", cursor: &networkingCursor}

	// Paged as the contract's PageSize and PageToken describe, 50 by default.
	// ASSUMPTION: the token is opaque, so an offset is as good as anything,
	// and one this server didn't issue starts the list again.
	vpcList = pager{key: "vpcs", offset: offsetRule{offsetContract, refuseBare},
		cursor: &cursorRule{page: 50, size: sizeLenient}}

	// The deprecated cursor path's page is two, whatever `pageSize` asks for.
	// No provider caller takes that path now: it's kept for a bare request,
	// which the real server still answers on the cursor.
	addressPoolList = pager{key: "addressPools", offset: offsetRule{offsetContract, refuseWithReason},
		cursor: &cursorRule{page: 2, cap: 2, badToken: tokenRefused400}}

	// One per page. ASSUMPTION, same as the address pool catalog: nothing in
	// the contract says a small catalog arrives whole. Two cluster types and a
	// page of one means the second is reachable only by following the token.
	clusterTypeList = pager{key: "clusterTypes", cursor: &cursorRule{page: 1, cap: 1, badToken: tokenRefused400}}

	// Cursor, not offset: the shared parameters are marked deprecated in favour
	// of `offset`/`limit`, but neither of these operations documents those
	// yet, and the deprecation note says to consult the operation rather than
	// assume. A fake that answered to `offset` would invent a migration that
	// hasn't happened. Three classes against a page of two, for the reason
	// the address-pool catalog has more than a page.
	shareTypeList = pager{key: "shareTypes", cursor: &storageCursor}
	// The stores a project holds page on the same terms, and for the same
	// reason: a default page big enough to hold every store a test creates
	// would never issue a cursor.
	fileStoreList = pager{key: "fileStores", cursor: &storageCursor}

	// One per page, as the other cursor catalogs are. `pageSize` is capped at
	// 200 and a larger one is a 422, not a clamp, and the operation documents
	// no offset.
	instanceList = pager{key: "instances",
		offset: offsetRule{offsetRefused, func(w http.ResponseWriter) {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
				"listInstances does not document offset paging")
		}},
		cursor: &cursorRule{page: 1, cap: 1, size: sizeChecked, maxSize: 200,
			badSize: func(w http.ResponseWriter, _ string) {
				apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
					"validation: pageSize must be between 0 and 200 (0 applies the default of 50)")
			},
			badToken: tokenRefused400}}

	// One per page, and for the same reason: these are the operations that
	// have migrated to offset paging, so a fake that returned everything at
	// once would let a provider that reads the first page and stops pass. The
	// organizations list refuses a request that mixes in the cursor.
	projectList = pager{key: "projects", offset: offsetRule{style: offsetOneRow}, count: "totalCount"}
	// listProjectsInOrg documents the offset contract and the deprecated
	// cursor, and declares a 422 and no 400. ASSUMPTION: its 422 is the bare
	// envelope networks answer with, and its cursor pages like theirs.
	projectInOrgList = pager{key: "projects", offset: offsetRule{offsetContract, refuseBare}, cursor: &networkingCursor}
	// listServiceAccounts and listServiceAccountCredentials document the same
	// offset contract and deprecated cursor, and the same 422 and no 400.
	// ASSUMPTION: they page as listProjectsInOrg does.
	serviceAccountList = pager{key: "serviceAccounts", offset: offsetRule{offsetContract, refuseBare}, cursor: &networkingCursor}
	credentialList     = pager{key: "credentials", offset: offsetRule{offsetContract, refuseBare}, cursor: &networkingCursor}
	roleList           = pager{key: "roles", offset: offsetRule{offsetContract, refuseBare}, cursor: &networkingCursor}
	organizationList   = pager{key: "organizations", count: "totalCount",
		offset: offsetRule{offsetOneRow, func(w http.ResponseWriter) {
			apiError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "offset and cursor paging are mutually exclusive")
		}}}

	// The compute lists declare PageToken and PageSize and page on the same
	// small maximum as the networking lists, for the same reason.
	//
	// ASSUMPTION: the 200 wrapper requires nothing, so an
	// empty result may omit the collection entirely rather than send `[]`.
	// Modelled as an omission because that is the value a Go client decodes
	// as nil, and nil-versus-empty is the class of thing that becomes a
	// permanent diff. A provider must read all three the same way.
	//
	// listClusters declares both a 400 and a 422 without saying which a
	// cursor draws. ASSUMPTION: a token it didn't issue is the 400 and a
	// pageSize out of range the 422, as on the storage lists.
	clusterList = pager{key: "clusters", omitEmpty: true,
		cursor: &cursorRule{page: 2, cap: 2, size: sizeChecked, maxSize: 500, badSize: sizeRefused422, badToken: tokenRefused400}}
	// listInstanceGroups: "a malformed `pageToken`, a `pageSize` that is not
	// an integer, is out of range, or is present and blank" is a 422 with
	// INVALID_ARGUMENT under `details`.
	instanceGroupList = pager{key: "instanceGroups", omitEmpty: true,
		cursor: &cursorRule{page: 2, cap: 2, size: sizeChecked, maxSize: 500, blankSizeRefused: true,
			badSize: sizeRefused422, badToken: tokenRefused422}}
	// listKubeconfigs declares only a 400, and its `total` counts every page.
	kubeconfigList = pager{key: "kubeconfigs", count: "total",
		cursor: &cursorRule{page: 2, cap: 2, size: sizeChecked, maxSize: 500,
			badSize: func(w http.ResponseWriter, raw string) {
				apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "pageSize must be between 0 and 500, got "+raw)
			},
			badToken: tokenRefused400}}

	// No pagination: the contract declares neither a cursor nor an offset on
	// these, so inventing a `nextPageToken` would teach a provider to page
	// something that never pages.
	clusterTypeVersionList = pager{key: "versions"}
	regionList             = pager{key: "regions"}
	attachmentList         = pager{key: "attachments"}
)

// networkingCursor is the cursor the four networking lists share. A cursor this
// server didn't issue is a 422 with the reason under `details`: these
// operations document 422 and no 400, and `listSecurityGroups` names a
// malformed `pageToken` as one. The real server clamps `pageSize` to 500, the
// size the provider asks for, so a test would need hundreds of objects to see
// a second page. The fake's own maximum is small enough that a handful does
// it.
var networkingCursor = cursorRule{page: 2, cap: 2, size: sizeChecked, badSize: sizeRefused422,
	badToken: tokenRefused422}

// storageCursor is the storage lists' cursor: a token not issued is a 400,
// and `pageSize` honoured whole.
var storageCursor = cursorRule{page: 2, size: sizeChecked, badSize: sizeRefused422, badToken: tokenRefused400}

// pager is one list route's paging, declared.
type pager struct {
	// key is the field the rows go in.
	key string
	// offset is how the route reads `offset`, `limit` and `includeTotal`.
	offset offsetRule
	// cursor is how it reads `pageToken` and `pageSize`. nil means it reads
	// neither and every row is on the one page, unless the offset says
	// otherwise.
	cursor *cursorRule
	// omitEmpty leaves the key out of an answer with no rows.
	omitEmpty bool
	// count names a count of every row, always beside the page, or "" for
	// none.
	count string
}

type offsetStyle int

const (
	// offsetIgnored is a route that documents no offset and reads none.
	offsetIgnored offsetStyle = iota
	// offsetContract is the offset contract on the five lists that document
	// `offset` and `limit`: networks, subnets, VPCs, security groups and
	// address pools. See offsetPage.
	offsetContract
	// offsetRefused answers `offset` or `limit` with refuse.
	offsetRefused
	// offsetOneRow is `offset` alone, one row a page, with a 400 for one that
	// isn't a non-negative integer and the count always present. A non-nil
	// refuse answers a request that mixes in a cursor parameter.
	offsetOneRow
)

type offsetRule struct {
	style offsetStyle
	// refuse writes the 422 the style answers with. The real handlers don't
	// share one writer for it, so each route names its own.
	refuse func(http.ResponseWriter)
}

type sizeRule int

const (
	// sizeIgnored reads no `pageSize`.
	sizeIgnored sizeRule = iota
	// sizeLenient takes a positive `pageSize` and ignores anything else.
	sizeLenient
	// sizeChecked takes a non-negative `pageSize`, refuses the rest with
	// badSize, and reads zero as a request for the default. That's the
	// documented difference between `pageSize` and the `limit` that replaces
	// it, and a client that can't easily omit the parameter depends on it.
	sizeChecked
)

type cursorRule struct {
	// page is how many rows a page holds when `pageSize` doesn't say.
	page int
	// cap is the most rows a page holds, whatever `pageSize` says, or 0 for no
	// ceiling but `pageSize`.
	cap  int
	size sizeRule
	// maxSize is the largest `pageSize` sizeChecked admits, or 0 for no
	// ceiling.
	maxSize int
	// blankSizeRefused refuses a `pageSize` that is present and empty, which
	// sizeChecked otherwise reads as absent.
	blankSizeRefused bool
	badSize          func(w http.ResponseWriter, raw string)
	// badToken answers a `pageToken` this server didn't issue. nil reads one
	// as the start of the list.
	badToken func(w http.ResponseWriter)
}

// serve answers one page of rows, or the route's refusal of the request.
func (p pager) serve(w http.ResponseWriter, r *http.Request, rows []any) {
	if rows == nil {
		rows = []any{}
	}
	q := r.URL.Query()
	switch p.offset.style {
	case offsetIgnored:
	case offsetContract:
		if asksForOffset(q) {
			p.offsetPage(w, q, rows)
			return
		}
	case offsetRefused:
		if q.Has("offset") || q.Has("limit") {
			p.offset.refuse(w)
			return
		}
	case offsetOneRow:
		if !p.refusesMixed(w, q) {
			p.oneRowPage(w, q, rows)
		}
		return
	}
	if p.cursor == nil {
		p.whole(w, rows)
		return
	}
	p.cursorPage(w, q, rows)
}

// refusesMixed answers a request that asks for both paging styles, on a route
// that refuses one, and reports whether it did.
func (p pager) refusesMixed(w http.ResponseWriter, q url.Values) bool {
	if p.offset.style != offsetOneRow || p.offset.refuse == nil {
		return false
	}
	if (q.Has("pageToken") || q.Has("pageSize")) && asksForOffset(q) {
		p.offset.refuse(w)
		return true
	}
	return false
}

func (p pager) whole(w http.ResponseWriter, rows []any) {
	if len(rows) == 0 && p.omitEmpty {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	body := map[string]any{p.key: rows}
	if p.count != "" {
		body[p.count] = len(rows)
	}
	writeJSON(w, http.StatusOK, body)
}

func (p pager) oneRowPage(w http.ResponseWriter, q url.Values, rows []any) {
	start := 0
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "offset is not a non-negative integer")
			return
		}
		start = min(n, len(rows))
	}
	end := min(start+1, len(rows))
	writeJSON(w, http.StatusOK, map[string]any{p.key: rows[start:end], p.count: len(rows)})
}

// cursorPage answers one page on the cursor parameters. There's no
// `totalCount`: the real cursor path leaves it out, and only the offset path
// reports one.
func (p pager) cursorPage(w http.ResponseWriter, q url.Values, rows []any) {
	c := p.cursor
	size := c.page
	switch c.size {
	case sizeIgnored:
	case sizeLenient:
		if n, err := strconv.Atoi(q.Get("pageSize")); err == nil && n > 0 {
			size = n
		}
	case sizeChecked:
		if raw := q.Get("pageSize"); raw != "" || (c.blankSizeRefused && q.Has("pageSize")) {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || (c.maxSize > 0 && n > c.maxSize) {
				c.badSize(w, raw)
				return
			}
			if n > 0 {
				size = n
			}
		}
	}
	if c.cap > 0 {
		size = min(size, c.cap)
	}

	start := 0
	token := q.Get("pageToken")
	switch {
	case c.badToken == nil:
		if n, err := strconv.Atoi(token); err == nil && n > 0 {
			start = min(n, len(rows))
		}
	case token != "":
		n, err := strconv.Atoi(token)
		if err != nil || n < 0 || n > len(rows) {
			c.badToken(w)
			return
		}
		start = n
	}

	if len(rows) == 0 && p.omitEmpty {
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	end := min(start+size, len(rows))
	body := map[string]any{p.key: rows[start:end]}
	if end < len(rows) {
		body["nextPageToken"] = strconv.Itoa(end)
	}
	if p.count != "" {
		body[p.count] = len(rows)
	}
	writeJSON(w, http.StatusOK, body)
}

// The offset contract's bounds, as the real server's parser has them. A
// `limit` outside them is refused rather than clamped.
const (
	offsetDefaultLimit = 50
	offsetMaxLimit     = 500
)

// offsetPage answers the offset contract. The style is decided by presence,
// as the real server decides it: any of `offset`, `limit` or `includeTotal`
// asks for it, even empty, and a bare request is an existing cursor client.
// Mixing in either cursor parameter is a 422, as is any value out of range.
// `offset` and `limit` are echoed, `totalCount` is there unless
// `includeTotal=false`, and there is never a `nextPageToken`. The page applied
// is the page requested, whatever the route's cursor maximum.
func (p pager) offsetPage(w http.ResponseWriter, q url.Values, rows []any) {
	if q.Has("pageToken") || q.Has("pageSize") {
		p.offset.refuse(w)
		return
	}
	offset, limit, includeTotal, ok := offsetParams(q)
	if !ok {
		p.offset.refuse(w)
		return
	}
	start := min(offset, len(rows))
	end := min(start+limit, len(rows))
	body := map[string]any{p.key: rows[start:end], "offset": offset, "limit": limit}
	if includeTotal {
		body["totalCount"] = len(rows)
	}
	writeJSON(w, http.StatusOK, body)
}

func asksForOffset(q url.Values) bool {
	return q.Has("offset") || q.Has("limit") || q.Has("includeTotal")
}

// offsetParams reads the three offset parameters strictly. An offset past the
// end isn't an error: it's an empty page beside the real total.
func offsetParams(q url.Values) (offset, limit int, includeTotal, ok bool) {
	if raw := q.Get("offset"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > math.MaxInt32 {
			return 0, 0, false, false
		}
		offset = n
	}
	limit = offsetDefaultLimit
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > offsetMaxLimit {
			return 0, 0, false, false
		}
		limit = n
	}
	switch q.Get("includeTotal") {
	case "", "true":
		includeTotal = true
	case "false":
	default:
		return 0, 0, false, false
	}
	return offset, limit, includeTotal, true
}

// refuseBare is the 422 networks, subnets and VPCs write: the plain envelope,
// with no reason anywhere. The real message is the masked "validation".
func refuseBare(w http.ResponseWriter) {
	errorEnvelope(w, http.StatusUnprocessableEntity, "", "validation", nil)
}

// refuseWithReason is the 422 security groups and address pools write, with
// the reason under `details`.
func refuseWithReason(w http.ResponseWriter) {
	errorEnvelope(w, http.StatusUnprocessableEntity, "", "validation",
		[]any{map[string]any{"reason": "INVALID_ARGUMENT", "message": "validation"}})
}

// tokenRefused400 is the 400 for a cursor this server didn't issue, on the
// lists that document a 400 for it.
func tokenRefused400(w http.ResponseWriter) {
	apiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "pageToken is not one this server issued")
}

// tokenRefused422 is the 422 for a cursor this server didn't issue, with the
// reason under `details`, on the lists that document a 422 and no 400 for it.
func tokenRefused422(w http.ResponseWriter) {
	storageError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT", "pageToken is not one this server issued")
}

// sizeRefused422 is the 422 for a `pageSize` that isn't a non-negative
// integer, with the reason under `details` as the storage operations put it.
func sizeRefused422(w http.ResponseWriter, raw string) {
	storageError(w, http.StatusUnprocessableEntity, "INVALID_ARGUMENT",
		"pageSize must be a non-negative integer, got "+raw)
}
