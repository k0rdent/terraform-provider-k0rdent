package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"testing"
)

// listAll walks a list handler by its cursor, asking for the provider's page
// size, and returns the ids and how many pages it took.
func listAll(t *testing.T, h http.HandlerFunc, key string) ([]string, int) {
	t.Helper()
	var ids []string
	token := ""
	for pages := 1; pages < 100; pages++ {
		req := httptest.NewRequest(http.MethodGet, "/?pageSize=500&pageToken="+token, nil)
		req.SetPathValue("networkId", "n")
		req.SetPathValue("vpcId", "v")
		rec := httptest.NewRecorder()
		h(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d: %d %s", pages, rec.Code, rec.Body)
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		var rows []struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(out[key], &rows); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		var next string
		if raw, ok := out["nextPageToken"]; ok {
			if err := json.Unmarshal(raw, &next); err != nil {
				t.Fatal(err)
			}
		}
		if next == "" {
			return ids, pages
		}
		token = next
	}
	t.Fatal("the list never ended")
	return nil, 0
}

// These lists page on a small maximum, as the real server pages on its own, so
// a provider that read only the first page fails an acceptance test with a
// handful of objects in it.
func TestTheNetworkingListsPage(t *testing.T) {
	s := newState()
	o := s.in(httptest.NewRequest(http.MethodGet, "/", nil)) // the scope listAll addresses
	withParents(o)
	for i := range 5 {
		id := "x" + strconv.Itoa(i)
		o.networks[at{id: id}] = &fakeNetwork{id: id, kind: "evpn", state: "active"}
		o.subnets[at{"n", id}] = &fakeSubnet{id: id, network: "n", state: "active"}
		o.groupsSec[at{id: id}] = &fakeSecurityGroup{id: id, state: "active"}
		o.peerings[at{"v", id}] = &fakePeering{id: id, vpc: "v", state: "active"}
	}
	for name, list := range map[string]struct {
		h   http.HandlerFunc
		key string
	}{
		"networks":        {s.listNetworks, "networks"},
		"subnets":         {s.listSubnets, "subnets"},
		"security groups": {s.listSecurityGroups, "securityGroups"},
		"peerings":        {s.listVPCPeerings, "peerings"},
	} {
		ids, pages := listAll(t, list.h, list.key)
		if name == "networks" {
			ids = slices.DeleteFunc(ids, func(id string) bool { return id == "n" })
		}
		if len(ids) != 5 || ids[0] != "x0" || ids[4] != "x4" || pages < 2 {
			t.Errorf("%s: got %v in %d pages, want all five over more than one", name, ids, pages)
		}
	}
}

// A cursor this server didn't issue is a 422 on all four lists, with the
// reason under `details`, as the contract documents. None of them documents
// a 400.
func TestTheNetworkingListsRefuseAForeignCursor(t *testing.T) {
	s := newState()
	withParents(s.in(httptest.NewRequest(http.MethodGet, "/", nil)))
	for name, h := range map[string]http.HandlerFunc{
		"networks": s.listNetworks, "subnets": s.listSubnets,
		"security groups": s.listSecurityGroups, "peerings": s.listVPCPeerings,
	} {
		for _, query := range []string{"pageToken=nope", "pageSize=-1"} {
			req := httptest.NewRequest(http.MethodGet, "/?"+query, nil)
			req.SetPathValue("networkId", "n")
			req.SetPathValue("vpcId", "v")
			rec := httptest.NewRecorder()
			h(rec, req)
			var out struct {
				Error struct {
					Details []struct {
						Reason string `json:"reason"`
					} `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusUnprocessableEntity || len(out.Error.Details) != 1 ||
				out.Error.Details[0].Reason != "INVALID_ARGUMENT" {
				t.Errorf("%s with %s: %d %s, want 422 with details[].reason INVALID_ARGUMENT", name, query, rec.Code, rec.Body)
			}
		}
	}
}

// offsetLists are operations that document `offset` and `limit`, with the 422
// envelope each one's real handler writes. Security groups and address pools
// answer with the reason under `details`, the others with the bare envelope.
func offsetLists(s *state) map[string]struct {
	h         http.HandlerFunc
	key       string
	total     int
	reasonful bool
} {
	return map[string]struct {
		h         http.HandlerFunc
		key       string
		total     int
		reasonful bool
	}{
		"networks":        {s.listNetworks, "networks", 5, false},
		"subnets":         {s.listSubnets, "subnets", 5, false},
		"VPCs":            {s.listVPCs, "vpcs", 5, false},
		"security groups": {s.listSecurityGroups, "securityGroups", 5, true},
		"address pools":   {s.listAddressPools, "addressPools", len(addressPools), true},
		"cluster types":   {s.listClusterTypes, "clusterTypes", listedByDefault(), false},
	}
}

// listedByDefault counts the cluster types a list that doesn't ask for the
// hidden ones answers with.
func listedByDefault() int {
	n := 0
	for _, ct := range catalog {
		if ct.status == "available" {
			n++
		}
	}
	return n
}

func seededForOffset(t *testing.T) *state {
	t.Helper()
	s := newState()
	o := s.in(httptest.NewRequest(http.MethodGet, "/", nil))
	for i := range 5 {
		id := "x" + strconv.Itoa(i)
		o.networks[at{id: id}] = &fakeNetwork{id: id, kind: "evpn", state: "active"}
		o.subnets[at{"x0", id}] = &fakeSubnet{id: id, network: "x0", state: "active"}
		o.groupsSec[at{id: id}] = &fakeSecurityGroup{id: id, state: "active"}
		o.vpcs[at{id: id}] = &fakeVPC{id: id, uid: id, owner: id, state: "active"}
	}
	return s
}

// withParents gives the scope listAll addresses the network and the VPC its
// subnets and peerings sit under. A list under a parent that isn't there is
// the parent's 404, not a page.
func withParents(o *objects) {
	o.networks[at{id: "n"}] = &fakeNetwork{id: "n", kind: "evpn", state: "active"}
	o.vpcs[at{id: "v"}] = &fakeVPC{id: "v", uid: "v", owner: "v", state: "active"}
}

type offsetPage struct {
	rows       int
	totalCount *int
	offset     *int
	limit      *int
	next       bool
}

func getOffsetPage(t *testing.T, h http.HandlerFunc, key, query string) (int, offsetPage, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/?"+query, nil)
	req.SetPathValue("networkId", "x0")
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		return rec.Code, offsetPage{}, rec.Body.Bytes()
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	var rows []any
	if err := json.Unmarshal(out[key], &rows); err != nil {
		t.Fatalf("%s: %v", key, err)
	}
	num := func(name string) *int {
		raw, ok := out[name]
		if !ok {
			return nil
		}
		var n int
		if err := json.Unmarshal(raw, &n); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return &n
	}
	_, next := out["nextPageToken"]
	return rec.Code, offsetPage{rows: len(rows), totalCount: num("totalCount"), offset: num("offset"),
		limit: num("limit"), next: next}, rec.Body.Bytes()
}

// On the offset path the page applied is the page requested, whatever the
// fake's own cursor maximum, and the end is where `offset` plus the rows
// reaches `totalCount`. `offset` and `limit` come back as sent, and there is
// never a `nextPageToken`.
func TestTheOffsetListsServeThePageAsked(t *testing.T) {
	s := seededForOffset(t)
	for name, list := range offsetLists(s) {
		for _, tc := range []struct {
			query       string
			rows, off   int
			limit       int
			countAbsent bool
		}{
			{query: "offset=0&limit=500", rows: list.total, limit: 500},
			{query: "offset=1&limit=2", rows: 2, off: 1, limit: 2},
			{query: "offset=2&limit=2", rows: list.total - 2, off: 2, limit: 2},
			{query: "offset=9999&limit=2", rows: 0, off: 9999, limit: 2},
			{query: "offset=0", rows: list.total, limit: 50},
			{query: "limit=2", rows: 2, limit: 2},
			{query: "offset=0&limit=500&includeTotal=false", rows: list.total, limit: 500, countAbsent: true},
		} {
			code, got, body := getOffsetPage(t, list.h, list.key, tc.query)
			if code != http.StatusOK {
				t.Errorf("%s ?%s: %d %s", name, tc.query, code, body)
				continue
			}
			if got.rows != min(tc.rows, tc.limit) || got.next ||
				got.offset == nil || *got.offset != tc.off || got.limit == nil || *got.limit != tc.limit {
				t.Errorf("%s ?%s: got %+v, want %d rows at offset %d, limit %d and no token",
					name, tc.query, got, min(tc.rows, tc.limit), tc.off, tc.limit)
			}
			switch {
			case tc.countAbsent && got.totalCount != nil:
				t.Errorf("%s ?%s: totalCount %d after includeTotal=false", name, tc.query, *got.totalCount)
			case !tc.countAbsent && (got.totalCount == nil || *got.totalCount != list.total):
				t.Errorf("%s ?%s: totalCount %v, want %d", name, tc.query, got.totalCount, list.total)
			}
		}
	}
}

// A request with no offset-style parameter is an existing cursor client. It
// gets the cursor page and none of the offset path's fields: a `totalCount`
// there would be a claim the real cursor path never makes. The cluster type
// list never had a cursor, so its first page is the last.
func TestTheOffsetListsKeepTheCursorForABareRequest(t *testing.T) {
	s := seededForOffset(t)
	for name, list := range offsetLists(s) {
		for _, query := range []string{"", "pageSize=2"} {
			code, got, body := getOffsetPage(t, list.h, list.key, query)
			if code != http.StatusOK {
				t.Errorf("%s ?%s: %d %s", name, query, code, body)
				continue
			}
			if got.totalCount != nil || got.offset != nil || got.limit != nil {
				t.Errorf("%s ?%s: got %+v, want no offset fields", name, query, got)
			}
			switch {
			case name == "cluster types" && (got.next || got.rows >= list.total):
				t.Errorf("%s ?%s: got %+v, want a first page short of all %d and no token", name, query, got, list.total)
			case name != "cluster types" && query != "" && !got.next:
				t.Errorf("%s ?%s: one page of two rows ended the list", name, query)
			}
		}
	}
}

// A cluster type with no `available` version is listed only beside
// `includeDeprecated=true`, and counted only there.
func TestTheClusterTypeListHidesATypeWithNoAvailableVersion(t *testing.T) {
	if listedByDefault() == len(catalog) {
		t.Fatal("every cluster type in the catalog is available, so nothing is hidden")
	}
	s := newState()
	for query, want := range map[string]int{
		"offset=0": listedByDefault(), "offset=0&includeDeprecated=false": listedByDefault(),
		"offset=0&includeDeprecated=true": len(catalog),
	} {
		code, got, body := getOffsetPage(t, s.listClusterTypes, "clusterTypes", query)
		if code != http.StatusOK || got.rows != want || got.totalCount == nil || *got.totalCount != want {
			t.Errorf("?%s: %d %s, want %d types", query, code, body, want)
		}
	}
}

// listClusterTypes refuses every `pageToken`: on its own with a 400, as the
// cursor was never implemented there, and beside an offset parameter with the
// 422 for mixed styles. Both envelopes are bare.
func TestTheClusterTypeListRefusesEveryCursor(t *testing.T) {
	s := newState()
	for query, want := range map[string]int{
		"pageToken=2": 400, "pageToken=nope": 400, "pageSize=500&pageToken=2": 400,
		"offset=0&pageToken=2": 422,
	} {
		code, _, body := getOffsetPage(t, s.listClusterTypes, "clusterTypes", query)
		var out struct {
			Error struct {
				Reason  *string `json:"reason"`
				Details []any   `json:"details"`
			} `json:"error"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatalf("?%s: %v: %s", query, err, body)
		}
		if code != want || out.Error.Reason != nil || out.Error.Details != nil {
			t.Errorf("?%s: %d %s, want a bare %d", query, code, body, want)
		}
	}
}

// Mixing the styles, in any combination, and an offset, limit or includeTotal
// out of range are each a 422, in the envelope that route's handler writes.
// Nothing is clamped.
func TestTheOffsetListsRefuseMixedStylesAndOutOfRangeValues(t *testing.T) {
	s := seededForOffset(t)
	for name, list := range offsetLists(s) {
		for _, query := range []string{
			"offset=0&pageSize=2", "limit=5&pageToken=abc", "offset=0&pageToken=",
			"includeTotal=false&pageToken=2",
			"limit=0", "limit=501", "limit=-1", "limit=two",
			"offset=-1", "offset=x", "offset=2147483648", "includeTotal=1",
		} {
			code, _, body := getOffsetPage(t, list.h, list.key, query)
			var out struct {
				Error struct {
					Code    int     `json:"code"`
					Message string  `json:"message"`
					Reason  *string `json:"reason"`
					Details []struct {
						Reason string `json:"reason"`
					} `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &out); err != nil {
				t.Fatalf("%s ?%s: %v: %s", name, query, err, body)
			}
			if code != http.StatusUnprocessableEntity || out.Error.Code != 422 || out.Error.Message == "" ||
				out.Error.Reason != nil {
				t.Errorf("%s ?%s: %d %s, want a 422 with no top-level reason", name, query, code, body)
				continue
			}
			reasonful := len(out.Error.Details) == 1 && out.Error.Details[0].Reason == "INVALID_ARGUMENT"
			if list.reasonful != reasonful || (!list.reasonful && out.Error.Details != nil) {
				t.Errorf("%s ?%s: %s, want details[].reason %v", name, query, body, list.reasonful)
			}
		}
	}
}
