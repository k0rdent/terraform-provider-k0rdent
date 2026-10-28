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
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// Only CONFLICT_IMMUTABLE is the backend refusing. A change in flight is not.
func TestBindingConflictNamesTheBackendOnlyWhenItIsTheCause(t *testing.T) {
	for reason, wantBackendHint := range map[string]bool{"CONFLICT_IMMUTABLE": true, "CONFLICT_IN_USE": false} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			if err := json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": 409, "message": "refused", "status": "CONFLICT",
				"details": []any{map[string]any{"reason": reason}},
			}}); err != nil {
				t.Errorf("write: %v", err)
			}
		}))
		r := &vpcSecurityGroupsResource{meta: testClusterMeta(t, server.URL, time.Minute)}
		groups, _ := types.ListValueFrom(context.Background(), types.StringType, []string{"g1"})
		var s resource.SchemaResponse
		r.Schema(context.Background(), resource.SchemaRequest{}, &s)
		dst := tfsdk.State{Raw: tftypes.NewValue(s.Schema.Type().TerraformType(context.Background()), nil), Schema: s.Schema}
		_, diags := r.apply(context.Background(), vpcSecurityGroupsModel{
			VPC: types.StringValue("v1"), SecurityGroups: groups,
			Region: types.StringValue("test"), Project: types.StringValue("test"),
		}, &dst, nil)
		server.Close()
		gotHint := false
		for _, d := range diags.Warnings() {
			gotHint = gotHint || d.Summary() == "Check the VPC's fabric backend"
		}
		if gotHint != wantBackendHint {
			t.Errorf("%s: backend hint = %v, want %v", reason, gotHint, wantBackendHint)
		}
	}
}

// Taking a binding off puts the VPC's platform-managed DEFAULT group back on,
// which filters differently from the groups it replaces. A replacement takes
// the old binding off just as a destroy does, and the old VPC keeps DEFAULT
// when the new binding names another one, so both plans carry the warning. A change made in place swaps
// one list for another and never passes through DEFAULT, so it doesn't.
func TestTheWideningWarningFiresOnEveryPlanThatTakesTheBindingOff(t *testing.T) {
	r := &vpcSecurityGroupsResource{meta: &Meta{Region: "test", Project: "test"}}
	groups := func(ids ...string) tftypes.Value {
		vals := make([]tftypes.Value, len(ids))
		for i, id := range ids {
			vals[i] = str(id)
		}
		return tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, vals)
	}
	prior := map[string]tftypes.Value{
		"vpc": str("v1"), "region": str("test"), "project": str("test"), "security_groups": groups("sg-a"),
	}
	for _, tc := range []struct {
		name   string
		change map[string]tftypes.Value
		warns  bool
	}{
		{"another vpc", map[string]tftypes.Value{"vpc": str("v2")}, true},
		{"a vpc known only after apply", map[string]tftypes.Value{"vpc": tftypes.NewValue(tftypes.String, tftypes.UnknownValue)}, true},
		{"another project", map[string]tftypes.Value{"project": str("elsewhere")}, true},
		{"another region", map[string]tftypes.Value{"region": str("west")}, true},
		{"the groups in place", map[string]tftypes.Value{"security_groups": groups("sg-a", "sg-b")}, false},
		{"nothing", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := planOf(t, r, prior, with(prior, tc.change))
			if resp.Diagnostics.HasError() {
				t.Fatalf("plan failed: %v", resp.Diagnostics)
			}
			got := wideningWarning(resp.Diagnostics)
			if tc.warns && (got == "" || !strings.Contains(got, `"v1"`)) {
				t.Errorf("no widening warning naming the old VPC in %v", resp.Diagnostics)
			}
			if !tc.warns && got != "" {
				t.Errorf("an in-place plan warned: %s", got)
			}
		})
	}

	t.Run("destroy", func(t *testing.T) {
		stateRaw, s := testValue(t, r, prior)
		resp := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Raw: tftypes.NewValue(stateRaw.Type(), nil), Schema: s.Schema}}
		r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
			Config: tfsdk.Config{Raw: tftypes.NewValue(stateRaw.Type(), nil), Schema: s.Schema},
			Plan:   tfsdk.Plan{Raw: tftypes.NewValue(stateRaw.Type(), nil), Schema: s.Schema},
			State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		}, &resp)
		if wideningWarning(resp.Diagnostics) == "" {
			t.Errorf("a destroy did not warn: %v", resp.Diagnostics)
		}
	})
}

