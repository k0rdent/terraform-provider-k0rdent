package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// groupsServer stands in for the compute API around one cluster. It keeps
// every mutating body it was sent, answers a PATCH that carries only
// `securityGroups` with 202 as the platform does, and a POST with 201. Reads
// always find the cluster active.
type groupsServer struct {
	mu   sync.Mutex
	sent []map[string]any
}

func (g *groupsServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	cluster := map[string]any{
		"id": "c1", "clusterType": "/v1/regions/test/compute/cluster-types/test",
		"nodePools": []any{}, "nodeCount": 0, "state": "active", "securityGroups": []any{},
		"selfLink":  "/v1/regions/test/projects/test/compute/clusters/c1",
		"uid":       "00000000-0000-4000-8000-000000000009",
		"createdAt": "2026-09-22T00:00:00Z", "updatedAt": "2026-09-22T00:00:00Z",
		"health": map[string]string{"state": "unknown"},
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status := http.StatusOK
		if r.Method == http.MethodPatch || r.Method == http.MethodPost {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read %s: %v", r.Method, err)
			}
			body := map[string]any{}
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("%s body: %v", r.Method, err)
			}
			g.mu.Lock()
			g.sent = append(g.sent, body)
			g.mu.Unlock()
			_, groups := body["securityGroups"]
			switch {
			case r.Method == http.MethodPost:
				status = http.StatusCreated
			case groups && len(body) > 1:
				// What the platform answers, measured on v1.3.0-dev.3.
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				if _, err := io.WriteString(w, `{"error":{"code":422,"message":"validation: securityGroups `+
					`cannot be combined with other fields in one PATCH; send it on its own"}}`); err != nil {
					t.Errorf("write: %v", err)
				}
				return
			case groups:
				status = http.StatusAccepted
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if err := json.NewEncoder(w).Encode(cluster); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (g *groupsServer) bodies() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]map[string]any(nil), g.sent...)
}

func stringList(items ...string) tftypes.Value {
	values := make([]tftypes.Value, 0, len(items))
	for _, s := range items {
		values = append(values, str(s))
	}
	return tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, values)
}

func unknownList() tftypes.Value {
	return tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, tftypes.UnknownValue)
}

// clusterValues is the minimum an existing cluster carries, plus set.
func existingCluster(set map[string]tftypes.Value) map[string]tftypes.Value {
	out := map[string]tftypes.Value{
		"id": str("c1"), "cluster_type": str("/v1/regions/test/compute/cluster-types/test"),
		"region": str("test"), "project": str("test"),
	}
	for k, v := range set {
		out[k] = v
	}
	return out
}

func runClusterUpdate(t *testing.T, state, plan map[string]tftypes.Value) []map[string]any {
	t.Helper()
	var g groupsServer
	server := g.start(t)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	stateRaw, s := testValue(t, r, existingCluster(state))
	planRaw, _ := testValue(t, r, existingCluster(plan))

	resp := resource.UpdateResponse{State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}
	r.Update(context.Background(), resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics.Errors())
	}
	return g.bodies()
}

// The platform refuses `securityGroups` beside any other field, so a change
// to both goes as two PATCHes: the metadata first, then the groups alone, in
// the configured order.
func TestClusterUpdateSendsSecurityGroupsOnTheirOwn(t *testing.T) {
	sent := runClusterUpdate(t,
		map[string]tftypes.Value{"display_name": str("old"), "security_groups": stringList("a")},
		map[string]tftypes.Value{"display_name": str("new"), "security_groups": stringList("b", "a")},
	)
	if len(sent) != 2 {
		t.Fatalf("sent %d mutating requests, want 2: %v", len(sent), sent)
	}
	if _, ok := sent[0]["securityGroups"]; ok || sent[0]["displayName"] != "new" {
		t.Errorf("first PATCH = %v, want the display name and no securityGroups", sent[0])
	}
	want := map[string]any{"securityGroups": []any{"b", "a"}}
	if !reflect.DeepEqual(sent[1], want) {
		t.Errorf("second PATCH = %v, want %v", sent[1], want)
	}
}

// `[]` is the one way to detach, so it has to reach the wire as an empty
// array, not be dropped as "nothing to send".
func TestClusterUpdateSendsAnEmptyListToDetach(t *testing.T) {
	sent := runClusterUpdate(t,
		map[string]tftypes.Value{"security_groups": stringList("a", "b")},
		map[string]tftypes.Value{"security_groups": stringList()},
	)
	want := []map[string]any{{"securityGroups": []any{}}}
	if !reflect.DeepEqual(sent, want) {
		t.Errorf("sent %v, want %v", sent, want)
	}
}

// Left out of the configuration, the attribute plans as the state value, or
// as unknown when state has none. Neither asks for a change, so no PATCH
// carries the field and groups attached elsewhere stay attached.
func TestClusterUpdateLeavesAnUnsetBindingAlone(t *testing.T) {
	for name, planned := range map[string]tftypes.Value{
		"the state value": stringList("a"),
		"unknown":         unknownList(),
	} {
		t.Run(name, func(t *testing.T) {
			sent := runClusterUpdate(t,
				map[string]tftypes.Value{"display_name": str("old"), "security_groups": stringList("a")},
				map[string]tftypes.Value{"display_name": str("new"), "security_groups": planned},
			)
			for _, body := range sent {
				if _, ok := body["securityGroups"]; ok {
					t.Errorf("sent %v, want no securityGroups", body)
				}
			}
		})
	}
}

