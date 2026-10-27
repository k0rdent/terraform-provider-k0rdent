package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// Every list route's pager, by name. A route added without a row here is a
// route no test pages.
var everyList = map[string]pager{
	"networks": networkList, "subnets": subnetList, "security groups": securityGroupList,
	"peerings": peeringList, "VPCs": vpcList, "address pools": addressPoolList,
	"cluster types": clusterTypeList, "share types": shareTypeList, "file stores": fileStoreList,
	"instances": instanceList, "projects": projectList, "organizations": organizationList, "projects in an organization": projectInOrgList,
	"service accounts": serviceAccountList, "service account credentials": credentialList, "roles": roleList,
	"clusters": clusterList, "instance groups": instanceGroupList,
	"cluster type versions": clusterTypeVersionList, "regions": regionList,
	"attachments": attachmentList, "kubeconfigs": kubeconfigList,
}

// servePage asks one pager for one page.
func servePage(t *testing.T, p pager, rows []any, query string) (int, map[string]json.RawMessage) {
	t.Helper()
	rec := httptest.NewRecorder()
	p.serve(rec, httptest.NewRequest(http.MethodGet, "/?"+query, nil), rows)
	var out map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("?%s: %v: %s", query, err, rec.Body)
	}
	return rec.Code, out
}

// Whatever the route's style, a client that follows it gets every row once and
// in order, and a route that pages at all doesn't hand five rows over at once.
// The walk follows the cursor where there is one, the offset where that's the
// only way on, and otherwise reads the one page.
func TestEveryListGivesEveryRowOnceToAClientThatFollowsIt(t *testing.T) {
	rows := []any{}
	for i := range 5 {
		rows = append(rows, map[string]any{"id": "r" + strconv.Itoa(i)})
	}
	for name, p := range everyList {
		t.Run(name, func(t *testing.T) {
			var got []string
			pages := 0
			query := ""
			for pages < 20 {
				pages++
				code, out := servePage(t, p, rows, query)
				if code != http.StatusOK {
					t.Fatalf("?%s: %d %v", query, code, out)
				}
				var page []struct {
					ID string `json:"id"`
				}
				if err := json.Unmarshal(out[p.key], &page); err != nil {
					t.Fatalf("%s: %v", p.key, err)
				}
				for _, row := range page {
					got = append(got, row.ID)
				}
				var next string
				if raw, ok := out["nextPageToken"]; ok {
					if err := json.Unmarshal(raw, &next); err != nil {
						t.Fatal(err)
					}
				}
				switch {
				case next != "":
					query = "pageToken=" + next
					continue
				case (p.offset.style == offsetOneRow || p.cursor != nil && p.cursor.firstPageOnly) &&
					len(got) < len(rows) && len(page) > 0:
					query = "offset=" + strconv.Itoa(len(got))
					continue
				}
				break
			}
			if len(got) != len(rows) {
				t.Fatalf("got %v, want all %d rows", got, len(rows))
			}
			for i, id := range got {
				if id != "r"+strconv.Itoa(i) {
					t.Errorf("row %d is %s", i, id)
				}
			}
			perPage := len(rows)
			switch {
			case p.cursor != nil && p.cursor.cap > 0:
				perPage = min(p.cursor.page, p.cursor.cap)
			case p.cursor != nil:
				perPage = p.cursor.page
			case p.offset.style == offsetOneRow:
				perPage = 1
			}
			if perPage < len(rows) && pages < 2 {
				t.Errorf("one page held all %d rows", len(rows))
			}
		})
	}
}

// An empty list is `[]`, never `null`, except where the route leaves the key
// out altogether, and a count rides beside the rows only where declared.
func TestAnEmptyListIsAnEmptyArrayOrNoKey(t *testing.T) {
	for name, p := range everyList {
		t.Run(name, func(t *testing.T) {
			_, out := servePage(t, p, nil, "")
			raw, has := out[p.key]
			switch {
			case p.omitEmpty && has:
				t.Errorf("%s = %s, want the key left out", p.key, raw)
			case !p.omitEmpty && string(raw) != "[]":
				t.Errorf("%s = %s, want []", p.key, raw)
			}
			if _, has := out[p.count]; p.count != "" && !has {
				t.Errorf("no %s beside the rows: %v", p.count, out)
			}
		})
	}
}

// A route that reads offset alone answers one row a page with the count
// beside it, and a 400 for an offset that isn't a non-negative integer. The
// organizations list refuses a request that mixes in the cursor, and the
// projects list doesn't.
func TestTheOneRowOffsetRoutes(t *testing.T) {
	rows := []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}
	for name, p := range map[string]pager{"projects": projectList, "organizations": organizationList} {
		code, out := servePage(t, p, rows, "offset=1")
		if code != http.StatusOK || string(out["totalCount"]) != "2" || string(out[p.key]) != `[{"id":"b"}]` {
			t.Errorf("%s ?offset=1: %d %v", name, code, out)
		}
		for _, bad := range []string{"offset=-1", "offset=x"} {
			if got, out := servePage(t, p, rows, bad); got != http.StatusBadRequest {
				t.Errorf("%s ?%s: %d %v, want 400", name, bad, got, out)
			}
		}
		code, _ = servePage(t, p, rows, "offset=0&pageSize=1")
		if want := map[string]int{"projects": http.StatusOK, "organizations": http.StatusUnprocessableEntity}[name]; code != want {
			t.Errorf("%s mixing styles: %d, want %d", name, code, want)
		}
	}
}