// wideningWarning is the summary and detail of the warning that the VPC's
// filter changes, or empty.
func wideningWarning(diags diag.Diagnostics) string {
	for _, d := range diags.Warnings() {
		if strings.Contains(d.Summary(), "changes the VPC's filter") {
			return d.Summary() + " " + d.Detail()
		}
	}
	return ""
}

// What the warning and the description say has to hold under every way
// Terraform runs a replacement. With create_before_destroy on the same VPC the
// destroy runs last, finds the new binding and leaves it, so the VPC isn't on
// DEFAULT at all, and the texts mustn't say it ends the apply there. And a
// replacement forced by `-replace` or
// `replace_triggered_by` is decided by Terraform alone, so the provider's plan
// can't see it coming, and the description mustn't promise a warning there.
func TestTheWideningTextsHoldUnderEveryReplacement(t *testing.T) {
	r := &vpcSecurityGroupsResource{meta: &Meta{Region: "test", Project: "test"}}
	prior := map[string]tftypes.Value{
		"vpc": str("v1"), "region": str("test"), "project": str("test"),
		"security_groups": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{str("sg-a")}),
	}
	resp := planOf(t, r, prior, with(prior, map[string]tftypes.Value{"project": str("elsewhere")}))
	got := wideningWarning(resp.Diagnostics)
	if !strings.Contains(got, "create_before_destroy") {
		t.Errorf("the replacement warning doesn't say what create_before_destroy does: %s", got)
	}
	if strings.Contains(got, "because the destroy then runs last") {
		t.Errorf("the replacement warning says create_before_destroy leaves the VPC on DEFAULT: %s", got)
	}

	var s resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &s)
	desc := s.Schema.MarkdownDescription
	if strings.Contains(desc, "the plan warns when either is coming") {
		t.Error("the description promises a warning the provider can't give for -replace or replace_triggered_by")
	}
	if strings.Contains(desc, "so the VPC is left on DEFAULT after the apply") {
		t.Error("the description says create_before_destroy leaves the VPC on DEFAULT")
	}
	for _, want := range []string{"create_before_destroy", "-replace", "replace_triggered_by"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the description doesn't mention %s", want)
		}
	}
}

// A destroy clears the binding only when the VPC still holds exactly what this
// instance last read there. Order counts, because the fabric matches rules
// first-match and so two orders are two filters. And a binding that a newer
// instance wrote earlier in the same run belongs to that instance even when
// it names the same groups, which is what -replace with create_before_destroy
// writes.
func TestADestroyClearsOnlyTheBindingItLastRead(t *testing.T) {
	for _, tc := range []struct {
		name           string
		live, lastRead []string
		newer          []string
		want           bindingVerdict
	}{
		{"unchanged", []string{"a", "b"}, []string{"a", "b"}, nil, bindingOurs},
		{"reordered", []string{"b", "a"}, []string{"a", "b"}, nil, bindingChanged},
		{"a group added", []string{"a", "b"}, []string{"a"}, nil, bindingChanged},
		{"a group taken off", []string{"a"}, []string{"a", "b"}, nil, bindingChanged},
		{"set back to the default", []string{"default"}, []string{"a"}, nil, bindingChanged},
		{"the default this instance asked for", []string{"default"}, []string{"default"}, nil, bindingOurs},
		{"nothing recorded to compare", []string{"a"}, nil, nil, bindingOurs},
		{"a newer instance wrote other groups", []string{"b"}, []string{"a"}, []string{"b"}, bindingNewer},
		{"a newer instance wrote the same groups", []string{"a"}, []string{"a"}, []string{"a"}, bindingNewer},
		{"changed again after a newer instance wrote", []string{"c"}, []string{"a"}, []string{"b"}, bindingChanged},
		{"a newer instance's write in another order", []string{"b", "a"}, []string{"a", "b"}, []string{"a", "b"}, bindingChanged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := destroyVerdict(tc.live, tc.lastRead, tc.newer); got != tc.want {
				t.Errorf("destroyVerdict(%v, %v, %v) = %v, want %v", tc.live, tc.lastRead, tc.newer, got, tc.want)
			}
		})
	}
}

