package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// The list resources are driven through the plugin protocol, the way
// `terraform query` reaches them, against a stub of the list operations. The
// acceptance suite runs real `terraform query` against the fake, with
// -generate-config-out, and that's where the generated configuration is held
// to validating and planning clean.

// listAPI is a stub API: the token mint, and whatever routes a test adds.
func listAPI(t *testing.T, routes map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/regions/global/auth/token", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"access_token": "t", "token_type": "Bearer", "expires_in": 900})
	})
	for pattern, h := range routes {
		mux.HandleFunc(pattern, h)
	}
	api := httptest.NewServer(mux)
	t.Cleanup(api.Close)
	return api
}

// paged serves rows one to a page, on the paging style the operation
// documents, so a list that reads only the first page finds one row.
func paged(t *testing.T, key string, offset bool, rows ...map[string]any) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if offset {
			if q.Has("pageToken") || q.Has("pageSize") {
				t.Errorf("%s: cursor paging on an offset list: %s", r.URL.Path, r.URL.RawQuery)
			}
			at, _ := strconv.Atoi(q.Get("offset")) //nolint:errcheck // absent reads as the first page
			page := []any{}
			if at < len(rows) {
				page = append(page, rows[at])
			}
			writeTestJSON(t, w, http.StatusOK, map[string]any{key: page, "totalCount": len(rows)})
			return
		}
		if q.Has("offset") || q.Has("limit") {
			t.Errorf("%s: offset paging on a cursor list: %s", r.URL.Path, r.URL.RawQuery)
		}
		at, _ := strconv.Atoi(q.Get("pageToken")) //nolint:errcheck // absent reads as the first page
		body := map[string]any{key: []any{}}
		if at < len(rows) {
			body[key] = []any{rows[at]}
		}
		if at+1 < len(rows) {
			body["nextPageToken"] = strconv.Itoa(at + 1)
		}
		writeTestJSON(t, w, http.StatusOK, body)
	}
}

// listServer is the provider's protocol server, configured for us-texas-1
// and demo against api.
func listServer(t *testing.T, api string) (tfprotov6.ProviderServer, *tfprotov6.GetProviderSchemaResponse) {
	t.Helper()
	ctx := context.Background()
	p := &aiProvider{version: "test", lookupEnv: envMap(saEnv(nil))}
	server, err := providerserver.NewProtocol6WithError(p)()
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := server.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	config := nullObjectWith(t, schemas.Provider.ValueType(), map[string]string{
		"endpoint": api, "region": "us-texas-1", "project": "demo",
	})
	configured, err := server.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: config})
	if err != nil || len(diagnosticErrors(configured.Diagnostics)) > 0 {
		t.Fatalf("configure: %v %v", err, diagnosticErrors(configured.Diagnostics))
	}
	return server, schemas
}

// listConfigValue is a list block's config with the named values set and
// everything else null.
func listConfigValue(t *testing.T, schema *tfprotov6.Schema, set map[string]any) *tfprotov6.DynamicValue {
	t.Helper()
	if schema == nil {
		t.Fatal("no list resource schema")
	}
	obj, ok := schema.ValueType().(tftypes.Object)
	if !ok {
		t.Fatal("the list schema isn't an object")
	}
	values := map[string]tftypes.Value{}
	for name, at := range obj.AttributeTypes {
		values[name] = tftypes.NewValue(at, set[name])
	}
	for name := range set {
		if _, ok := obj.AttributeTypes[name]; !ok {
			t.Fatalf("the list schema has no %q", name)
		}
	}
	dv, err := tfprotov6.NewDynamicValue(obj, tftypes.NewValue(obj, values))
	if err != nil {
		t.Fatal(err)
	}
	return &dv
}

// listedResult is one result as a test reads it.
type listedResult struct {
	display  string
	identity map[string]string
	resource tftypes.Value
}

func (r listedResult) key() string {
	keys := make([]string, 0, len(r.identity))
	for k := range r.identity {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+r.identity[k])
	}
	return strings.Join(parts, " ")
}

