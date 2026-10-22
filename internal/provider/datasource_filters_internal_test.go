package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Every pair asked for has to be there with the same value. Tags the object
// carries beyond those don't count against it.
func TestTagsMatchEveryPairAskedFor(t *testing.T) {
	t.Parallel()

	have := map[string]string{"env": "prod", "team": "ml"}
	for _, tc := range []struct {
		name string
		want map[string]string
		ok   bool
	}{
		{"nothing asked", nil, true},
		{"an empty map asked", map[string]string{}, true},
		{"one pair present", map[string]string{"env": "prod"}, true},
		{"every pair present", map[string]string{"env": "prod", "team": "ml"}, true},
		{"a value differs", map[string]string{"env": "dev"}, false},
		{"a key is missing", map[string]string{"owner": "ml"}, false},
		{"one of two differs", map[string]string{"env": "prod", "team": "infra"}, false},
		{"an empty value is still a value", map[string]string{"env": ""}, false},
	} {
		if got := tagsMatch(tc.want, have); got != tc.ok {
			t.Errorf("%s: tagsMatch(%v) = %v, want %v", tc.name, tc.want, got, tc.ok)
		}
	}
	if tagsMatch(map[string]string{"env": "prod"}, nil) {
		t.Error("an object with no tags matched a tag filter")
	}
}

func TestTheFilterKeepsOnlyWhatMatchesEveryPart(t *testing.T) {
	t.Parallel()

	prod := "prod-gpu"
	lab := "lab-gpu"
	subject := func(name *string, state string, tags map[string]string) filterSubject {
		return filterSubject{displayName: name, state: state, tags: tags}
	}
	for _, tc := range []struct {
		name   string
		filter objectFilter
		in     filterSubject
		keep   bool
	}{
		{"no filter keeps everything", objectFilter{}, subject(nil, "", nil), true},
		{"unanchored regex", objectFilter{displayName: regexp.MustCompile("gpu")}, subject(&prod, "", nil), true},
		{"anchored regex refuses a later match", objectFilter{displayName: regexp.MustCompile("^gpu")}, subject(&prod, "", nil), false},
		{"anchored regex", objectFilter{displayName: regexp.MustCompile("^prod-")}, subject(&prod, "", nil), true},
		{"regex against another name", objectFilter{displayName: regexp.MustCompile("^prod-")}, subject(&lab, "", nil), false},
		{"regex against no display name", objectFilter{displayName: regexp.MustCompile(".*")}, subject(nil, "", nil), false},
		{"state matches", objectFilter{state: "active"}, subject(nil, "active", nil), true},
		{"state differs", objectFilter{state: "active"}, subject(nil, "failed", nil), false},
		{"state unknown to the platform", objectFilter{state: "active"}, subject(nil, "", nil), false},
		{"tags match", objectFilter{tags: map[string]string{"env": "prod"}}, subject(nil, "", map[string]string{"env": "prod"}), true},
		{"tags differ", objectFilter{tags: map[string]string{"env": "prod"}}, subject(nil, "", map[string]string{"env": "lab"}), false},
		{
			"every part has to hold",
			objectFilter{displayName: regexp.MustCompile("^prod-"), state: "active", tags: map[string]string{"env": "prod"}},
			subject(&prod, "failed", map[string]string{"env": "prod"}),
			false,
		},
	} {
		if got := tc.filter.keeps(tc.in); got != tc.keep {
			t.Errorf("%s: keeps = %v, want %v", tc.name, got, tc.keep)
		}
	}
}

