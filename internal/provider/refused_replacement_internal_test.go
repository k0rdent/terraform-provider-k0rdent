package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// checksIn is the replacement checks private holds.
func checksIn(t *testing.T, private any) []replacementCheck {
	t.Helper()
	p, ok := private.(privateState)
	if !ok {
		t.Fatal("private state cannot be read")
	}
	var diags diag.Diagnostics
	got := replacementChecksIn(context.Background(), p, &diags)
	if diags.HasError() {
		t.Fatal(diags)
	}
	return got
}

// withChecks writes checks into a fresh private state at field.
func withChecks[T any](t *testing.T, field *T, checks ...replacementCheck) {
	t.Helper()
	freshPrivate(t, field, nil)
	raw, err := json.Marshal(checks)
	if err != nil {
		t.Fatal(err)
	}
	w, ok := any(*field).(privateWriter)
	if !ok {
		t.Fatal("private state cannot be written")
	}
	if d := w.SetKey(context.Background(), replacementChecksKey, raw); d.HasError() {
		t.Fatal(d)
	}
}

// A create plan keeps the region and project its region check was given,
// whatever the check decided, so that Delete can make it again live.
func TestACreatePlanKeepsItsRegionCheckForDelete(t *testing.T) {
	for name, api := range map[string]*mismatchAPI{
		"the project in another region":  {home: "us-east"},
		"the project in the same region": {home: "eu"},
		"the project not listed":         {home: "us-east", hidden: true},
	} {
		t.Run(name, func(t *testing.T) {
			r := &networkResource{meta: mismatchMeta(t, api)}
			configRaw, s := testValue(t, r, map[string]tftypes.Value{"id": str("n"), "project": str("p"), "region": str("eu")})
			plan := tfsdk.Plan{Raw: configRaw, Schema: s.Schema}
			resp := resource.ModifyPlanResponse{Plan: plan}
			freshPrivate(t, &resp.Private, nil)
			modifyScopePlan(context.Background(), r.meta, resource.ModifyPlanRequest{
				Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema},
				Plan:   plan,
				State:  tfsdk.State{Raw: tftypes.NewValue(configRaw.Type(), nil), Schema: s.Schema},
			}, &resp)
			got := checksIn(t, resp.Private)
			if len(got) != 1 || got[0] != (replacementCheck{Kind: "region", Region: "eu", Project: "p"}) {
				t.Errorf("checks kept = %+v, want the region check with region eu and project p", got)
			}
		})
	}
}

// regionKinds are the registered resources with a region and a project,
// each of which refuses at apply a create in another region than its
// project's. A new kind with both joins the list by itself.
func regionKinds(t *testing.T) map[string]resource.Resource {
	t.Helper()
	out := map[string]resource.Resource{}
	for _, f := range (&aiProvider{}).Resources(context.Background()) {
		r := f()
		var md resource.MetadataResponse
		r.Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: "k0rdent"}, &md)
		var s resource.SchemaResponse
		r.Schema(context.Background(), resource.SchemaRequest{}, &s)
		if _, ok := s.Schema.Attributes["region"]; !ok {
			continue
		}
		if _, ok := s.Schema.Attributes["project"]; !ok {
			continue
		}
		out[md.TypeName] = r
	}
	return out
}

// deleteWithChecks runs r's Delete of a resource in region eu and project p,
// with checks in the private state the plan handed it, against api. It
// returns the response and every request the API saw.
func deleteWithChecks(t *testing.T, r resource.Resource, api http.HandlerFunc, checks ...replacementCheck) (resource.DeleteResponse, []string) {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		mu.Lock()
		sent = append(sent, req.Method+" "+req.URL.Path)
		mu.Unlock()
		api(w, req)
	}))
	t.Cleanup(server.Close)
	meta := testClusterMeta(t, server.URL, time.Minute)
	configureWith(t, r, meta)
	stateRaw, s := testValue(t, r, scopedValues(r, "x1"))
	req := resource.DeleteRequest{State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}
	withChecks(t, &req.Private, checks...)
	resp := resource.DeleteResponse{State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}
	withChecks(t, &resp.Private, checks...)
	r.Delete(context.Background(), req, &resp)
	mu.Lock()
	defer mu.Unlock()
	return resp, append([]string(nil), sent...)
}

// scopedValues are r's region eu and project p, and id when r has one.
func scopedValues(r resource.Resource, id string) map[string]tftypes.Value {
	var s resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &s)
	v := map[string]tftypes.Value{"region": str("eu"), "project": str("p")}
	if _, ok := s.Schema.Attributes["id"]; ok {
		v["id"] = str(id)
	}
	return v
}