// The instance list refuses an offset and a page over 200 with a 422, and
// reads a token it didn't issue as a 400. The VPC list reads either kind of
// junk as the start.
func TestTheRefusingAndTheLenientCursors(t *testing.T) {
	rows := []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}}
	for query, want := range map[string]int{
		"offset=0": 422, "limit=1": 422, "pageSize=201": 422, "pageSize=-1": 422,
		"pageSize=200": 200, "pageSize=0": 200, "pageToken=x": 400, "pageToken=9": 400,
	} {
		if code, out := servePage(t, instanceList, rows, query); code != want {
			t.Errorf("instances ?%s: %d %v, want %d", query, code, out, want)
		}
	}
	for _, query := range []string{"pageToken=x", "pageToken=-3", "pageSize=x", "pageSize=-1"} {
		code, out := servePage(t, vpcList, rows, query)
		if code != http.StatusOK || string(out["vpcs"]) != `[{"id":"a"},{"id":"b"}]` {
			t.Errorf("VPCs ?%s: %d %v, want the whole list from the start", query, code, out)
		}
	}
}

// listClusters, listInstanceGroups and listKubeconfigs declare PageToken and
// PageSize, so they page on the fake's small maximum like the other cursor
// lists, and a provider that reads only the first page fails. Each refuses a
// token it didn't issue in the envelope its operation documents: listClusters
// and listKubeconfigs with a 400, listInstanceGroups with a 422 carrying
// INVALID_ARGUMENT under `details`, which it also answers for a pageSize that
// is out of range or present and blank. A kubeconfig list's `total` counts
// every page.
func TestTheComputeListsPage(t *testing.T) {
	s := newState()
	for i := range 3 {
		id := "c" + strconv.Itoa(i)
		if code, out := in(t, s.createCluster, http.MethodPost, "r", "p", nil,
			`{"id":"`+id+`","clusterType":"`+inferenceRef+`/versions/`+inferenceV2+`"}`); code != http.StatusCreated {
			t.Fatalf("cluster: %d %v", code, out)
		}
		if code, out := in(t, s.createInstanceGroup, http.MethodPost, "r", "p", nil,
			`{"id":"`+id+`","clusterType":"`+inferenceRef+`/versions/`+inferenceV2+`","nodePools":[{"nodePool":"workers","size":2}]}`); code != http.StatusCreated {
			t.Fatalf("group: %d %v", code, out)
		}
	}
	get := func(h http.HandlerFunc, query string) (int, map[string]any) {
		req := httptest.NewRequest(http.MethodGet, "/?"+query, nil)
		req.SetPathValue("region", "r")
		req.SetPathValue("project", "p")
		req.SetPathValue("id", "c0")
		rec := httptest.NewRecorder()
		h(rec, req)
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("%v: %s", err, rec.Body)
		}
		return rec.Code, out
	}
	for name, l := range map[string]struct {
		h   http.HandlerFunc
		key string
	}{"clusters": {s.listClusters, "clusters"}, "instance groups": {s.listInstanceGroups, "instanceGroups"}} {
		code, out := get(l.h, "pageSize=500")
		if code != http.StatusOK || listed(out, l.key) >= 3 || out["nextPageToken"] == nil {
			t.Errorf("%s: %d %v, want a first page short of all three and a token", name, code, out)
		}
	}
	for query, want := range map[string]int{"pageToken=nope": 400, "pageToken=9": 400} {
		if code, out := get(s.listClusters, query); code != want {
			t.Errorf("clusters ?%s: %d %v, want %d", query, code, out, want)
		}
		if code, out := get(s.listKubeconfigs, query); code != want {
			t.Errorf("kubeconfigs ?%s: %d %v, want %d", query, code, out, want)
		}
	}
	for _, query := range []string{"pageToken=nope", "pageSize=501", "pageSize=-1", "pageSize=x", "pageSize="} {
		code, out := get(s.listInstanceGroups, query)
		if code != http.StatusUnprocessableEntity || detailReason(out) != "INVALID_ARGUMENT" {
			t.Errorf("instance groups ?%s: %d %v, want 422 with details[].reason INVALID_ARGUMENT", query, code, out)
		}
	}
	if _, out := get(s.listKubeconfigs, ""); out["total"] != float64(0) {
		t.Errorf("kubeconfigs of an unsettled cluster: %v", out)
	}
}