// A pattern RE2 can't compile is a plan error. Unknown and null are left for
// later, as every validator here leaves them.
func TestABadRegexIsRefusedAtPlan(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		value types.String
		bad   bool
	}{
		{types.StringValue("^prod-[0-9]+$"), false},
		{types.StringValue(""), false},
		{types.StringValue("(unclosed"), true},
		{types.StringValue("a**"), true},
		// Perl lookahead, which RE2 doesn't have.
		{types.StringValue("^(?=prod)"), true},
		{types.StringNull(), false},
		{types.StringUnknown(), false},
	} {
		var resp validator.StringResponse
		validRegex{}.ValidateString(context.Background(), validator.StringRequest{
			Path: path.Root("display_name_regex"), ConfigValue: tc.value,
		}, &resp)
		if resp.Diagnostics.HasError() != tc.bad {
			t.Errorf("%s: error = %v, want %v", tc.value, resp.Diagnostics.Errors(), tc.bad)
		}
	}
}

// The consistency rule, checked on every plural data source rather than on the
// ones someone remembered: where the listed objects carry `display_name`,
// `tags`, `state` or `status`, the data source takes the matching filter. A
// new plural data source, or a new attribute on an old one, fails here until
// its filter exists.
func TestEveryPluralDataSourceFiltersOnWhatItsObjectsCarry(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	for _, newDataSource := range (&aiProvider{}).DataSources(ctx) {
		ds := newDataSource()
		var md datasource.MetadataResponse
		ds.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
		// A plural is named in the plural. A singular's nested lists, such
		// as a ClusterType's node pools, are parts of one object rather
		// than a catalog to search.
		if !strings.HasSuffix(md.TypeName, "s") {
			continue
		}
		var s datasource.SchemaResponse
		ds.Schema(ctx, datasource.SchemaRequest{}, &s)

		for listName, a := range s.Schema.Attributes {
			list, ok := a.(schema.ListNestedAttribute)
			if !ok || !list.Computed {
				continue
			}
			carried := list.NestedObject.Attributes
			for field, filter := range map[string]string{
				"display_name": "display_name_regex",
				"tags":         "tags",
				"state":        "state",
				"status":       "status",
			} {
				if _, has := carried[field]; !has {
					continue
				}
				got, has := s.Schema.Attributes[filter]
				if !has {
					t.Errorf("%s: `%s` carries `%s` but there's no `%s` filter", md.TypeName, listName, field, filter)
					continue
				}
				if !got.IsOptional() || got.IsRequired() || got.IsComputed() {
					t.Errorf("%s: `%s` has to be Optional only", md.TypeName, filter)
				}
				if re, ok := got.(schema.StringAttribute); ok && filter == "display_name_regex" {
					var resp validator.StringResponse
					for _, v := range re.Validators {
						v.ValidateString(ctx, validator.StringRequest{
							Path: path.Root(filter), ConfigValue: types.StringValue("(prod"),
						}, &resp)
					}
					if !resp.Diagnostics.HasError() {
						t.Errorf("%s: `%s` takes `(prod` at plan time", md.TypeName, filter)
					}
				}
				if !strings.Contains(got.GetMarkdownDescription(), "Filtered") &&
					!strings.Contains(got.GetMarkdownDescription(), "Sent to the platform") {
					t.Errorf("%s: `%s` doesn't say where it's filtered: %q", md.TypeName, filter, got.GetMarkdownDescription())
				}
			}
		}
	}
}

