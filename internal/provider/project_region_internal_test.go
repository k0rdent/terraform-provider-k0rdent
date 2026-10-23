package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

// v1.3.0-dev.4 locks every project to one region, given at create, and
// makes `region` required on a project create. These pin how the project
// resource fills it, plans it and reads it back.

// regionsAPI answers listRegions with regions, and the project create with the
// project it was asked for, keeping the body it was sent.
type regionsAPI struct {
	mu      sync.Mutex
	regions []map[string]any // nil answers listRegions with a 500
	created map[string]any
	lists   int
}

func (a *regionsAPI) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/regions":
			a.lists++
			if a.regions == nil {
				writeTestError(t, w, http.StatusInternalServerError, "regions are unavailable")
				return
			}
			writeJSON(w, map[string]any{"regions": a.regions})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/projects"):
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read the create body: %v", err)
			}
			a.created = map[string]any{}
			if err := json.Unmarshal(raw, &a.created); err != nil {
				t.Errorf("create body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			body := projectBody("active", "", "")
			body["id"], body["region"] = a.created["id"], a.created["region"]
			body["uid"] = "00000000-0000-4000-8000-00000000000c"
			if err := json.NewEncoder(w).Encode(body); err != nil {
				t.Errorf("write: %v", err)
			}
		default:
			writeTestError(t, w, http.StatusNotFound, "not found")
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func region(id string, hosts bool) map[string]any {
	kind := "regional"
	if id == "global" {
		kind = "global"
	}
	return map[string]any{"id": id, "type": kind, "hostsProjects": hosts}
}

// planProject plans a project. A nil state is a create. Unset computed
// attributes are unknown in the plan, as Terraform proposes them.
func planProject(t *testing.T, meta *Meta, config, state map[string]tftypes.Value) resource.ModifyPlanResponse {
	t.Helper()
	r := &projectResource{meta: meta}
	base := map[string]tftypes.Value{"organization_id": str("org-ml")}
	merged := func(over map[string]tftypes.Value) map[string]tftypes.Value {
		out := map[string]tftypes.Value{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range over {
			out[k] = v
		}
		return out
	}
	configRaw, s := testValue(t, r, merged(config))
	planned := merged(config)
	for _, computed := range []string{"id", "region", "status", "self_link"} {
		if _, set := config[computed]; !set {
			planned[computed] = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)
			if state != nil && computed != "region" {
				planned[computed] = state[computed]
			}
		}
	}
	planRaw, _ := testValue(t, r, planned)
	stateRaw := tftypes.NewValue(configRaw.Type(), nil)
	if state != nil {
		stateRaw, _ = testValue(t, r, merged(state))
	}
	plan := tfsdk.Plan{Raw: planRaw, Schema: s.Schema}
	resp := resource.ModifyPlanResponse{Plan: plan}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema},
		Plan:   plan,
		State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
	}, &resp)
	return resp
}

func plannedRegion(t *testing.T, resp resource.ModifyPlanResponse) types.String {
	t.Helper()
	var got types.String
	if d := resp.Plan.GetAttribute(context.Background(), path.Root("region"), &got); d.HasError() {
		t.Fatal(d)
	}
	return got
}

func regionsMeta(t *testing.T, api *regionsAPI, providerRegion string) *Meta {
	t.Helper()
	meta := testClusterMeta(t, api.serve(t).URL, time.Minute)
	meta.Region = providerRegion
	return meta
}

// A project left without a region takes the provider's, the same default
// every regional resource has, and the plan shows it.
func TestAProjectDefaultsToTheProvidersRegion(t *testing.T) {
	api := &regionsAPI{regions: []map[string]any{region("global", false), region("local", true)}}
	resp := planProject(t, regionsMeta(t, api, "local"), nil, nil)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if got := plannedRegion(t, resp); got.ValueString() != "local" {
		t.Errorf("region = %s, want local", got)
	}
}

// With no region anywhere, the plan says where to set one rather than letting
// the create get the platform's 422.
func TestAProjectWithNoRegionAnywhereIsRefusedAtPlan(t *testing.T) {
	api := &regionsAPI{regions: []map[string]any{region("local", true)}}
	resp := planProject(t, regionsMeta(t, api, ""), nil, nil)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a project with no region planned")
	}
	text := resp.Diagnostics.Errors()[0].Summary() + "\n" + resp.Diagnostics.Errors()[0].Detail()
	for _, want := range []string{"region", "provider", "hosts_projects", "local"} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostic does not say %q:\n%s", want, text)
		}
	}
}

// The contract takes only a region whose hostsProjects is true. A region the
// list says can't hold one is refused at plan, naming the ones that can.
func TestAProjectRegionThatHostsNoProjectsIsRefusedAtPlan(t *testing.T) {
	api := &regionsAPI{regions: []map[string]any{region("global", false), region("east", true), region("west", true)}}
	for _, asked := range []string{"global", "nowhere"} {
		resp := planProject(t, regionsMeta(t, api, ""), map[string]tftypes.Value{"region": str(asked)}, nil)
		if !resp.Diagnostics.HasError() {
			t.Fatalf("region %q planned", asked)
		}
		text := resp.Diagnostics.Errors()[0].Detail()
		for _, want := range []string{asked, "east", "west"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: diagnostic does not say %q:\n%s", asked, want, text)
			}
		}
	}

	// On an all-in-one install `global` runs compute and hosts projects.
	api.regions = []map[string]any{region("global", true)}
	resp := planProject(t, regionsMeta(t, api, "global"), nil, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("global refused where it hosts projects: %v", resp.Diagnostics)
	}
}