// The record of what a Create or Update wrote is per VPC and per scope, so a
// write to one VPC never marks another as taken.
func TestTheBindingLedgerIsPerVPC(t *testing.T) {
	var l bindingLedger
	here := clusterTarget{region: "r", project: "p"}
	l.wrote(here, "v1", []string{"a"}, "t1")
	if got := l.lookup(here, "v1"); strings.Join(got, ",") != "a" {
		t.Errorf("v1 = %v, want [a]", got)
	}
	if got := l.newerThan(here, "v1", "t1"); got != nil {
		t.Errorf("v1 seen by its own writer = %v, want nothing newer", got)
	}
	for _, own := range []string{"t2", ""} {
		if got := l.newerThan(here, "v1", own); strings.Join(got, ",") != "a" {
			t.Errorf("v1 seen by %q = %v, want [a]", own, got)
		}
	}
	for _, other := range []struct {
		target clusterTarget
		vpc    string
	}{{here, "v2"}, {clusterTarget{region: "r", project: "q"}, "v1"}, {clusterTarget{region: "s", project: "p"}, "v1"}} {
		if got := l.lookup(other.target, other.vpc); got != nil {
			t.Errorf("%v %s = %v, want nothing", other.target, other.vpc, got)
		}
	}
}

// Another configuration, or the console, set the VPC's binding after this
// instance last read it. Its destroy has to leave that binding on, say so,
// and still drop the instance, rather than put the VPC back on DEFAULT under
// whoever set it. A binding still as it was read is cleared as before.
func TestADestroyLeavesABindingSetElsewhere(t *testing.T) {
	for _, tc := range []struct {
		name      string
		live      string
		wantPosts int
	}{
		{"changed since the last read", `["theirs"]`, 0},
		{"as last read", `["mine"]`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			posts := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				live := tc.live
				if r.Method == http.MethodPost {
					posts++
					live = `["default"]`
					w.WriteHeader(http.StatusAccepted)
				}
				if _, err := w.Write([]byte(`{"id":"v1","state":"active","securityGroups":` + live + `}`)); err != nil {
					t.Errorf("write: %v", err)
				}
			}))
			defer server.Close()

			r := &vpcSecurityGroupsResource{meta: testClusterMeta(t, server.URL, time.Minute)}
			list := func(ids ...string) tftypes.Value {
				vals := make([]tftypes.Value, len(ids))
				for i, id := range ids {
					vals[i] = str(id)
				}
				return tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, vals)
			}
			stateRaw, s := testValue(t, r, map[string]tftypes.Value{
				"vpc": str("v1"), "region": str("test"), "project": str("test"),
				"security_groups": list("mine"), "attached": list("mine"),
			})
			var resp resource.DeleteResponse
			r.Delete(context.Background(), resource.DeleteRequest{State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}, &resp)

			if resp.Diagnostics.HasError() {
				t.Fatalf("delete failed: %v", resp.Diagnostics)
			}
			if posts != tc.wantPosts {
				t.Errorf("sent %d writes, want %d", posts, tc.wantPosts)
			}
			left := false
			for _, d := range resp.Diagnostics.Warnings() {
				left = left || strings.Contains(d.Detail(), "[theirs]")
			}
			if left != (tc.wantPosts == 0) {
				t.Errorf("warning naming what was left = %v, want %v: %v", left, tc.wantPosts == 0, resp.Diagnostics)
			}
		})
	}
}

// bindingServer is one VPC, v1, whose binding a POST replaces and a GET
// reports, always settled. posted is told of every POST's body as it arrives,
// before it's applied.
type bindingServer struct {
	t      *testing.T
	mu     sync.Mutex
	groups []string
	posted chan []string
}

