package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// twoPageList answers any list with one row per page over two pages, and
// records the cursor and page size each request carried. A caller that drops
// either gets the first page back forever, which the bound turns into an
// error, or reads one row.
type twoPageList struct {
	key, kind string // the list field, and `type` for a union like Network
	mu        sync.Mutex
	asked     []string
}

func (l *twoPageList) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		l.mu.Lock()
		l.asked = append(l.asked, q.Get("pageToken")+"/"+q.Get("pageSize"))
		l.mu.Unlock()
		if q.Has("offset") || q.Has("limit") {
			t.Errorf("%s: sent offset paging on a cursor list: %s", l.key, r.URL.RawQuery)
		}
		row := func(id string) map[string]any {
			out := map[string]any{"id": id, "displayName": id}
			if l.kind != "" {
				out["type"] = l.kind
			}
			return out
		}
		body := map[string]any{l.key: []any{row("a")}, "nextPageToken": "p2"}
		if q.Get("pageToken") == "p2" {
			body = map[string]any{l.key: []any{row("b")}}
		}
		writeTestJSON(t, w, http.StatusOK, body)
	}))
	t.Cleanup(server.Close)
	return server
}

// readTestDataSource runs a data source's Read with every attribute null
// except those in set.
func readTestDataSource(t *testing.T, ds datasource.DataSource, set map[string]tftypes.Value) datasource.ReadResponse {
	t.Helper()
	ctx := context.Background()
	var s datasource.SchemaResponse
	ds.Schema(ctx, datasource.SchemaRequest{}, &s)
	typ, ok := s.Schema.Type().TerraformType(ctx).(tftypes.Object)
	if !ok {
		t.Fatal("the schema is not an object")
	}
	vals := map[string]tftypes.Value{}
	for name, at := range typ.AttributeTypes {
		vals[name] = tftypes.NewValue(at, nil)
	}
	for name, v := range set {
		vals[name] = v
	}
	raw := tftypes.NewValue(typ, vals)
	resp := datasource.ReadResponse{State: tfsdk.State{Raw: raw, Schema: s.Schema}}
	ds.Read(ctx, datasource.ReadRequest{Config: tfsdk.Config{Raw: raw, Schema: s.Schema}}, &resp)
	return resp
}

// Each cursor list hands the module's token and page size to its own request.
// The module's tests can't see that: they script the closure, and a closure
// that dropped the token would pass them. These are the lists whose operations
// document only the cursor.
func TestEveryCursorListSendsTheCursorOn(t *testing.T) {
	cases := []struct {
		name, key, kind string
		ds              func(*Meta) datasource.DataSource
		set             map[string]tftypes.Value
		// size is the page the operation takes, where it caps one below the
		// catalog's and answers 422 to more.
		size int
	}{
		{name: "instances", key: "instances", size: instancePageSize,
			ds: func(m *Meta) datasource.DataSource { return &instancesDataSource{meta: m} }},
		{name: "peerings", key: "peerings",
			ds:  func(m *Meta) datasource.DataSource { return &vpcPeeringsDataSource{meta: m} },
			set: map[string]tftypes.Value{"vpc": str("v")}},
		{name: "share_types", key: "shareTypes",
			ds: func(m *Meta) datasource.DataSource { return &shareTypesDataSource{meta: m} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			size := catalogPageSize
			if tc.size != 0 {
				size = tc.size
			}
			want := []string{"/" + strconv.Itoa(size), "p2/" + strconv.Itoa(size)}
			list := &twoPageList{key: tc.key, kind: tc.kind}
			server := list.serve(t)
			resp := readTestDataSource(t, tc.ds(testClusterMeta(t, server.URL, time.Minute)), tc.set)
			if resp.Diagnostics.HasError() {
				t.Fatalf("read: %v", resp.Diagnostics.Errors())
			}
			if !slices.Equal(list.asked, want) {
				t.Errorf("asked for %v, want %v", list.asked, want)
			}
			var rows types.List
			resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root(tc.name), &rows)...)
			if got := len(rows.Elements()); got != 2 {
				t.Errorf("%s holds %d rows, want one from each page", tc.name, got)
			}
		})
	}
}

// The kubeconfig list is read inside a wait, through the module's error form.
func TestTheKubeconfigListSendsTheCursorOn(t *testing.T) {
	list := &twoPageList{key: "kubeconfigs"}
	server := list.serve(t)
	items, err := readKubeconfigList(context.Background(), testClusterMeta(t, server.URL, time.Minute),
		clusterTarget{region: "test", project: "test"}, "c1")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{fmt.Sprintf("/%d", catalogPageSize), fmt.Sprintf("p2/%d", catalogPageSize)}; len(items) != 2 || !slices.Equal(list.asked, want) {
		t.Errorf("got %d records from %v, want 2 from %v", len(items), list.asked, want)
	}
}

// offsetList answers a list the way the five migrated operations do. A request
// carrying any offset-style parameter gets the offset contract, one carrying
// none is served the deprecated cursor, and one mixing the two is a 422. Rows
// come one per page under a totalCount of two, so a caller that advanced
// `offset` by the limit rather than by the rows it got would skip the second.
type offsetList struct {
	key, kind string
	// repeat serves "a" again at offset 1, as a list sorted newest first does
	// when a row is created between the two pages.
	repeat bool
	mu     sync.Mutex
	asked  []string // offset/limit, as each offset request carried them
	cursor int      // requests the cursor path served
}