// runList runs one list block and returns its results and every diagnostic.
func runList(t *testing.T, api, suffix string, set map[string]any, include bool, limit int64) ([]listedResult, []*tfprotov6.Diagnostic) {
	t.Helper()
	ctx := context.Background()
	server, schemas := listServer(t, api)
	typeName := ResourceName(suffix)
	lists, ok := server.(tfprotov6.ProviderServerWithListResource) //nolint:staticcheck // the only way terraform-plugin-go exposes it
	if !ok {
		t.Fatal("the protocol server serves no list resources")
	}
	stream, err := lists.ListResource(ctx, &tfprotov6.ListResourceRequest{
		TypeName:        typeName,
		Config:          listConfigValue(t, schemas.ListResourceSchemas[typeName], set),
		IncludeResource: include,
		Limit:           limit,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := identityType(t, server, typeName)
	var (
		out   []listedResult
		diags []*tfprotov6.Diagnostic
	)
	for result := range stream.Results {
		diags = append(diags, result.Diagnostics...)
		if result.Identity == nil {
			continue
		}
		got := listedResult{display: result.DisplayName, identity: map[string]string{}}
		v, err := result.Identity.IdentityData.Unmarshal(identity)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]tftypes.Value
		if err = v.As(&fields); err != nil {
			t.Fatal(err)
		}
		for name, f := range fields {
			var s string
			if err = f.As(&s); err != nil {
				t.Fatal(err)
			}
			got.identity[name] = s
		}
		if result.Resource != nil {
			got.resource, err = result.Resource.Unmarshal(schemas.ResourceSchemas[typeName].ValueType())
			if err != nil {
				t.Fatal(err)
			}
		}
		out = append(out, got)
	}
	return out, diags
}

func scopedID(id string, parents ...string) map[string]string {
	out := map[string]string{"region": "us-texas-1", "project": "demo", "id": id}
	for i := 0; i+1 < len(parents); i += 2 {
		out[parents[i]] = parents[i+1]
	}
	return out
}

// listCase is one list resource's search: the routes it reads, what its
// config sets, and what it should find, in order.
type listCase struct {
	name     string
	suffix   string
	set      map[string]any
	routes   func(t *testing.T) map[string]http.HandlerFunc
	want     []map[string]string
	displays []string
}

const (
	projectPath = "/v1/regions/us-texas-1/projects/demo"
	orgPath     = "/v1/regions/global/organizations/o1"
)

func listCases() []listCase {
	evpn := func(id string, extra map[string]any) map[string]any {
		out := map[string]any{"id": id, "type": "evpn", "state": "active"}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	return []listCase{
		{
			name: "every cluster but one being deleted", suffix: clusterResourceSuffix,
			routes: func(t *testing.T) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{"GET " + projectPath + "/compute/clusters": paged(t, "clusters", false,
					map[string]any{"id": "train", "displayName": "Training"},
					map[string]any{"id": "infer", "displayName": "infer"},
					map[string]any{"id": "old", "state": "deleting"})}
			},
			want:     []map[string]string{scopedID("infer"), scopedID("train")},
			displays: []string{"Training (train)", "infer"},
		},
		{
			name: "every instance group", suffix: instanceGroupResourceSuffix,
			routes: func(t *testing.T) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{"GET " + projectPath + "/compute/instance-groups": paged(t, "instanceGroups", false,
					map[string]any{"id": "hosts"}, map[string]any{"id": "gone", "state": "deleted"})}
			},
			want: []map[string]string{scopedID("hosts")},
		},
		{
			name: "every network a tenant made", suffix: networkResourceSuffix,
			routes: func(t *testing.T) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{"GET " + projectPath + "/networking/networks": paged(t, "networks", true,
					evpn("fabric", nil), evpn("cluster-net", map[string]any{"clusterId": "train"}))}
			},
			want: []map[string]string{scopedID("fabric")},
		},
		{
			name: "every network, the platform's too", suffix: networkResourceSuffix,
			set: map[string]any{"include_platform_owned": true},
			routes: func(t *testing.T) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{"GET " + projectPath + "/networking/networks": paged(t, "networks", true,
					evpn("fabric", nil), evpn("cluster-net", map[string]any{"clusterId": "train"}))}
			},
			want: []map[string]string{scopedID("cluster-net"), scopedID("fabric")},
		},
		{
			name: "every subnet on every network", suffix: subnetResourceSuffix,
			routes: func(t *testing.T) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					"GET " + projectPath + "/networking/networks": paged(t, "networks", true, evpn("n1", nil), evpn("n2", nil)),
					"GET " + projectPath + "/networking/networks/n1/subnets": paged(t, "subnets", true,
						map[string]any{"id": "s1"}, map[string]any{"id": "s-cluster", "clusterId": "train"}),
					"GET " + projectPath + "/networking/networks/n2/subnets": paged(t, "subnets", true, map[string]any{"id": "s2"}),
				}
			},
			want: []map[string]string{scopedID("s1", "network", "n1"), scopedID("s2", "network", "n2")},
		},
		{
			name: "every subnet on one network", suffix: subnetResourceSuffix,
			set: map[string]any{"network": "n2"},
			routes: func(t *testing.T) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					"GET " + projectPath + "/networking/networks/n2/subnets": paged(t, "subnets", true, map[string]any{"id": "s2"}),
				}
			},
			want: []map[string]string{scopedID("s2", "network", "n2")},
		},
		{
			name: "every security group a tenant made", suffix: securityGroupResourceSuffix,
			routes: func(t *testing.T) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{"GET " + projectPath + "/networking/security-groups": paged(t, "securityGroups", true,
					map[string]any{"id": "office"}, map[string]any{"id": "vpc-default", "ownerKind": "vpc"})}
			},
			want: []map[string]string{scopedID("office")},
		},
		{
			name: "every peering on every VPC", suffix: vpcPeeringResourceSuffix,
			routes: func(t *testing.T) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{
					"GET " + projectPath + "/networking/vpcs": paged(t, "vpcs", true,
						map[string]any{"id": "vpc-a"}, map[string]any{"id": "vpc-b", "state": "deleting"}),
					"GET " + projectPath + "/networking/vpcs/vpc-a/peerings": paged(t, "peerings", false,
						map[string]any{"id": "to-b"}, map[string]any{"id": "to-c"}),
				}
			},
			want: []map[string]string{scopedID("to-b", "vpc", "vpc-a"), scopedID("to-c", "vpc", "vpc-a")},
		},
		{
			name: "every VPC's binding", suffix: vpcSecurityGroupsResourceSuffix,
			routes: func(t *testing.T) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{"GET " + projectPath + "/networking/vpcs": paged(t, "vpcs", true,
					map[string]any{"id": "vpc-a"}, map[string]any{"id": "vpc-b"})}
			},
			want: []map[string]string{
				{"region": "us-texas-1", "project": "demo", "vpc": "vpc-a"},
				{"region": "us-texas-1", "project": "demo", "vpc": "vpc-b"},
			},
		},
		{
			name: "every file store, in another project", suffix: fileStoreResourceSuffix,
			set: map[string]any{"project": "other"},
			routes: func(t *testing.T) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{"GET /v1/regions/us-texas-1/projects/other/storage/file-stores": paged(t, "fileStores", false,
					map[string]any{"id": "datasets"}, map[string]any{"id": "scratch"})}
			},
			want: []map[string]string{
				{"region": "us-texas-1", "project": "other", "id": "datasets"},
				{"region": "us-texas-1", "project": "other", "id": "scratch"},
			},
		},
		{
			name: "every project", suffix: projectResourceSuffix,
			routes: func(t *testing.T) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{"GET /v1/regions/global/projects": paged(t, "projects", true,
					map[string]any{"id": "demo", "organizationId": "o1", "displayName": "Demo"},
					map[string]any{"id": "demo", "organizationId": "o2"},
					map[string]any{"id": "old", "organizationId": "o1", "status": "deleting"})}
			},
			want:     []map[string]string{{"organization_id": "o1", "id": "demo"}, {"organization_id": "o2", "id": "demo"}},
			displays: []string{"Demo (demo)", "demo"},
		},
		{
			name: "every webhook subscription", suffix: webhookSubscriptionResourceSuffix,
			routes: func(t *testing.T) map[string]http.HandlerFunc {
				return map[string]http.HandlerFunc{"GET /v1/regions/global/notifications/webhooks": func(w http.ResponseWriter, _ *http.Request) {
					writeTestJSON(t, w, http.StatusOK, map[string]any{"subscriptions": []any{
						map[string]any{"id": "6f1c2a8e-4d3b-4c5a-9e7f-0a1b2c3d4e5f", "url": "https://hooks.example.com/k0rdent"},
					}})
				}}
			},
			want: []map[string]string{{"id": "6f1c2a8e-4d3b-4c5a-9e7f-0a1b2c3d4e5f"}},
			// The id, never the URL: a receiver URL can carry a token.
			displays: []string{"6f1c2a8e-4d3b-4c5a-9e7f-0a1b2c3d4e5f"},
		},
	}
}

