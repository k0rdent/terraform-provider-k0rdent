package provider

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// An adopt-only create sends no create request. It records the cluster only
// when the platform's token equals the configured one, and refuses one with no
// token or another token, before anything reaches state.
func TestAnAdoptOnlyCreateRecordsOnlyAnExactTokenMatch(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		lookup  createLookup
		outcome createOutcome
		want    string
	}{
		"absent":      {lookup: createLookup{}, outcome: createAdoptAbsent},
		"read failed": {lookup: createLookup{err: context.DeadlineExceeded}, outcome: createFailed, want: "Could not read the cluster back"},
		"no token": {lookup: createLookup{found: true, uid: "u1"}, outcome: createFailed,
			want: "carries no create token, so it cannot exactly match"},
		"another token": {lookup: createLookup{found: true, token: "another-run", uid: "u1"}, outcome: createFailed,
			want: "does not exactly match the `create_token`"},
		"match": {lookup: createLookup{found: true, token: "wanted", uid: "u1"}, outcome: createDone,
			want: "has the exact `create_token`"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c := convergingCreate{
				key:  createKey{id: "c1", token: "wanted", durableToken: true},
				kind: "cluster", what: "create the cluster", adoptOnly: true,
				resourceName: ResourceName(clusterResourceSuffix), importID: func(id string) string { return id },
				send: func(context.Context) createAnswer {
					t.Error("an adopt-only create sent a create request")
					return createAnswer{}
				},
				lookup: func(context.Context, string) createLookup { return tc.lookup },
			}
			outcome, made, diags := c.run(context.Background())
			if outcome != tc.outcome {
				t.Fatalf("outcome = %v, want %v: %s", outcome, tc.outcome, diagText(diags))
			}
			if tc.want != "" && !strings.Contains(diagText(diags), tc.want) {
				t.Errorf("the diagnostics don't say %q:\n%s", tc.want, diagText(diags))
			}
			if tc.want == "" && len(diags) > 0 {
				// The caller reports absence once, so the adopt adds nothing.
				t.Errorf("an absent cluster got diagnostics from the adopt:\n%s", diagText(diags))
			}
			if outcome == createDone {
				if made.uid != "u1" || diags.HasError() {
					t.Errorf("adopted %+v with errors %v, want uid u1 and none", made, diags.Errors())
				}
			} else if made.id != "" || made.uid != "" {
				t.Errorf("a refused adopt names %+v to record", made)
			}
		})
	}
}

// adoptOnlyPlan is a cluster plan with adopt_only set and token configured.
// A null token leaves create_token null.
func adoptOnlyPlan(t *testing.T, token types.String) (tfsdk.Plan, tfsdk.Config) {
	t.Helper()
	s := testClusterState(t, "c1", "/v1/regions/test/compute/cluster-types/test")
	if d := s.SetAttribute(context.Background(), path.Root(adoptOnlyAttribute), types.BoolValue(true)); d.HasError() {
		t.Fatal(d)
	}
	if d := s.SetAttribute(context.Background(), path.Root(createTokenAttribute), token); d.HasError() {
		t.Fatal(d)
	}
	return tfsdk.Plan(s), tfsdk.Config(s)
}

// The plan guard's own adopt-only branch: a mismatch is refused, and only an
// exact match warns that the apply adopts it.
func TestThePlanGuardAdoptsOnlyAnExactTokenMatch(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		tags      map[string]string
		refused   bool
		wantInAll string
	}{
		"no token":      {tags: map[string]string{}, refused: true, wantInAll: "carries no create token"},
		"another token": {tags: map[string]string{createTokenTag(): "another-run"}, refused: true, wantInAll: "create token differs"},
		"match":         {tags: map[string]string{createTokenTag(): "wanted"}, wantInAll: "Adopting the existing cluster"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newClusterAPI(t)
			api.tags["c1"] = tc.tags
			r := &clusterResource{meta: testClusterMeta(t, api.serve().URL, time.Minute)}
			plan, config := adoptOnlyPlan(t, types.StringValue("wanted"))
			var diags diag.Diagnostics
			r.refuseExisting(context.Background(), plan, config, &diags)
			if diags.HasError() != tc.refused {
				t.Fatalf("refused = %v, want %v:\n%s", diags.HasError(), tc.refused, diagText(diags))
			}
			text := diagText(diags)
			if !strings.Contains(text, tc.wantInAll) {
				t.Errorf("the plan doesn't say %q:\n%s", tc.wantInAll, text)
			}
			if tc.refused && strings.Contains(text, "after the import") {
				t.Errorf("a plan with no prior state describes an import:\n%s", text)
			}
		})
	}
}