func (l *offsetList) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		offsetStyle := q.Has("offset") || q.Has("limit") || q.Has("includeTotal")
		cursorStyle := q.Has("pageToken") || q.Has("pageSize")
		l.mu.Lock()
		defer l.mu.Unlock()
		if offsetStyle && cursorStyle {
			writeTestJSON(t, w, http.StatusUnprocessableEntity, map[string]any{
				"error": map[string]any{"code": 422, "message": "validation"},
			})
			return
		}
		row := func(id string) map[string]any {
			out := map[string]any{"id": id, "displayName": id}
			if l.kind != "" {
				out["type"] = l.kind
			}
			return out
		}
		if !offsetStyle {
			// What a caller that fell back would get: a whole list on one
			// cursor page, which reads as success unless the wire is checked.
			l.cursor++
			writeTestJSON(t, w, http.StatusOK, map[string]any{l.key: []any{row("a"), row("b")}})
			return
		}
		l.asked = append(l.asked, q.Get("offset")+"/"+q.Get("limit"))
		rows := []any{}
		switch q.Get("offset") {
		case "0":
			rows = append(rows, row("a"))
		case "1":
			if l.repeat {
				rows = append(rows, row("a"))
			} else {
				rows = append(rows, row("b"))
			}
		}
		writeTestJSON(t, w, http.StatusOK, map[string]any{l.key: rows, "totalCount": 2})
	}))
	t.Cleanup(server.Close)
	return server
}

// The lists whose operations document `offset` and `limit` send those and
// nothing else: `offset` advances by the rows received, `limit` is the
// contract's largest page, and no `pageToken` or `pageSize` goes out.
var offsetCallers = []struct {
	name, key, kind string
	ds              func(*Meta) datasource.DataSource
	set             map[string]tftypes.Value
}{
	{name: "networks", key: "networks", kind: "evpn",
		ds: func(m *Meta) datasource.DataSource { return &networksDataSource{meta: m} }},
	{name: "subnets", key: "subnets",
		ds:  func(m *Meta) datasource.DataSource { return &subnetsDataSource{meta: m} },
		set: map[string]tftypes.Value{"network": str("n")}},
	{name: "security_groups", key: "securityGroups",
		ds: func(m *Meta) datasource.DataSource { return &securityGroupsDataSource{meta: m} }},
	{name: "vpcs", key: "vpcs",
		ds: func(m *Meta) datasource.DataSource { return &vpcsDataSource{meta: m} }},
	{name: "address_pools", key: "addressPools",
		ds: func(m *Meta) datasource.DataSource { return &addressPoolsDataSource{meta: m} }},
	{name: "organizations", key: "organizations",
		ds: func(m *Meta) datasource.DataSource { return &organizationsDataSource{meta: m} }},
	{name: "cluster_types", key: "clusterTypes",
		ds: func(m *Meta) datasource.DataSource { return &clusterTypesDataSource{meta: m} }},
}

func TestEveryOffsetListSendsOffsetAndLimit(t *testing.T) {
	want := []string{"0/" + strconv.Itoa(catalogPageSize), "1/" + strconv.Itoa(catalogPageSize)}
	for _, tc := range offsetCallers {
		t.Run(tc.name, func(t *testing.T) {
			list := &offsetList{key: tc.key, kind: tc.kind}
			server := list.serve(t)
			resp := readTestDataSource(t, tc.ds(testClusterMeta(t, server.URL, time.Minute)), tc.set)
			if resp.Diagnostics.HasError() {
				t.Fatalf("read: %v", resp.Diagnostics.Errors())
			}
			// A caller back on the cursor reads both rows in one page, so the
			// row count alone would pass. The path the server took wouldn't.
			if list.cursor != 0 {
				t.Errorf("%d requests went down the cursor path", list.cursor)
			}
			if !slices.Equal(list.asked, want) {
				t.Errorf("asked for %v, want %v", list.asked, want)
			}
			var rows types.List
			resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root(tc.name), &rows)...)
			if got := len(rows.Elements()); got != 2 {
				t.Errorf("%s holds %d rows, want one from each page", tc.name, got)
			}
		})
	}
}

// An offset is a position, not a row, so a row created ahead of the reader
// pushes one already read onto the next page. Every offset list drops the
// second copy: an `ids` list with one id in it twice breaks a configuration
// that keys a map on it.
func TestEveryOffsetListDropsARowItReadTwice(t *testing.T) {
	for _, tc := range offsetCallers {
		t.Run(tc.name, func(t *testing.T) {
			list := &offsetList{key: tc.key, kind: tc.kind, repeat: true}
			server := list.serve(t)
			resp := readTestDataSource(t, tc.ds(testClusterMeta(t, server.URL, time.Minute)), tc.set)
			if resp.Diagnostics.HasError() {
				t.Fatalf("read: %v", resp.Diagnostics.Errors())
			}
			var rows types.List
			resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root(tc.name), &rows)...)
			if got := len(rows.Elements()); got != 1 {
				t.Errorf("%s holds %d rows, want the repeated one once", tc.name, got)
			}
		})
	}
}