// Each list resource finds what its list operation returns, every page of it,
// named by its managed resource's identity, and leaves out what it should.
func TestEveryListResourceFindsWhatItsOperationReturns(t *testing.T) {
	t.Parallel()
	covered := map[string]bool{}
	for _, tc := range listCases() {
		covered[tc.suffix] = true
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			api := listAPI(t, tc.routes(t))
			got, diags := runList(t, api.URL, tc.suffix, tc.set, false, 100)
			if errs := diagnosticErrors(diags); len(errs) > 0 {
				t.Fatalf("the list failed: %v", errs)
			}
			var keys, displays []string
			for _, r := range got {
				keys = append(keys, r.key())
				displays = append(displays, r.display)
				if r.resource.Type() != nil {
					t.Errorf("%s carries the resource, which nothing asked for", r.key())
				}
			}
			var want []string
			for _, id := range tc.want {
				want = append(want, listedResult{identity: id}.key())
			}
			sort.Strings(keys)
			sort.Strings(want)
			if !slices.Equal(keys, want) {
				t.Errorf("found\n  %v\nwant\n  %v", keys, want)
			}
			if tc.displays != nil {
				sort.Strings(displays)
				wantDisplays := slices.Clone(tc.displays)
				sort.Strings(wantDisplays)
				if !slices.Equal(displays, wantDisplays) {
					t.Errorf("display names %q, want %q", displays, wantDisplays)
				}
			}
		})
	}
	for _, s := range listSpecs() {
		if !covered[s.suffix] {
			t.Errorf("no case lists %s", ResourceName(s.suffix))
		}
	}
}