// Four networks over two pages, of which one matches all three filters. A
// filter applied to the first page only would find none of the second.
func TestTheNetworksFilterAfterReadingEveryPage(t *testing.T) {
	t.Parallel()

	row := func(id, name, state string, tags map[string]any) map[string]any {
		return map[string]any{
			"id": id, "type": "evpn", "displayName": name, "state": state, "tags": tags,
			"selfLink": "/v1/regions/test/projects/test/networking/networks/" + id,
		}
	}
	rows := []any{
		row("a", "prod-a", "active", map[string]any{"env": "lab"}),
		row("b", "lab-b", "active", map[string]any{"env": "prod"}),
		row("c", "prod-c", "failed", map[string]any{"env": "prod"}),
		row("d", "prod-d", "active", map[string]any{"env": "prod", "team": "ml"}),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("offset") == "2" {
			writeTestJSON(t, w, http.StatusOK, map[string]any{"networks": rows[2:], "totalCount": 4})
			return
		}
		writeTestJSON(t, w, http.StatusOK, map[string]any{"networks": rows[:2], "totalCount": 4})
	}))
	t.Cleanup(server.Close)

	resp := readTestDataSource(t, &networksDataSource{meta: testClusterMeta(t, server.URL, time.Minute)},
		map[string]tftypes.Value{
			"tags":               tfTags(map[string]string{"env": "prod"}),
			"display_name_regex": str("^prod-"),
			"state":              str("active"),
		})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	var got networksModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	if len(got.Networks) != 1 || got.Networks[0].ID.ValueString() != "d" {
		t.Fatalf("kept %v, want only d", got.Networks)
	}
	var ids []string
	resp.Diagnostics.Append(got.IDs.ElementsAs(context.Background(), &ids, false)...)
	if len(ids) != 1 || ids[0] != "d" {
		t.Errorf("ids = %v, want [d] at the same positions as networks", ids)
	}
	tags := map[string]string{}
	resp.Diagnostics.Append(got.Networks[0].Tags.ElementsAs(context.Background(), &tags, false)...)
	if tags["team"] != "ml" || tags["env"] != "prod" {
		t.Errorf("the kept network's tags read as %v", tags)
	}
}

// Where the list operation takes the filter, it goes to the platform, and what
// comes back is still checked: a platform that ignored the parameter would
// otherwise hand back rows the configuration said to leave out.
func TestAStateTheListTakesReachesThePlatform(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		query string
		set   map[string]tftypes.Value
		body  map[string]any
		ds    func(*Meta) datasource.DataSource
		count func(t *testing.T, resp datasource.ReadResponse) int
	}{
		{
			name:  "security groups",
			query: "state=active",
			set:   map[string]tftypes.Value{"state": str("active")},
			body: map[string]any{"totalCount": 2, "securityGroups": []any{
				map[string]any{"id": "a", "state": "active", "tags": map[string]any{}},
				map[string]any{"id": "b", "state": "failed", "tags": map[string]any{}},
			}},
			ds: func(m *Meta) datasource.DataSource { return &securityGroupsDataSource{meta: m} },
			count: func(t *testing.T, resp datasource.ReadResponse) int {
				var got securityGroupsModel
				resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
				return len(got.SecurityGroups)
			},
		},
		{
			name:  "instances",
			query: "inStates=running",
			set:   map[string]tftypes.Value{"state": str("running")},
			body: map[string]any{"instances": []any{
				map[string]any{"id": "inst-a", "state": "running"},
				map[string]any{"id": "inst-b", "state": "stopped"},
			}},
			ds: func(m *Meta) datasource.DataSource { return &instancesDataSource{meta: m} },
			count: func(t *testing.T, resp datasource.ReadResponse) int {
				var got instancesModel
				resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
				return len(got.Instances)
			},
		},
		{
			name:  "projects",
			query: "status=active",
			set:   map[string]tftypes.Value{"status": str("active")},
			body: map[string]any{"totalCount": 2, "projects": []any{
				map[string]any{"id": "a", "organizationId": "o", "status": "active"},
				map[string]any{"id": "b", "organizationId": "o", "status": "suspended"},
			}},
			ds: func(m *Meta) datasource.DataSource { return &projectsDataSource{meta: m} },
			count: func(t *testing.T, resp datasource.ReadResponse) int {
				var got projectsModel
				resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
				return len(got.Projects)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var query atomic.Value
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				query.Store(r.URL.RawQuery)
				writeTestJSON(t, w, http.StatusOK, tc.body)
			}))
			t.Cleanup(server.Close)

			resp := readTestDataSource(t, tc.ds(testClusterMeta(t, server.URL, time.Minute)), tc.set)
			if resp.Diagnostics.HasError() {
				t.Fatalf("read: %v", resp.Diagnostics.Errors())
			}
			if q := fmt.Sprint(query.Load()); !strings.Contains(q, tc.query) {
				t.Errorf("the request %q doesn't carry %s", q, tc.query)
			}
			if n := tc.count(t, resp); n != 1 {
				t.Errorf("kept %d rows, want the one the filter names", n)
			}
		})
	}
}

