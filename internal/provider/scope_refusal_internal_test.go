package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// A move of region or project to where the resource's project can't be is
// refused at apply, not at plan, so that `terraform destroy` isn't stopped
// by it, and not planned as a replacement, which would delete the resource
// and then fail its create. See scope_refusal.go.

// planNetworkMove runs modifyScopePlan for network n in project p, which
// is in region from, with region to in its configuration.
func planNetworkMove(t *testing.T, meta *Meta, from, to string, private map[string][]byte) resource.ModifyPlanResponse {
	t.Helper()
	r := &networkResource{meta: meta}
	configRaw, s := testValue(t, r, map[string]tftypes.Value{"id": str("n"), "project": str("p"), "region": str(to)})
	stateRaw, _ := testValue(t, r, map[string]tftypes.Value{"id": str("n"), "project": str("p"), "region": str(from)})
	plan := tfsdk.Plan{Raw: configRaw, Schema: s.Schema}
	resp := resource.ModifyPlanResponse{Plan: plan}
	freshPrivate(t, &resp.Private, nil)
	for key, raw := range private {
		writePrivate(t, resp.Private, key, raw)
	}
	modifyScopePlan(context.Background(), meta, resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema},
		Plan:   plan,
		State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
	}, &resp)
	return resp
}

func writePrivate(t *testing.T, private any, key string, raw []byte) {
	t.Helper()
	w, ok := private.(privateWriter)
	if !ok {
		t.Fatal("private state cannot be written")
	}
	if d := w.SetKey(context.Background(), key, raw); d.HasError() {
		t.Fatal(d)
	}
}

// scopeRefused is the refusal private data holds, or nil.
func scopeRefused(t *testing.T, private any) *scopeRefusal {
	t.Helper()
	p, ok := private.(privateState)
	if !ok {
		t.Fatal("private state cannot be read")
	}
	var diags diag.Diagnostics
	r := readScopeRefusal(context.Background(), p, &diags)
	if diags.HasError() {
		t.Fatal(diags)
	}
	return r
}

// A move out of the region the project list shows the project in is no
// replacement and no error. It's recorded for the apply, with what moved.
// A move the list doesn't rule out is a replacement as before.
func TestAMoveOutOfTheProjectsRegionIsRecordedForTheApply(t *testing.T) {
	api := &mismatchAPI{home: "us-east"}
	resp := planNetworkMove(t, mismatchMeta(t, api), "us-east", "eu", nil)
	if resp.Diagnostics.HasError() {
		t.Errorf("errors = %q, want none, so a destroy's plan goes ahead", errorSummaries(resp.Diagnostics))
	}
	if len(resp.RequiresReplace) != 0 {
		t.Errorf("RequiresReplace = %v, want none", resp.RequiresReplace)
	}
	want := &scopeRefusal{
		Moves:  []scopeMove{{Attr: "region", From: "us-east", To: "eu", Configured: true}},
		Where:  `region "eu" with project "p"`,
		Reason: `Project "p" is in region "us-east". A project is locked to one region, and the platform refuses every request for it in another.`,
	}
	if got := scopeRefused(t, resp.Private); !reflect.DeepEqual(got, want) {
		t.Errorf("recorded %+v, want %+v", got, want)
	}

	// The project's own region, and one the list doesn't show the project
	// in, are replacements, and nothing is recorded.
	for name, meta := range map[string]*Meta{
		"into its region": mismatchMeta(t, &mismatchAPI{home: "eu"}),
		"unseen project":  mismatchMeta(t, &mismatchAPI{home: "us-east", hidden: true}),
	} {
		resp := planNetworkMove(t, meta, "us-east", "eu", nil)
		if !resp.RequiresReplace.Contains(path.Root("region")) || resp.Diagnostics.HasError() {
			t.Errorf("%s: RequiresReplace = %v, %v, want region", name, resp.RequiresReplace, resp.Diagnostics)
		}
		if got := scopeRefused(t, resp.Private); got != nil {
			t.Errorf("%s: recorded %+v", name, got)
		}
	}
}

// A refused Update writes the plan's private data back to state, so the
// next plan clears it: a plan with no move after a refusal records nothing.
func TestAPlanClearsAnEarlierScopeRefusal(t *testing.T) {
	earlier := map[string][]byte{scopeRefusedKey: []byte(`{"where":"region \"eu\""}`)}
	resp := planNetworkMove(t, mismatchMeta(t, &mismatchAPI{home: "us-east"}), "us-east", "us-east", earlier)
	if got := scopeRefused(t, resp.Private); got != nil {
		t.Errorf("the earlier refusal outlived the plan: %+v", got)
	}

	// A project's plan too.
	api := &regionsAPI{regions: []map[string]any{region("east", true)}}
	r := &projectResource{meta: regionsMeta(t, api, "east")}
	set := map[string]tftypes.Value{"organization_id": str("o"), "id": str("p"), "region": str("east"),
		"status": str("active"), "self_link": str("/p")}
	raw, s := testValue(t, r, set)
	presp := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}}
	freshPrivate(t, &presp.Private, nil)
	writePrivate(t, presp.Private, scopeRefusedKey, earlier[scopeRefusedKey])
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{Config: tfsdk.Config{Raw: raw, Schema: s.Schema},
		Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}, State: tfsdk.State{Raw: raw, Schema: s.Schema}}, &presp)
	if got := scopeRefused(t, presp.Private); got != nil {
		t.Errorf("the earlier refusal outlived the project's plan: %+v", got)
	}
}