// Every managed resource has a list resource of the same name, so
// `terraform query` can adopt all of them. A resource added without one fails
// here: its platform type either has a public list operation, and then it
// needs the list resource, or it hasn't, and then this is where to say so.
func TestEveryResourceHasAListResource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	p := New("test")()
	var resources, lists []string
	for _, f := range p.Resources(ctx) {
		var md resource.MetadataResponse
		f().Metadata(ctx, resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
		resources = append(resources, md.TypeName)
	}
	withLists, ok := p.(fwprovider.ProviderWithListResources)
	if !ok {
		t.Fatal("the provider registers no list resources")
	}
	for _, f := range withLists.ListResources(ctx) {
		var md resource.MetadataResponse
		f().Metadata(ctx, resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
		lists = append(lists, md.TypeName)
	}
	if !slices.Equal(resources, lists) {
		t.Errorf("the list resources are\n  %v\nand the managed resources\n  %v", lists, resources)
	}
	for _, s := range listSpecs() {
		if _, ok := s.newResource().(resource.ResourceWithImportState); !ok {
			t.Errorf("%s has a list resource and no import, so include_resource can't read it", ResourceName(s.suffix))
		}
	}
}

// securityGroupAPI lists two groups and reads each. gone, when set, is a
// group the list names and the read then finds deleted.
func securityGroupAPI(t *testing.T, gone string) *httptest.Server {
	t.Helper()
	return listAPI(t, map[string]http.HandlerFunc{
		"GET " + projectPath + "/networking/security-groups": paged(t, "securityGroups", true,
			map[string]any{"id": "office"}, map[string]any{"id": "lab"}),
		"GET " + projectPath + "/networking/security-groups/{id}": func(w http.ResponseWriter, r *http.Request) {
			id := r.PathValue("id")
			if id == gone {
				writeTestJSON(t, w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404, "message": "not found"}})
				return
			}
			writeTestJSON(t, w, http.StatusOK, map[string]any{
				"id": id, "displayName": "Group " + id, "state": "active", "tags": map[string]any{"team": "ml"},
				"rules": map[string]any{
					"ingress": []any{map[string]any{"name": "ssh", "protocol": "tcp", "action": "permit",
						"sourcePrefix": "10.0.0.0/8", "destinationPrefix": "0.0.0.0/0", "destinationPortRange": "22"}},
					"egress": []any{},
				},
			})
		},
	})
}