// The platform hides deprecated types unless asked, so `status = "deprecated"`
// on its own would always come back empty.
func TestAskingForDeprecatedClusterTypesAsksThePlatformForThem(t *testing.T) {
	t.Parallel()

	var query atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query.Store(r.URL.RawQuery)
		writeTestJSON(t, w, http.StatusOK, map[string]any{"clusterTypes": []any{
			map[string]any{"id": "old", "displayName": "Old", "status": "deprecated"},
			map[string]any{"id": "new", "displayName": "New", "status": "available"},
		}})
	}))
	t.Cleanup(server.Close)

	resp := readTestDataSource(t, &clusterTypesDataSource{meta: testClusterMeta(t, server.URL, time.Minute)},
		map[string]tftypes.Value{"status": str("deprecated")})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	if q := fmt.Sprint(query.Load()); !strings.Contains(q, "includeDeprecated=true") {
		t.Errorf("the request %q doesn't ask for deprecated types", q)
	}
	var got clusterTypesModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	if len(got.ClusterTypes) != 1 || got.ClusterTypes[0].ID.ValueString() != "old" {
		t.Errorf("kept %v, want only old", got.ClusterTypes)
	}
}

// Each version the list keeps costs a read of its own, so the name filter
// runs before those reads, not after.
func TestTheVersionNameFilterRunsBeforeEachVersionIsRead(t *testing.T) {
	t.Parallel()

	const a, b = "00000000-0000-4000-8000-00000000000a", "00000000-0000-4000-8000-00000000000b"
	var reads []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/versions") {
			writeTestJSON(t, w, http.StatusOK, map[string]any{"versions": []any{
				map[string]any{"versionId": a, "version": 1, "displayName": "GPU v1", "status": "available"},
				map[string]any{"versionId": b, "version": 2, "displayName": "CPU v2", "status": "available"},
			}})
			return
		}
		mu.Lock()
		reads = append(reads, r.URL.Path)
		mu.Unlock()
		writeTestJSON(t, w, http.StatusOK, map[string]any{
			"id": "gpu", "displayName": "GPU v1", "status": "available",
			"selfLink": "/v1/regions/test/cluster-types/gpu/versions/" + a,
		})
	}))
	t.Cleanup(server.Close)

	resp := readTestDataSource(t, &clusterTypeVersionsDataSource{meta: testClusterMeta(t, server.URL, time.Minute)},
		map[string]tftypes.Value{"cluster_type": str("gpu"), "display_name_regex": str("^GPU")})
	if resp.Diagnostics.HasError() {
		t.Fatalf("read: %v", resp.Diagnostics.Errors())
	}
	sort.Strings(reads)
	if len(reads) != 1 || !strings.HasSuffix(reads[0], a) {
		t.Errorf("read versions %v, want only the one the filter keeps", reads)
	}
}