// A project moved to a region the region list says can't hold one is
// recorded for the apply, with the list's reason, and isn't replaced.
func TestAProjectMovedToARegionThatHoldsNoProjectsIsRecordedForTheApply(t *testing.T) {
	api := &regionsAPI{regions: []map[string]any{region("global", false), region("east", true)}}
	state := map[string]tftypes.Value{"id": str("cp-production"), "region": str("east"),
		"status": str("active"), "self_link": str("/p")}
	resp := planProject(t, regionsMeta(t, api, "east"),
		map[string]tftypes.Value{"id": str("cp-production"), "region": str("global")}, state)
	if resp.Diagnostics.HasError() || len(resp.RequiresReplace) != 0 {
		t.Fatalf("errors = %q, RequiresReplace = %v, want neither", errorSummaries(resp.Diagnostics), resp.RequiresReplace)
	}
	got := scopeRefused(t, resp.Private)
	if got == nil {
		t.Fatal("nothing recorded")
	}
	if got.Where != `region "global"` || !reflect.DeepEqual(got.Moves,
		[]scopeMove{{Attr: "region", From: "east", To: "global", Configured: true}}) {
		t.Errorf("recorded %+v", got)
	}
	if !strings.Contains(got.Reason, "is listed with `hostsProjects` false") || !strings.Contains(got.Reason, "east") {
		t.Errorf("reason = %q, want the region list's", got.Reason)
	}
}

// The guarded stub, with a refused move recorded by its own ModifyPlan.
func guardedMovingStub(t *testing.T) (*stubResource, resource.Resource, schema.Schema) {
	t.Helper()
	stub, r, s := guardedStub(t)
	stub.moveRefused = &scopeRefusal{Moves: []scopeMove{{Attr: "region", From: "us-east", To: "eu", Configured: true}},
		Where: `region "eu" with project "p"`, Reason: `Project "p" is in region "us-east".`}
	return stub, r, s
}

func summaries(diags []diag.Diagnostic) []string {
	var out []string
	for _, d := range diags {
		out = append(out, d.Summary())
	}
	return out
}

// The plan warns of a refused move whatever allow_replace says, since the
// replacement couldn't be made. When another change still replaces the
// resource, which on a guarded one takes allow_replace = true, the plan is
// refused: Terraform would delete it before an Update could refuse.
func TestTheGuardWarnsOfARefusedMoveAndRefusesOneThatsStillReplaced(t *testing.T) {
	_, r, s := guardedMovingStub(t)
	prior := stubValue(t, s, stubBase)
	const warning = `This stub can't be in region "eu" with project "p", so the apply will refuse the change`
	const refusal = `This stub can't be in region "eu" with project "p", and this plan replaces it`
	for _, tc := range []struct {
		name             string
		change           map[string]tftypes.Value
		warnings, errors []string
	}{
		{"alone", map[string]tftypes.Value{"name": str("b")}, []string{warning}, nil},
		{"alone, allow_replace", map[string]tftypes.Value{"name": str("b"), allowReplaceAttribute: yes},
			[]string{warning}, nil},
		{"with a replacement the guard refuses", map[string]tftypes.Value{"id": str("s2")},
			[]string{"Changing `id` would replace this stub, so the apply will refuse it", warning}, nil},
		{"with a permitted replacement", map[string]tftypes.Value{"id": str("s2"), allowReplaceAttribute: yes},
			nil, []string{refusal}},
	} {
		resp := planStub(t, r, s, prior, stubValue(t, s, with(stubBase, tc.change)))
		if got := summaries(resp.Diagnostics.Warnings()); !reflect.DeepEqual(got, tc.warnings) {
			t.Errorf("%s: warnings = %q, want %q", tc.name, got, tc.warnings)
		}
		if got := errorSummaries(resp.Diagnostics); !reflect.DeepEqual(got, tc.errors) {
			t.Errorf("%s: errors = %q, want %q", tc.name, got, tc.errors)
		}
		if len(resp.RequiresReplace) != 0 {
			t.Errorf("%s: RequiresReplace = %v, want none", tc.name, resp.RequiresReplace)
		}
		if tc.errors != nil && resp.Diagnostics.HasError() {
			if d := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(d, "for its change to `id`") ||
				!strings.Contains(d, "`terraform destroy -refresh=false`") {
				t.Errorf("%s: the refusal doesn't name the replacing change and the way to destroy: %q", tc.name, d)
			}
		}
	}
}