// With include_resource, each result carries the object as an `import` block
// naming the same identity lands it in state. That's the guarantee the
// generated configuration rests on, so the two are compared value for value.
func TestIncludeResourceIsWhatAnImportReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	api := securityGroupAPI(t, "")
	got, diags := runList(t, api.URL, securityGroupResourceSuffix, nil, true, 100)
	if errs := diagnosticErrors(diags); len(errs) > 0 {
		t.Fatalf("the list failed: %v", errs)
	}
	if len(got) != 2 {
		t.Fatalf("found %d groups, want 2", len(got))
	}

	server, schemas := listServer(t, api.URL)
	typeName := ResourceName(securityGroupResourceSuffix)
	stateType := schemas.ResourceSchemas[typeName].ValueType()
	for _, listed := range got {
		if listed.resource.IsNull() || !listed.resource.IsKnown() {
			t.Fatalf("%s carries no resource", listed.key())
		}
		identity, err := tfprotov6.NewDynamicValue(identityType(t, server, typeName), identityValue(t, server, typeName, listed.identity))
		if err != nil {
			t.Fatal(err)
		}
		imported, err := server.ImportResourceState(ctx, &tfprotov6.ImportResourceStateRequest{
			TypeName: typeName, Identity: &tfprotov6.ResourceIdentityData{IdentityData: &identity},
		})
		if err != nil || len(diagnosticErrors(imported.Diagnostics)) > 0 || len(imported.ImportedResources) != 1 {
			t.Fatalf("import: %v %v", err, diagnosticErrors(imported.Diagnostics))
		}
		read, err := server.ReadResource(ctx, &tfprotov6.ReadResourceRequest{
			TypeName: typeName, CurrentState: imported.ImportedResources[0].State,
			CurrentIdentity: imported.ImportedResources[0].Identity, Private: imported.ImportedResources[0].Private,
		})
		if err != nil || len(diagnosticErrors(read.Diagnostics)) > 0 {
			t.Fatalf("read: %v %v", err, diagnosticErrors(read.Diagnostics))
		}
		want, err := read.NewState.Unmarshal(stateType)
		if err != nil {
			t.Fatal(err)
		}
		if diffs, err := listed.resource.Diff(want); err != nil || len(diffs) > 0 {
			t.Errorf("%s: the listed object and the imported one differ: %v %v", listed.key(), err, diffs)
		}
		var attrs map[string]tftypes.Value
		if err := listed.resource.As(&attrs); err != nil {
			t.Fatal(err)
		}
		var display string
		if err := attrs["display_name"].As(&display); err != nil || display != "Group "+listed.identity["id"] {
			t.Errorf("%s: display_name %q, which isn't what the read answered", listed.key(), display)
		}
		var ingress []tftypes.Value
		if err := attrs["ingress"].As(&ingress); err != nil || len(ingress) != 1 {
			t.Errorf("%s: %d ingress rules, want the one the read answered: %v", listed.key(), len(ingress), err)
		}
	}
}

// identityValue is an identity object with the named strings set.
func identityValue(t *testing.T, server tfprotov6.ProviderServer, typeName string, set map[string]string) tftypes.Value {
	t.Helper()
	obj, ok := identityType(t, server, typeName).(tftypes.Object)
	if !ok {
		t.Fatal("the identity isn't an object")
	}
	values := map[string]tftypes.Value{}
	for name, at := range obj.AttributeTypes {
		if v, ok := set[name]; ok {
			values[name] = tftypes.NewValue(at, v)
			continue
		}
		values[name] = tftypes.NewValue(at, nil)
	}
	return tftypes.NewValue(obj, values)
}

// An object deleted between the list and its read is left out, rather than
// offered for an import that would fail.
func TestAnObjectGoneBeforeItsReadIsLeftOut(t *testing.T) {
	t.Parallel()
	got, diags := runList(t, securityGroupAPI(t, "lab").URL, securityGroupResourceSuffix, nil, true, 100)
	if errs := diagnosticErrors(diags); len(errs) > 0 {
		t.Fatalf("the list failed: %v", errs)
	}
	if len(got) != 1 || got[0].identity["id"] != "office" {
		t.Errorf("found %v, want office alone", got)
	}
}