func TestSecurityGroupsChanged(t *testing.T) {
	list := func(items ...string) types.List {
		// A nil slice would map to a null list, which is "unset", not `[]`.
		v, _ := types.ListValueFrom(context.Background(), types.StringType, append([]string{}, items...))
		return v
	}
	for name, tt := range map[string]struct {
		plan, state types.List
		want        bool
	}{
		"unset":       {types.ListNull(types.StringType), list("a"), false},
		"unknown":     {types.ListUnknown(types.StringType), list("a"), false},
		"same":        {list("a", "b"), list("a", "b"), false},
		"reordered":   {list("b", "a"), list("a", "b"), true},
		"swapped":     {list("a", "c"), list("a", "b"), true},
		"emptied":     {list(), list("a"), true},
		"empty again": {list(), list(), false},
	} {
		t.Run(name, func(t *testing.T) {
			got := securityGroupsChanged(clusterModel{SecurityGroups: tt.plan}, clusterModel{SecurityGroups: tt.state})
			if got != tt.want {
				t.Errorf("securityGroupsChanged = %v, want %v", got, tt.want)
			}
		})
	}
}

// A create sends a list with entries and nothing else: `[]` and an unset
// attribute both leave the field out, which the platform reads as no groups.
func TestClusterCreateSendsOnlyAListWithEntries(t *testing.T) {
	for name, tt := range map[string]struct {
		planned tftypes.Value
		want    any
	}{
		"two groups, in order": {stringList("b", "a"), []any{"b", "a"}},
		"an empty list":        {stringList(), nil},
		"unset":                {unknownList(), nil},
	} {
		t.Run(name, func(t *testing.T) {
			var g groupsServer
			server := g.start(t)
			r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
			planRaw, s := testValue(t, r, existingCluster(map[string]tftypes.Value{
				"id": str("c1"), "security_groups": tt.planned,
			}))
			resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(planRaw.Type(), nil), Schema: s.Schema}}
			r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("create: %v", resp.Diagnostics.Errors())
			}
			sent := g.bodies()
			if len(sent) != 1 {
				t.Fatalf("sent %d mutating requests, want 1: %v", len(sent), sent)
			}
			got, ok := sent[0]["securityGroups"]
			if tt.want == nil && ok {
				t.Errorf("create carried securityGroups = %v, want it left out", got)
			}
			if tt.want != nil && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("create carried securityGroups = %v, want %v", got, tt.want)
			}
		})
	}
}

// The read is the platform's list in the platform's order, and an omitted
// field reads as a known empty list. A null would plan as "known after
// apply" forever on a Computed attribute, and would not match a configured `[]`.
func TestClusterStateFromReadsSecurityGroupsInOrder(t *testing.T) {
	for name, tt := range map[string]struct {
		got  []string
		want []string
	}{
		"in the platform's order": {[]string{"b", "a"}, []string{"b", "a"}},
		"none":                    {[]string{}, []string{}},
		"omitted":                 {nil, []string{}},
	} {
		t.Run(name, func(t *testing.T) {
			out, diags := clusterStateFrom(context.Background(),
				k0rdent.Cluster{Id: "c1", SecurityGroups: tt.got}, testClusterPriorState(), defaultTags{})
			if diags.HasError() {
				t.Fatalf("map: %v", diags.Errors())
			}
			if out.SecurityGroups.IsNull() || out.SecurityGroups.IsUnknown() {
				t.Fatalf("security_groups = %v, want a known list", out.SecurityGroups)
			}
			var have []string
			out.SecurityGroups.ElementsAs(context.Background(), &have, false)
			if len(have) != len(tt.want) || (len(have) > 0 && !reflect.DeepEqual(have, tt.want)) {
				t.Errorf("security_groups = %v, want %v", have, tt.want)
			}
		})
	}
}

// Each validator is a 422 the platform answers, caught at plan time instead.
func TestClusterSecurityGroupsValidators(t *testing.T) {
	many := make([]string, 17)
	for i := range many {
		many[i] = "g" + strconv.Itoa(i)
	}
	for name, tt := range map[string]struct {
		items []string
		ok    bool
	}{
		"two groups":       {[]string{"a", "b-2"}, true},
		"sixteen":          {many[:16], true},
		"seventeen":        {many, false},
		"a duplicate":      {[]string{"a", "a"}, false},
		"not an id":        {[]string{"Bad_Id"}, false},
		"a trailing dash":  {[]string{"a-"}, false},
		"empty":            {[]string{}, true},
		"an empty element": {[]string{""}, false},
	} {
		t.Run(name, func(t *testing.T) {
			value, _ := types.ListValueFrom(context.Background(), types.StringType, tt.items)
			attr := clusterSecurityGroupsAttribute()
			var failed bool
			for _, v := range attr.Validators {
				resp := validator.ListResponse{}
				v.ValidateList(context.Background(), validator.ListRequest{
					Path: path.Root("security_groups"), ConfigValue: value,
				}, &resp)
				failed = failed || resp.Diagnostics.HasError()
			}
			if failed == tt.ok {
				t.Errorf("%v: refused = %v, want %v", tt.items, failed, !tt.ok)
			}
		})
	}
}

// A List, so order is a change, and Optional+Computed, so leaving it out is
// not the same as `[]`.
func TestClusterSecurityGroupsShape(t *testing.T) {
	attr := clusterSecurityGroupsAttribute()
	if !attr.Optional || !attr.Computed {
		t.Errorf("optional = %v, computed = %v, want both", attr.Optional, attr.Computed)
	}
	if attr.ElementType != types.StringType {
		t.Errorf("element type = %v, want string", attr.ElementType)
	}
	if len(attr.PlanModifiers) != 1 {
		t.Errorf("%d plan modifiers, want UseStateForUnknown alone, and never RequiresReplace", len(attr.PlanModifiers))
	}
}