func (b *bindingServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if strings.Contains(r.URL.Path, "/security-groups/") {
		// The VPC's own default group, the only group any test here reads.
		if _, err := w.Write([]byte(`{"id":"default","ownerKind":"vpc","state":"active"}`)); err != nil {
			b.t.Errorf("write: %v", err)
		}
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if r.Method == http.MethodPost {
		var body struct {
			SecurityGroups []string `json:"securityGroups"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			b.t.Errorf("decode: %v", err)
		}
		if b.posted != nil {
			b.posted <- body.SecurityGroups
		}
		b.groups = body.SecurityGroups
		if len(b.groups) == 0 {
			b.groups = []string{"default"}
		}
		w.WriteHeader(http.StatusAccepted)
	}
	if err := json.NewEncoder(w).Encode(map[string]any{"id": "v1", "state": "active", "securityGroups": b.groups}); err != nil {
		b.t.Errorf("write: %v", err)
	}
}

func (b *bindingServer) holds() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.groups
}

func bindingValues(vpc string, groups ...string) map[string]tftypes.Value {
	vals := make([]tftypes.Value, len(groups))
	for i, id := range groups {
		vals[i] = str(id)
	}
	list := tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, vals)
	return map[string]tftypes.Value{
		"vpc": str(vpc), "region": str("test"), "project": str("test"),
		"security_groups": list, "attached": list,
	}
}

// Two instances under parallelism, one destroying its binding on v1 while the
// other creates one there with the same groups, as a swap of two bindings
// between two VPCs does. The destroy reads v1, finds its own list and decides
// to clear it. If the create's write and its record land between that
// decision and the destroy's write, the destroy wipes the binding the create
// just made. The interleaving is forced: the destroy pauses after deciding,
// starts the create, and waits until the create's write reaches the server or
// can't. Holding the VPC from the read to the write is what keeps it out.
func TestADestroyAndACreateOnOneVPCDoNotInterleave(t *testing.T) {
	server := &bindingServer{t: t, groups: []string{"g"}, posted: make(chan []string, 4)}
	ts := httptest.NewServer(server)
	defer ts.Close()
	meta := testClusterMeta(t, ts.URL, time.Minute)

	creator := &vpcSecurityGroupsResource{meta: meta}
	created := make(chan struct{})
	var createDiags diag.Diagnostics
	destroyer := &vpcSecurityGroupsResource{meta: meta, beforeClear: func() {
		go func() {
			planRaw, s := testValue(t, creator, with(bindingValues("v1", "g"), map[string]tftypes.Value{
				"attached": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, tftypes.UnknownValue),
			}))
			var resp resource.CreateResponse
			resp.State = tfsdk.State{Raw: tftypes.NewValue(planRaw.Type(), nil), Schema: s.Schema}
			creator.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}, &resp)
			createDiags = resp.Diagnostics
			close(created)
		}()
		select {
		case <-server.posted:
			// The create got its write in, and is recording it now.
			<-created
		case <-time.After(200 * time.Millisecond):
		}
	}}

	stateRaw, s := testValue(t, destroyer, bindingValues("v1", "g"))
	var resp resource.DeleteResponse
	destroyer.Delete(context.Background(), resource.DeleteRequest{State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete failed: %v", resp.Diagnostics)
	}
	select {
	case <-created:
		if createDiags.HasError() {
			t.Fatalf("create failed: %v", createDiags)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the create never finished")
	}
	if got := server.holds(); strings.Join(got, ",") != "g" {
		t.Errorf("v1 holds %v after the destroy and the create, want [g]", got)
	}
}

// With create_before_destroy and a new binding of [], the platform reports the
// VPC's default group, and that's what the old destroy finds. Clearing it
// would change nothing, so there's nothing to warn about and nothing to send.
func TestADestroyThatFindsOnlyTheDefaultSaysNothing(t *testing.T) {
	server := &bindingServer{t: t, groups: []string{"default"}}
	ts := httptest.NewServer(server)
	defer ts.Close()
	r := &vpcSecurityGroupsResource{meta: testClusterMeta(t, ts.URL, time.Minute)}
	r.meta.vpcBindings.wrote(clusterTarget{region: "test", project: "test"}, "v1", []string{"default"}, "newer")
	server.posted = make(chan []string, 1)

	stateRaw, s := testValue(t, r, bindingValues("v1", "g"))
	var resp resource.DeleteResponse
	r.Delete(context.Background(), resource.DeleteRequest{State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete failed: %v", resp.Diagnostics)
	}
	if len(resp.Diagnostics.Warnings()) != 0 {
		t.Errorf("warned about a VPC already on its default: %v", resp.Diagnostics)
	}
	if len(server.posted) != 0 {
		t.Error("wrote to a VPC already on its default")
	}
}

// bindingProvider is one configured provider serving the bindingServer, the
// way `terraform test` configures a provider block once and reuses it for
// every run and the teardown. It goes through the protocol because the
// private state a write leaves for its own destroy is carried by the
// framework, not by Create or Delete.
func bindingProvider(t *testing.T, server *bindingServer) (tfprotov6.ProviderServer, tftypes.Object) {
	t.Helper()
	ctx := context.Background()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth/token") {
			writeJSON(w, map[string]any{"access_token": "t", "token_type": "Bearer", "expires_in": 900})
			return
		}
		server.ServeHTTP(w, r)
	}))
	t.Cleanup(api.Close)

	p := &aiProvider{version: "test", lookupEnv: envMap(saEnv(nil))}
	ps, err := providerserver.NewProtocol6WithError(p)()
	if err != nil {
		t.Fatal(err)
	}
	schemas, err := ps.GetProviderSchema(ctx, &tfprotov6.GetProviderSchemaRequest{})
	if err != nil {
		t.Fatal(err)
	}
	config := nullObjectWith(t, schemas.Provider.ValueType(), map[string]string{
		"endpoint": api.URL, "region": "test", "project": "test",
	})
	configured, err := ps.ConfigureProvider(ctx, &tfprotov6.ConfigureProviderRequest{Config: config})
	if err != nil || len(diagnosticErrors(configured.Diagnostics)) > 0 {
		t.Fatalf("configure: %v %v", err, diagnosticErrors(configured.Diagnostics))
	}
	obj, ok := schemas.ResourceSchemas[ResourceName(vpcSecurityGroupsResourceSuffix)].ValueType().(tftypes.Object)
	if !ok {
		t.Fatal("the binding schema isn't an object")
	}
	return ps, obj
}

// bindingApply sends one ApplyResourceChange for the binding on v1 and fails
// the test on an error. groups nil is a destroy.
func bindingApply(t *testing.T, ps tfprotov6.ProviderServer, obj tftypes.Object, prior *tfprotov6.DynamicValue, private []byte, groups []string) *tfprotov6.ApplyResourceChangeResponse {
	t.Helper()
	nullValue := func() *tfprotov6.DynamicValue {
		dv, err := tfprotov6.NewDynamicValue(obj, tftypes.NewValue(obj, nil))
		if err != nil {
			t.Fatal(err)
		}
		return &dv
	}
	if prior == nil {
		prior = nullValue()
	}
	planned := nullValue()
	if groups != nil {
		vals := map[string]tftypes.Value{}
		for name, at := range obj.AttributeTypes {
			vals[name] = tftypes.NewValue(at, nil)
		}
		list := make([]tftypes.Value, len(groups))
		for i, g := range groups {
			list[i] = tftypes.NewValue(tftypes.String, g)
		}
		vals["vpc"] = tftypes.NewValue(tftypes.String, "v1")
		vals["region"] = tftypes.NewValue(tftypes.String, "test")
		vals["project"] = tftypes.NewValue(tftypes.String, "test")
		vals["security_groups"] = tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, list)
		vals["attached"] = tftypes.NewValue(obj.AttributeTypes["attached"], tftypes.UnknownValue)
		dv, err := tfprotov6.NewDynamicValue(obj, tftypes.NewValue(obj, vals))
		if err != nil {
			t.Fatal(err)
		}
		planned = &dv
	}
	resp, err := ps.ApplyResourceChange(context.Background(), &tfprotov6.ApplyResourceChangeRequest{
		TypeName:       ResourceName(vpcSecurityGroupsResourceSuffix),
		PriorState:     prior,
		PlannedState:   planned,
		Config:         planned,
		PlannedPrivate: private,
	})
	if err != nil {
		t.Fatal(err)
	}
	if errs := diagnosticErrors(resp.Diagnostics); len(errs) > 0 {
		t.Fatalf("apply: %v", errs)
	}
	return resp
}

// One configured provider creates a binding and later destroys it, as a
// `terraform test` file does across its runs and teardown. The ledger then
// holds the very list the destroy finds, but this instance wrote it, so the
// destroy is clearing its own binding and has to send the empty list. A
// replacement's new instance writing the same list is still left alone.
func TestADestroyClearsABindingItWroteItself(t *testing.T) {
	t.Run("its own write", func(t *testing.T) {
		server := &bindingServer{t: t, groups: []string{"default"}}
		ps, obj := bindingProvider(t, server)

		created := bindingApply(t, ps, obj, nil, nil, []string{"g"})
		destroyed := bindingApply(t, ps, obj, created.NewState, created.Private, nil)

		if got := server.holds(); strings.Join(got, ",") != "default" {
			t.Errorf("v1 holds %v after its own destroy, want [default]", got)
		}
		for _, d := range destroyed.Diagnostics {
			t.Errorf("destroy said %q: %s", d.Summary, d.Detail)
		}
	})
	t.Run("a newer instance's write of the same groups", func(t *testing.T) {
		server := &bindingServer{t: t, groups: []string{"default"}}
		ps, obj := bindingProvider(t, server)

		old := bindingApply(t, ps, obj, nil, nil, []string{"g"})
		bindingApply(t, ps, obj, nil, nil, []string{"g"})
		bindingApply(t, ps, obj, old.NewState, old.Private, nil)

		if got := server.holds(); strings.Join(got, ",") != "g" {
			t.Errorf("v1 holds %v after the old instance's destroy, want [g]", got)
		}
	})
}