// The region list is only a better error than the platform's. When it can't
// be read the plan goes ahead and the create says what the platform says.
func TestAnUnreadableRegionListDoesNotStopThePlan(t *testing.T) {
	api := &regionsAPI{}
	resp := planProject(t, regionsMeta(t, api, "local"), nil, nil)
	if resp.Diagnostics.HasError() {
		t.Fatalf("plan failed on an unreadable region list: %v", resp.Diagnostics)
	}
	if got := plannedRegion(t, resp); got.ValueString() != "local" {
		t.Errorf("region = %s, want local", got)
	}
}

// A project can't move. A changed region replaces it, whether the change is
// in the resource or in the provider block it defaults from.
func TestAChangedProjectRegionReplacesIt(t *testing.T) {
	api := &regionsAPI{regions: []map[string]any{region("east", true), region("west", true)}}
	state := map[string]tftypes.Value{"id": str("cp-production"), "region": str("east"),
		"status": str("active"), "self_link": str("/p")}
	for name, tc := range map[string]struct {
		config   map[string]tftypes.Value
		provider string
	}{
		"in the resource":       {map[string]tftypes.Value{"id": str("cp-production"), "region": str("west")}, "east"},
		"in the provider block": {map[string]tftypes.Value{"id": str("cp-production")}, "west"},
	} {
		resp := planProject(t, regionsMeta(t, api, tc.provider), tc.config, state)
		if !resp.RequiresReplace.Contains(path.Root("region")) {
			t.Errorf("%s: requires replace = %v, want region", name, resp.RequiresReplace)
		}
	}
	resp := planProject(t, regionsMeta(t, api, "east"), map[string]tftypes.Value{"id": str("cp-production")}, state)
	if resp.RequiresReplace.Contains(path.Root("region")) {
		t.Error("an unchanged region replaces the project")
	}
}

// A project made before regions were required reads back with none. Its
// state keeps what it had, and the provider's region isn't planned onto it,
// which would be a change no apply can make.
func TestAProjectWithNoRegionOnThePlatformKeepsItsState(t *testing.T) {
	api := &regionsAPI{regions: []map[string]any{region("local", true)}}
	state := map[string]tftypes.Value{"id": str("cp-production"), "status": str("active"), "self_link": str("/p")}
	resp := planProject(t, regionsMeta(t, api, "local"), map[string]tftypes.Value{"id": str("cp-production")}, state)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if got := plannedRegion(t, resp); !got.IsNull() {
		t.Errorf("region = %s, want null kept from state", got)
	}
	if len(resp.RequiresReplace) != 0 {
		t.Errorf("requires replace = %v", resp.RequiresReplace)
	}

	prior := projectModel{Region: types.StringValue("east"), OrganizationID: types.StringValue("org-ml")}
	if got := projectStateFrom(k0rdent.Project{Id: ptr("cp-production")}, prior).Region; got.ValueString() != "east" {
		t.Errorf("a read with no region wrote %s over the prior east", got)
	}
	west := "west"
	if got := projectStateFrom(k0rdent.Project{Id: ptr("cp-production"), Region: &west}, prior).Region; got.ValueString() != "west" {
		t.Errorf("a read naming west wrote %s", got)
	}
}

// The create sends the planned region, which is what dev.4 requires.
func TestAProjectCreateSendsItsRegion(t *testing.T) {
	api := &regionsAPI{regions: []map[string]any{region("local", true)}}
	meta := regionsMeta(t, api, "local")
	r := &projectResource{meta: meta}
	planRaw, s := testValue(t, r, map[string]tftypes.Value{
		"id": str("cp-production"), "organization_id": str("org-ml"), "region": str("local"),
	})
	resp := resource.CreateResponse{State: tfsdk.State{Raw: planRaw, Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if got := api.created["region"]; got != "local" {
		t.Errorf("create sent region %v, want local", got)
	}
	if got := stateAttr(t, resp.State, "region"); got != "local" {
		t.Errorf("region in state = %q", got)
	}
}

// The contract's pattern for a region id. It's looser than a resource id at
// the start, since a region may begin with a digit.
func TestAProjectRegionIsCheckedAgainstTheContractsPattern(t *testing.T) {
	for _, tc := range []struct {
		v  string
		ok bool
	}{
		{"local", true}, {"us-east-1", true}, {"1a", true}, {"a", true},
		{"global", true}, {strings.Repeat("a", 63), true},
		{"", false}, {"-a", false}, {"a-", false}, {"A", false}, {"a_b", false},
		{"a.b", false}, {strings.Repeat("a", 64), false},
	} {
		got := validateString(t, regionIDValidators(), tc.v)
		if got != tc.ok {
			t.Errorf("region %q: accepted %v, want %v", tc.v, got, tc.ok)
		}
	}
}

func validateString(t *testing.T, validators []validator.String, v string) bool {
	t.Helper()
	for _, val := range validators {
		if len(runStringValidator(val, types.StringValue(v))) > 0 {
			return false
		}
	}
	return true
}

// A project the platform reports with no region can't be given one: the
// platform refuses a region on an update, and an update that sent nothing
// would read the null back and fail the apply with an inconsistent result,
// on every apply. So setting `region` on one is refused at plan.
func TestARegionSetOnAProjectWithNoneIsRefusedAtPlan(t *testing.T) {
	api := &regionsAPI{regions: []map[string]any{region("local", true)}}
	state := map[string]tftypes.Value{"id": str("cp-production"), "status": str("active"), "self_link": str("/p")}
	resp := planProject(t, regionsMeta(t, api, "local"),
		map[string]tftypes.Value{"id": str("cp-production"), "region": str("local")}, state)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a region set on a project with none planned")
	}
	text := resp.Diagnostics.Errors()[0].Summary() + "\n" + resp.Diagnostics.Errors()[0].Detail()
	for _, want := range []string{"no region", "remove `region`", "-refresh=false"} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostic does not say %q:\n%s", want, text)
		}
	}
}