// configureWith hands r the provider's meta, as the framework does.
func configureWith(t *testing.T, r resource.Resource, meta *Meta) {
	t.Helper()
	c, ok := r.(resource.ResourceWithConfigure)
	if !ok {
		t.Fatal("no Configure")
	}
	var resp resource.ConfigureResponse
	c.Configure(context.Background(), resource.ConfigureRequest{ProviderData: meta}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
}

// projectsIn answers the project list with project p in home, and refuses
// everything else.
func projectsIn(home string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/regions/global/projects" {
			writeJSON(w, map[string]any{"projects": []any{
				map[string]any{"id": "p", "organizationId": "o", "region": home},
			}, "totalCount": 1})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// The delete that starts a replacement whose create the region check would
// refuse is refused before anything is sent, on every kind with a region,
// and the object stays in state. The checks are cleared from the private
// state the refused delete leaves, since Terraform saves it.
func TestADeleteBeforeARefusedCreateIsRefusedOnEveryRegionKind(t *testing.T) {
	kinds := regionKinds(t)
	if len(kinds) < 8 {
		t.Fatalf("found %d kinds with a region and a project, want at least 8: %v", len(kinds), kinds)
	}
	for name, r := range kinds {
		t.Run(name, func(t *testing.T) {
			resp, sent := deleteWithChecks(t, r, projectsIn("home"),
				replacementCheck{Kind: "region", Region: "eu", Project: "p"})
			errs := resp.Diagnostics.Errors()
			if len(errs) != 1 || errs[0].Summary() != replacementRefusal {
				t.Fatalf("diagnostics = %v, want the replacement's refusal", resp.Diagnostics)
			}
			for _, want := range []string{`Project "p" is in region "home"`, "this apply sent nothing for this resource",
				"`terraform destroy` still deletes it"} {
				if !strings.Contains(errs[0].Detail(), want) {
					t.Errorf("the refusal doesn't say %q:\n%s", want, errs[0].Detail())
				}
			}
			if got := strings.Join(sent, ", "); got != "GET /v1/regions/global/projects" {
				t.Errorf("sent %s, want only the project list", got)
			}
			if resp.State.Raw.IsNull() {
				t.Error("the refused delete dropped the resource from state")
			}
			if got := checksIn(t, resp.Private); len(got) != 0 {
				t.Errorf("the refused delete left %+v in private state", got)
			}
		})
	}
}

// Delete makes the check live: a cause gone since the plan, or a project
// list that can't say, lets the delete go ahead, as Create would.
func TestADeleteGoesAheadWhenTheCheckPassesNowOrCantDecide(t *testing.T) {
	for name, api := range map[string]http.HandlerFunc{
		"the project in the same region now": projectsIn("eu"),
		"the project list unreadable":        func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) },
	} {
		t.Run(name, func(t *testing.T) {
			resp, sent := deleteWithChecks(t, newNetworkResource(), api,
				replacementCheck{Kind: "region", Region: "eu", Project: "p"})
			for _, d := range resp.Diagnostics.Errors() {
				if d.Summary() == replacementRefusal {
					t.Fatalf("the delete was refused: %s", d.Detail())
				}
			}
			deleted := false
			for _, s := range sent {
				deleted = deleted || strings.HasPrefix(s, http.MethodDelete+" ")
			}
			if !deleted {
				t.Errorf("sent %v, want the delete", sent)
			}
		})
	}
}

// Every plan starts with the checks cleared, a destroy's included, on every
// kind with a region, so `terraform destroy` deletes whatever an earlier
// plan or a refused delete left in state.
func TestEveryPlanClearsTheChecksAnEarlierOneLeft(t *testing.T) {
	for name, r := range regionKinds(t) {
		t.Run(name, func(t *testing.T) {
			configureWith(t, r, testClusterMeta(t, "http://127.0.0.1:1", time.Minute))
			stateRaw, s := testValue(t, r, scopedValues(r, "x1"))
			req := resource.ModifyPlanRequest{
				State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
				Plan:   tfsdk.Plan{Raw: tftypes.NewValue(stateRaw.Type(), nil), Schema: s.Schema},
				Config: tfsdk.Config{Raw: tftypes.NewValue(stateRaw.Type(), nil), Schema: s.Schema},
			}
			withChecks(t, &req.Private, replacementCheck{Kind: "region", Region: "eu", Project: "p"})
			resp := resource.ModifyPlanResponse{Plan: req.Plan}
			withChecks(t, &resp.Private, replacementCheck{Kind: "region", Region: "eu", Project: "p"})
			m, ok := r.(resource.ResourceWithModifyPlan)
			if !ok {
				t.Fatal("no ModifyPlan")
			}
			m.ModifyPlan(context.Background(), req, &resp)
			if got := checksIn(t, resp.Private); len(got) != 0 {
				t.Errorf("the destroy's plan kept %+v", got)
			}
		})
	}
}