// A subscription's state is its `is_active` flag, so that's the filter. One
// the platform reports without the flag is neither active nor inactive, and
// a filter on either leaves it out.
func TestTheSubscriptionsFilterOnIsActive(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, w, http.StatusOK, map[string]any{"subscriptions": []any{
			map[string]any{"id": "0f8fad5b-d9cb-469f-a165-70867728950a", "url": "https://a.example", "is_active": true},
			map[string]any{"id": "0f8fad5b-d9cb-469f-a165-70867728950b", "url": "https://b.example", "is_active": false},
			map[string]any{"id": "0f8fad5b-d9cb-469f-a165-70867728950c", "url": "https://c.example"},
		}})
	}))
	t.Cleanup(server.Close)
	meta := testClusterMeta(t, server.URL, time.Minute)

	for flag, want := range map[bool]string{true: "https://a.example", false: "https://b.example"} {
		resp := readTestDataSource(t, &webhookSubscriptionsDataSource{meta: meta},
			map[string]tftypes.Value{"is_active": tftypes.NewValue(tftypes.Bool, flag)})
		if resp.Diagnostics.HasError() {
			t.Fatalf("read: %v", resp.Diagnostics.Errors())
		}
		var got webhookSubscriptionsModel
		resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
		var views []webhookSubscriptionView
		resp.Diagnostics.Append(got.Subscriptions.ElementsAs(context.Background(), &views, false)...)
		if len(views) != 1 || views[0].URL.ValueString() != want {
			t.Errorf("is_active = %v kept %v, want only %s", flag, views, want)
		}
	}
}

// A list reports an object with no tags as null, the way the resources read
// one back, whether the platform sent {} or a map holding only the create
// token. One shape across the lists, so a `for` over any of them can test
// `tags == null` and mean the same thing.
func TestEveryListReportsNoTagsAsNull(t *testing.T) {
	t.Parallel()

	token := map[string]any{createTokenTag(): "t"}
	for _, tc := range []struct {
		name string
		key  string
		rows []any
		ds   func(*Meta) datasource.DataSource
		set  map[string]tftypes.Value
		tags func(t *testing.T, resp datasource.ReadResponse) []types.Map
	}{
		{
			name: "networks", key: "networks",
			rows: []any{
				map[string]any{"id": "a", "type": "evpn", "tags": map[string]any{}},
				map[string]any{"id": "b", "type": "evpn", "tags": token},
			},
			ds: func(m *Meta) datasource.DataSource { return &networksDataSource{meta: m} },
			tags: func(t *testing.T, resp datasource.ReadResponse) []types.Map {
				var got networksModel
				resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
				var out []types.Map
				for _, n := range got.Networks {
					out = append(out, n.Tags)
				}
				return out
			},
		},
		{
			name: "subnets", key: "subnets",
			rows: []any{
				map[string]any{"id": "a", "prefixLength": 24, "tags": map[string]any{}},
				map[string]any{"id": "b", "prefixLength": 24, "tags": token},
			},
			ds:  func(m *Meta) datasource.DataSource { return &subnetsDataSource{meta: m} },
			set: map[string]tftypes.Value{"network": str("n")},
			tags: func(t *testing.T, resp datasource.ReadResponse) []types.Map {
				var got subnetsModel
				resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
				var out []types.Map
				for _, n := range got.Subnets {
					out = append(out, n.Tags)
				}
				return out
			},
		},
		{
			name: "security groups", key: "securityGroups",
			rows: []any{
				map[string]any{"id": "a", "tags": map[string]any{}},
				map[string]any{"id": "b", "tags": token},
			},
			ds: func(m *Meta) datasource.DataSource { return &securityGroupsDataSource{meta: m} },
			tags: func(t *testing.T, resp datasource.ReadResponse) []types.Map {
				var got securityGroupsModel
				resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
				var out []types.Map
				for _, n := range got.SecurityGroups {
					out = append(out, n.Tags)
				}
				return out
			},
		},
		{
			name: "peerings", key: "peerings",
			rows: []any{
				map[string]any{"id": "a", "tags": map[string]any{}},
				map[string]any{"id": "b", "tags": token},
			},
			ds:  func(m *Meta) datasource.DataSource { return &vpcPeeringsDataSource{meta: m} },
			set: map[string]tftypes.Value{"vpc": str("v")},
			tags: func(t *testing.T, resp datasource.ReadResponse) []types.Map {
				var got vpcPeeringsModel
				resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
				var out []types.Map
				for _, n := range got.Peerings {
					out = append(out, n.Tags)
				}
				return out
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeTestJSON(t, w, http.StatusOK, map[string]any{tc.key: tc.rows, "totalCount": len(tc.rows)})
			}))
			t.Cleanup(server.Close)

			resp := readTestDataSource(t, tc.ds(testClusterMeta(t, server.URL, time.Minute)), tc.set)
			if resp.Diagnostics.HasError() {
				t.Fatalf("read: %v", resp.Diagnostics.Errors())
			}
			got := tc.tags(t, resp)
			if len(got) != 2 {
				t.Fatalf("read %d rows, want 2", len(got))
			}
			for i, m := range got {
				if !m.IsNull() {
					t.Errorf("row %d: tags read as %v, want null", i, m)
				}
			}
		})
	}
}