// The `limit` a list block sets, 100 by default, caps the results, counted
// after an object gone before its read is dropped, so a vanished one doesn't
// cost a place. With include_resource it caps the reads too, and a list cut
// short says so.
func TestAListStopsAtItsLimit(t *testing.T) {
	t.Parallel()
	var reads atomic.Int32
	api := listAPI(t, map[string]http.HandlerFunc{
		"GET " + projectPath + "/networking/security-groups": paged(t, "securityGroups", true,
			map[string]any{"id": "a"}, map[string]any{"id": "b"}, map[string]any{"id": "c"}, map[string]any{"id": "d"}),
		"GET " + projectPath + "/networking/security-groups/{id}": func(w http.ResponseWriter, r *http.Request) {
			reads.Add(1)
			if r.PathValue("id") == "a" {
				writeTestJSON(t, w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404, "message": "not found"}})
				return
			}
			writeTestJSON(t, w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "state": "active", "rules": map[string]any{"ingress": []any{}, "egress": []any{}}})
		},
	})
	got, diags := runList(t, api.URL, securityGroupResourceSuffix, nil, true, 2)
	if errs := diagnosticErrors(diags); len(errs) > 0 {
		t.Fatalf("the list failed: %v", errs)
	}
	if len(got) != 2 || got[0].identity["id"] != "b" || got[1].identity["id"] != "c" {
		t.Errorf("found %v, want b and c", got)
	}
	if n := reads.Load(); n != 3 {
		t.Errorf("read %d groups, want 3: a, which is gone, then b and c", n)
	}
	if !warned(diags, "limit") {
		t.Error("nothing said the list stopped at its limit")
	}

	// A list that fits its limit says nothing about it.
	_, diags = runList(t, api.URL, securityGroupResourceSuffix, nil, false, 4)
	if warned(diags, "limit") {
		t.Error("a list that fit its limit warned about it")
	}
}

// warned is whether any warning's summary or detail mentions want.
func warned(diags []*tfprotov6.Diagnostic, want string) bool {
	for _, d := range diags {
		if d.Severity == tfprotov6.DiagnosticSeverityWarning && strings.Contains(d.Summary+" "+d.Detail, want) {
			return true
		}
	}
	return false
}

// A read that fails for one object names that object in a warning and the
// rest of the list still comes back. One object the credential can't read
// shouldn't hide a project's worth of others.
func TestAFailedReadIsNamedAndTheRestListed(t *testing.T) {
	t.Parallel()
	api := listAPI(t, map[string]http.HandlerFunc{
		"GET " + projectPath + "/networking/security-groups": paged(t, "securityGroups", true,
			map[string]any{"id": "office"}, map[string]any{"id": "locked"}, map[string]any{"id": "lab"}),
		"GET " + projectPath + "/networking/security-groups/{id}": func(w http.ResponseWriter, r *http.Request) {
			if r.PathValue("id") == "locked" {
				writeTestJSON(t, w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": 403, "message": "no"}})
				return
			}
			writeTestJSON(t, w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "state": "active", "rules": map[string]any{"ingress": []any{}, "egress": []any{}}})
		},
	})
	got, diags := runList(t, api.URL, securityGroupResourceSuffix, nil, true, 100)
	if errs := diagnosticErrors(diags); len(errs) > 0 {
		t.Fatalf("one failed read failed the list: %v", errs)
	}
	var ids []string
	for _, r := range got {
		ids = append(ids, r.identity["id"])
	}
	if !slices.Equal(ids, []string{"lab", "office"}) {
		t.Errorf("found %v, want lab and office", ids)
	}
	if !warned(diags, "locked") {
		t.Error("nothing named the group that couldn't be read")
	}

	// It doesn't take a place under the limit either: lab, then locked, which
	// is left out, then office.
	got, _ = runList(t, api.URL, securityGroupResourceSuffix, nil, true, 2)
	if len(got) != 2 || got[1].identity["id"] != "office" {
		t.Errorf("under a limit of 2 found %v, want lab and office", got)
	}
}