// updateWithMove runs r's Update with private data that holds a refused
// move, as the plan left it.
func updateWithMove(t *testing.T, r resource.Resource, s schema.Schema, prior, planned tftypes.Value) resource.UpdateResponse {
	t.Helper()
	req := resource.UpdateRequest{Config: tfsdk.Config{Raw: planned, Schema: s},
		Plan: tfsdk.Plan{Raw: planned, Schema: s}, State: tfsdk.State{Raw: prior, Schema: s}}
	freshPrivate(t, &req.Private, nil)
	writePrivate(t, req.Private, scopeRefusedKey,
		[]byte(`{"moves":[{"attr":"region","from":"us-east","to":"eu","configured":true}],`+
			`"where":"region \"eu\" with project \"p\"","reason":"Project \"p\" is in region \"us-east\"."}`))
	resp := resource.UpdateResponse{State: tfsdk.State{Raw: prior, Schema: s}, Private: req.Private}
	r.Update(context.Background(), req, &resp)
	return resp
}

// The apply's half on a guarded resource: the update is refused before the
// resource is called, with allow_replace = true too, state stays as it was,
// and the refusal isn't written back.
func TestTheGuardedUpdateRefusesARecordedMoveWhateverAllowReplaceSays(t *testing.T) {
	stub, r, s := guardedStub(t)
	prior := stubValue(t, s, stubBase)
	planned := stubValue(t, s, with(stubBase, map[string]tftypes.Value{"name": str("b"), allowReplaceAttribute: yes}))
	resp := updateWithMove(t, r, s, prior, planned)
	want := []string{`This stub can't be in region "eu" with project "p", so the apply refused the change`}
	if got := errorSummaries(resp.Diagnostics); !reflect.DeepEqual(got, want) {
		t.Errorf("errors = %q, want %q", got, want)
	}
	if !resp.Diagnostics.HasError() {
		t.FailNow()
	}
	detail := resp.Diagnostics.Errors()[0].Detail()
	for _, say := range []string{`from region "us-east" to "eu"`, `Project "p" is in region "us-east".`,
		"`allow_replace` doesn't change that", "before it sent any request", "set `region` back to \"us-east\""} {
		if !strings.Contains(detail, say) {
			t.Errorf("the refusal doesn't say %q:\n%s", say, detail)
		}
	}
	if len(stub.calls) != 0 {
		t.Errorf("calls = %q, want none", stub.calls)
	}
	if !resp.State.Raw.Equal(prior) {
		t.Errorf("state = %v, want prior state", resp.State.Raw)
	}
	if got := scopeRefused(t, resp.Private); got != nil {
		t.Errorf("the refusal is written back to state: %+v", got)
	}
}

// Every resource with a region refuses a recorded move in its Update before
// it sends a request: the guarded ones through the guard, and the security
// group, the peering and the binding by themselves.
func TestEveryRegionalUpdateRefusesARecordedMoveWithoutARequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Errorf("sent %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	meta := testClusterMeta(t, server.URL, time.Second)
	ctx := context.Background()
	regional := 0
	for _, newResource := range New("test")().Resources(ctx) {
		r := newResource()
		s := schemaOf(t, r)
		if _, ok := s.Attributes["region"]; !ok {
			continue
		}
		regional++
		t.Run(suffixOf(r), func(t *testing.T) {
			if c, ok := r.(resource.ResourceWithConfigure); ok {
				c.Configure(ctx, resource.ConfigureRequest{ProviderData: meta}, &resource.ConfigureResponse{})
			}
			values := map[string]tftypes.Value{}
			for name, v := range map[string]tftypes.Value{"id": str("x1"), "region": str("us-east"), "project": str("p")} {
				if _, ok := s.Attributes[name]; ok {
					values[name] = v
				}
			}
			prior, _ := testValue(t, r, values)
			planned, _ := testValue(t, r, with(values, map[string]tftypes.Value{"region": str("eu")}))
			resp := updateWithMove(t, r, s, prior, planned)
			got := errorSummaries(resp.Diagnostics)
			if len(got) != 1 || !strings.HasSuffix(got[0], `can't be in region "eu" with project "p", so the apply refused the change`) {
				t.Errorf("errors = %q, want the refusal", got)
			}
			if !resp.State.Raw.Equal(prior) {
				t.Errorf("state = %v, want prior state", resp.State.Raw)
			}
			if got := scopeRefused(t, resp.Private); got != nil {
				t.Errorf("the refusal is written back to state: %+v", got)
			}
		})
	}
	if regional != 9 {
		t.Errorf("%d resources have a region, want the 9 this was written for", regional)
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("%d requests, want none", n)
	}
}