// The adopt-only warning shows on a tainted row's replacement too, where
// Terraform destroys first, so it says what each case does.
func TestTheAdoptOnlyWarningCoversATaintedRow(t *testing.T) {
	t.Parallel()
	var diags diag.Diagnostics
	existing{kind: "cluster", resourceName: ResourceName(clusterResourceSuffix), id: "c1", state: "creating"}.warnAdopt(&diags)
	containsAll(t, "the adopt warning", diagText(diags),
		"If this state doesn't hold it, the apply records its UID", "sends no create request",
		"If this state holds it tainted, Terraform replaces it", "deletes it first", "no cluster to adopt",
		"terraform untaint <address>")
}

// Adopt-only without a token is refused at plan, before any read. Without
// this refusal, an untagged cluster's empty token equals a null token's empty
// value, and the import flow would adopt and later destroy a cluster this
// configuration never made.
func TestAdoptOnlyWithoutATokenIsRefusedAtPlan(t *testing.T) {
	t.Parallel()
	api := newClusterAPI(t)
	api.tags["c1"] = map[string]string{}
	r := &clusterResource{meta: testClusterMeta(t, api.serve().URL, time.Minute)}
	plan, config := adoptOnlyPlan(t, types.StringNull())
	for name, state := range map[string]tfsdk.State{
		"no prior state": {Schema: plan.Schema, Raw: tftypes.NewValue(plan.Raw.Type(), nil)},
		"imported row":   tfsdk.State(plan),
	} {
		var diags diag.Diagnostics
		r.checkAdoptOnly(context.Background(), plan, state, config, &diags)
		if !strings.Contains(diagText(diags), "Adopt-only requires a create token") {
			t.Errorf("%s: a null token wasn't refused:\n%s", name, diagText(diags))
		}
	}
}

// A saved adopt plan applied after the cluster is gone reaches the provider
// with prior state over a 404. A plan's refresh would have dropped that row,
// so it's an error, not an adoption of nothing. With no prior state, a 404 is
// left to the create, which reports no cluster to adopt.
func TestAnAdoptPlanOverAGoneClusterIsRefused(t *testing.T) {
	t.Parallel()
	api := newClusterAPI(t)
	r := &clusterResource{meta: testClusterMeta(t, api.serve().URL, time.Minute)}
	plan, config := adoptOnlyPlan(t, types.StringValue("wanted"))

	var held diag.Diagnostics
	r.checkAdoptOnly(context.Background(), plan, tfsdk.State(plan), config, &held)
	containsAll(t, "the stale adopt plan", diagText(held), `The cluster "c1" is gone`, "answers 404",
		"Nothing was recorded, created or deleted", "Run `terraform plan` again")
	if !held.HasError() {
		t.Error("a 404 under prior state wasn't an error")
	}

	var fresh diag.Diagnostics
	r.checkAdoptOnly(context.Background(), plan, tfsdk.State{Schema: plan.Schema, Raw: tftypes.NewValue(plan.Raw.Type(), nil)}, config, &fresh)
	if fresh.HasError() {
		t.Errorf("a 404 with no prior state was refused at plan: %v", fresh.Errors())
	}
}

// The provenance refusal names the import only when there was one, and never
// says removing adopt_only allows a create, since the id is taken.
func TestTheAdoptOnlyRefusalNamesTheImportOnlyWhenHeld(t *testing.T) {
	t.Parallel()
	e := existing{kind: "cluster", resourceName: ResourceName(clusterResourceSuffix), id: "c1", state: "active"}
	for _, held := range []bool{false, true} {
		var diags diag.Diagnostics
		e.refuseAdopt(&diags, "another-run", held)
		text := diagText(diags)
		if got := strings.Contains(text, "This check ran after the import"); got != held {
			t.Errorf("held %v: names the import = %v:\n%s", held, got, text)
		}
		if strings.Contains(text, "remove `adopt_only`") || strings.Contains(text, "allow a create") {
			t.Errorf("held %v: says removing adopt_only allows a create:\n%s", held, text)
		}
		containsAll(t, "the refusal", text, "No create or delete was sent", "pass the original `create_token`",
			"Otherwise choose another `id`.")
	}
}