// A walk over every network or VPC skips one being deleted, and one gone
// between the parent list and its own is named and skipped rather than
// failing the rest. The service account walk skips a project being deleted.
func TestAWalkSkipsParentsGoingAwayOrGone(t *testing.T) {
	t.Parallel()
	refuse := func(what string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			t.Errorf("listed under %s, which is being deleted: %s", what, r.URL)
			writeTestJSON(t, w, http.StatusOK, map[string]any{})
		}
	}
	gone := func(w http.ResponseWriter, _ *http.Request) {
		writeTestJSON(t, w, http.StatusNotFound, map[string]any{"error": map[string]any{"code": 404, "message": "not found"}})
	}
	t.Run("subnets", func(t *testing.T) {
		t.Parallel()
		api := listAPI(t, map[string]http.HandlerFunc{
			"GET " + projectPath + "/networking/networks": paged(t, "networks", true,
				map[string]any{"id": "n1", "type": "evpn"},
				map[string]any{"id": "n2", "type": "evpn", "state": "deleting"},
				map[string]any{"id": "n3", "type": "evpn"}),
			"GET " + projectPath + "/networking/networks/n1/subnets": paged(t, "subnets", true, map[string]any{"id": "s1"}),
			"GET " + projectPath + "/networking/networks/n2/subnets": refuse("n2"),
			"GET " + projectPath + "/networking/networks/n3/subnets": gone,
		})
		got, diags := runList(t, api.URL, subnetResourceSuffix, nil, false, 100)
		if errs := diagnosticErrors(diags); len(errs) > 0 {
			t.Fatalf("the list failed: %v", errs)
		}
		if len(got) != 1 || got[0].identity["id"] != "s1" {
			t.Errorf("found %v, want s1 alone", got)
		}
		if !warned(diags, "n3") {
			t.Error("nothing named the network that went away")
		}
	})
	t.Run("peerings", func(t *testing.T) {
		t.Parallel()
		api := listAPI(t, map[string]http.HandlerFunc{
			"GET " + projectPath + "/networking/vpcs": paged(t, "vpcs", true,
				map[string]any{"id": "vpc-a"}, map[string]any{"id": "vpc-b"}),
			"GET " + projectPath + "/networking/vpcs/vpc-a/peerings": gone,
			"GET " + projectPath + "/networking/vpcs/vpc-b/peerings": paged(t, "peerings", false, map[string]any{"id": "p"}),
		})
		got, diags := runList(t, api.URL, vpcPeeringResourceSuffix, nil, false, 100)
		if errs := diagnosticErrors(diags); len(errs) > 0 {
			t.Fatalf("the list failed: %v", errs)
		}
		if len(got) != 1 || got[0].identity["vpc"] != "vpc-b" {
			t.Errorf("found %v, want the peering on vpc-b", got)
		}
		if !warned(diags, "vpc-a") {
			t.Error("nothing named the VPC that went away")
		}
	})
}

// A list that fails says why, as the data source's would, and finds nothing.
func TestAFailedListSaysWhy(t *testing.T) {
	t.Parallel()
	api := listAPI(t, map[string]http.HandlerFunc{
		"GET " + projectPath + "/compute/clusters": func(w http.ResponseWriter, _ *http.Request) {
			writeTestJSON(t, w, http.StatusForbidden, map[string]any{"error": map[string]any{"code": 403, "message": "no"}})
		},
	})
	got, diags := runList(t, api.URL, clusterResourceSuffix, nil, false, 100)
	errs := diagnosticErrors(diags)
	if len(got) != 0 || len(errs) != 1 || !strings.Contains(errs[0], "list the clusters") {
		t.Errorf("found %v with errors %v, want nothing and the failed list named", got, errs)
	}
}

// A network of a kind this build doesn't know is named in a warning and left
// out. It can't be imported either, and the rest of the project still can.
func TestANetworkOfAnUnknownKindIsNamedAndLeftOut(t *testing.T) {
	t.Parallel()
	api := listAPI(t, map[string]http.HandlerFunc{
		"GET " + projectPath + "/networking/networks": paged(t, "networks", true,
			map[string]any{"id": "fabric", "type": "evpn"}, map[string]any{"id": "q", "type": "quantum"}),
	})
	got, diags := runList(t, api.URL, networkResourceSuffix, nil, false, 100)
	if errs := diagnosticErrors(diags); len(errs) > 0 {
		t.Fatalf("the list failed: %v", errs)
	}
	if len(got) != 1 || got[0].identity["id"] != "fabric" {
		t.Errorf("found %v, want fabric alone", got)
	}
	var warned bool
	for _, d := range diags {
		warned = warned || (d.Severity == tfprotov6.DiagnosticSeverityWarning && strings.Contains(d.Detail, "quantum"))
	}
	if !warned {
		t.Error("nothing named the network left out")
	}
}