// A null value in the filter is a plan error on the attribute. Left to the
// read, it would surface as the framework failing to convert the map.
func TestATagsFilterWithANullValueIsAPlanError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	filterAttr := tagsFilterAttribute("networks")
	for _, tc := range []struct {
		value types.Map
		bad   bool
	}{
		{types.MapValueMust(types.StringType, map[string]attr.Value{"env": types.StringValue("prod")}), false},
		{types.MapValueMust(types.StringType, map[string]attr.Value{"env": types.StringNull()}), true},
		{types.MapValueMust(types.StringType, map[string]attr.Value{"env": types.StringUnknown()}), false},
		{types.MapNull(types.StringType), false},
	} {
		var resp validator.MapResponse
		for _, v := range filterAttr.Validators {
			v.ValidateMap(ctx, validator.MapRequest{Path: path.Root("tags"), ConfigValue: tc.value}, &resp)
		}
		if resp.Diagnostics.HasError() != tc.bad {
			t.Errorf("%v: error = %v, want %v", tc.value, resp.Diagnostics.Errors(), tc.bad)
		}
	}
}

// The platform hides deprecated types unless asked, so asking for only
// deprecated ones while saying not to include them can't return anything.
// That's refused at plan rather than answered with an empty list.
func TestDeprecatedStatusRefusesIncludeDeprecatedFalse(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ds := &clusterTypesDataSource{}
	var s datasource.SchemaResponse
	ds.Schema(ctx, datasource.SchemaRequest{}, &s)
	typ, ok := s.Schema.Type().TerraformType(ctx).(tftypes.Object)
	if !ok {
		t.Fatal("the schema is not an object")
	}
	for _, tc := range []struct {
		status  string
		include *bool
		bad     bool
	}{
		{"deprecated", ptr(false), true},
		{"deprecated", ptr(true), false},
		{"deprecated", nil, false},
		{"available", ptr(false), false},
	} {
		vals := map[string]tftypes.Value{}
		for name, at := range typ.AttributeTypes {
			vals[name] = tftypes.NewValue(at, nil)
		}
		vals["status"] = str(tc.status)
		if tc.include != nil {
			vals["include_deprecated"] = tftypes.NewValue(tftypes.Bool, *tc.include)
		}
		var resp datasource.ValidateConfigResponse
		ds.ValidateConfig(ctx, datasource.ValidateConfigRequest{
			Config: tfsdk.Config{Raw: tftypes.NewValue(typ, vals), Schema: s.Schema},
		}, &resp)
		if resp.Diagnostics.HasError() != tc.bad {
			t.Errorf("status %s, include_deprecated %v: error = %v, want %v",
				tc.status, tc.include, resp.Diagnostics.Errors(), tc.bad)
		}
	}
}

// A description names an enum's members the way a sentence would, so one
// built from Values() reads like the hand-written one it replaced.
func TestEnumProseReadsAsAList(t *testing.T) {
	for want, values := range map[string][]string{
		"`rw`":                           {"rw"},
		"`permit` or `deny`":             {"permit", "deny"},
		"`org`, `project` or `platform`": {"org", "project", "platform"},
	} {
		if got := enumProse(values); got != want {
			t.Errorf("enumProse(%q) = %s, want %s", values, got, want)
		}
	}
}