// A cluster whose token equals the configured one is adopted by an apply that
// doesn't hold it, as after a kill -9. The plan's warning says so, rather than
// that the create fails with "already exists".
func TestTheReplacingWarningSaysAMatchingTokenIsAdopted(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		token, state string
		want, never  []string
	}{
		"match, creating": {token: "wanted", state: "creating",
			want: []string{"applying this plan adopts it, or cancels it if this state holds it tainted",
				"equals the `create_token` this configuration supplied", "the apply adopts it",
				"records its UID and waits for it", "terraform untaint <address>"},
			never: []string{"already exists", "terraform import"}},
		"match, active": {token: "wanted", state: "active",
			want:  []string{"applying this plan adopts it, or replaces it if this state holds it tainted", "the apply adopts it"},
			never: []string{"already exists"}},
		"match, failed": {token: "wanted", state: "failed",
			want:  []string{"the apply adopts it, then fails because it's `failed`"},
			never: []string{"already exists"}},
		"another token": {token: "another-run", state: "creating",
			want:  []string{"cancels it if this state holds it", "the create fails with \"already exists\"", "terraform import"},
			never: []string{"adopts it"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := newClusterAPI(t)
			api.tags["c1"] = map[string]string{createTokenTag(): tc.token}
			api.readAnswer = func(w http.ResponseWriter) bool {
				c := api.cluster("c1")
				c["state"] = tc.state
				api.write(w, c)
				return true
			}
			r := &clusterResource{meta: testClusterMeta(t, api.serve().URL, time.Minute)}
			s := testClusterState(t, "c1", "/v1/regions/test/compute/cluster-types/test")
			if d := s.SetAttribute(context.Background(), path.Root(createTokenAttribute), types.StringValue("wanted")); d.HasError() {
				t.Fatal(d)
			}
			var diags diag.Diagnostics
			r.refuseExisting(context.Background(), tfsdk.Plan(s), tfsdk.Config(s), &diags)
			if diags.HasError() {
				t.Fatalf("the plan was refused: %v", diags.Errors())
			}
			text := diagText(diags)
			containsAll(t, "the plan's warning", text, tc.want...)
			for _, never := range tc.never {
				if strings.Contains(text, never) {
					t.Errorf("the plan's warning says %q:\n%s", never, text)
				}
			}
		})
	}
}

// The create token isn't a secret, so a diagnostic shows it as it is. One
// equal to the cluster's id used to blank the id everywhere it appeared.
func TestACreateTokenEqualToTheIDIsNotBlanked(t *testing.T) {
	t.Parallel()
	api := newClusterAPI(t)
	api.tags["c1"] = map[string]string{createTokenTag(): "someone-elses"}
	meta := testClusterMeta(t, api.serve().URL, time.Minute)
	s := testClusterState(t, "c1", "/v1/regions/test/compute/cluster-types/test")
	if d := s.SetAttribute(context.Background(), path.Root(createTokenAttribute), types.StringValue("c1")); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(s.Raw.Type(), nil), Schema: s.Schema}}
	(&clusterResource{meta: meta}).Create(context.Background(),
		resource.CreateRequest{Plan: tfsdk.Plan(s), Config: tfsdk.Config(s)}, &resp)
	text := diagText(resp.Diagnostics)
	if !resp.Diagnostics.HasError() {
		t.Fatalf("a cluster with another token was adopted:\n%s", text)
	}
	containsAll(t, "the refusal", text, `named "c1" already exists`, "terraform import <address> test/test/c1")
	if strings.Contains(text, "[REDACTED]") {
		t.Errorf("the refusal blanks the token's text:\n%s", text)
	}
}

// What the warning says of a `failed` cluster with this configuration's token,
// run: the create's 409 finds it under the same token and adopts it, its wait
// fails on `failed`, and the error with state set is what Terraform taints.
func TestAFailedClusterWithTheTokenIsAdoptedThenFails(t *testing.T) {
	t.Parallel()
	api := newClusterAPI(t)
	api.tags["c1"] = map[string]string{createTokenTag(): "wanted"}
	api.readAnswer = func(w http.ResponseWriter) bool {
		c := api.cluster("c1")
		c["state"] = "failed"
		api.write(w, c)
		return true
	}
	meta := testClusterMeta(t, api.serve().URL, time.Minute)
	s := testClusterState(t, "c1", "/v1/regions/test/compute/cluster-types/test")
	if d := s.SetAttribute(context.Background(), path.Root(createTokenAttribute), types.StringValue("wanted")); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(s.Raw.Type(), nil), Schema: s.Schema}}
	(&clusterResource{meta: meta}).Create(context.Background(),
		resource.CreateRequest{Plan: tfsdk.Plan(s), Config: tfsdk.Config(s)}, &resp)
	text := diagText(resp.Diagnostics)
	if !resp.Diagnostics.HasError() || resp.State.Raw.IsNull() {
		t.Fatalf("want an error with the cluster in state, got state null = %v:\n%s", resp.State.Raw.IsNull(), text)
	}
	containsAll(t, "the create", text, "its create token matches", "did not become active")
	if len(api.sent) != 1 {
		t.Errorf("%d creates sent, want one", len(api.sent))
	}
}
