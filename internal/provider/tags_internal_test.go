package provider

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/path"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// tagMap is a known map of strings, or a null one for nil.
func tagMap(t *testing.T, m map[string]string) types.Map {
	t.Helper()
	if m == nil {
		return types.MapNull(types.StringType)
	}
	v, diags := types.MapValueFrom(context.Background(), types.StringType, m)
	if diags.HasError() {
		t.Fatalf("building a map: %v", diags)
	}
	return v
}

func knownDefaults(m map[string]string) defaultTags { return defaultTags{tags: m} }

func wantTags(t *testing.T, what string, got types.Map, want map[string]string) {
	t.Helper()
	if !got.Equal(tagMap(t, want)) {
		t.Errorf("%s = %v, want %v", what, got, tagMap(t, want))
	}
}

// The merge is what the plan shows, so every rule a reader of the plan relies
// on is here: a resource tag wins over a default with the same key, and a map
// with nothing in it is still a known map rather than null.
func TestTagsAllMergesDefaultsUnderResourceTags(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		defaults map[string]string
		tags     map[string]string
		want     map[string]string
	}{
		{"defaults alone", map[string]string{"owner": "pipeline"}, nil,
			map[string]string{"owner": "pipeline"}},
		{"resource tags alone", nil, map[string]string{"team": "ml"},
			map[string]string{"team": "ml"}},
		{"a resource tag overrides a default", map[string]string{"env": "prod", "owner": "pipeline"},
			map[string]string{"env": "dev"}, map[string]string{"env": "dev", "owner": "pipeline"}},
		{"a resource tag equal to a default", map[string]string{"env": "prod"},
			map[string]string{"env": "prod"}, map[string]string{"env": "prod"}},
		{"nothing at all", nil, nil, map[string]string{}},
		{"an empty resource map", nil, map[string]string{}, map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, diags := knownDefaults(tc.defaults).merge(ctx, tagMap(t, tc.tags))
			if diags.HasError() {
				t.Fatal(diags)
			}
			wantTags(t, "tags_all", got, tc.want)
		})
	}
}

// AWS's framework resources plan `tags_all` unknown when the resource's tags
// aren't wholly known, and a default that isn't known yet is the same case
// from the other side. A known value guessed from the known half would fail
// the apply with "Provider produced inconsistent final plan".
func TestTagsAllIsUnknownUntilBothHalvesAreKnown(t *testing.T) {
	ctx := context.Background()
	partly := types.MapValueMust(types.StringType, map[string]attr.Value{
		"team": types.StringValue("ml"),
		"run":  types.StringUnknown(),
	})
	for _, tc := range []struct {
		name     string
		defaults defaultTags
		tags     types.Map
	}{
		{"defaults unknown", defaultTags{unknown: true}, tagMap(t, map[string]string{"team": "ml"})},
		{"tags unknown", knownDefaults(map[string]string{"owner": "pipeline"}), types.MapUnknown(types.StringType)},
		{"one tag unknown", knownDefaults(nil), partly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, diags := tc.defaults.merge(ctx, tc.tags)
			if diags.HasError() {
				t.Fatal(diags)
			}
			if !got.IsUnknown() {
				t.Errorf("tags_all = %v, want unknown", got)
			}
		})
	}
}

// A provider block whose default_tags aren't known at plan, wholly or in one
// value, configures rather than failing, and says so.
func TestDefaultTagsFromConfiguration(t *testing.T) {
	ctx := context.Background()
	got, diags := defaultTagsFrom(ctx, types.MapNull(types.StringType))
	if diags.HasError() || got.unknown || len(got.tags) != 0 {
		t.Errorf("null = %+v, %v; want known and empty", got, diags)
	}
	got, diags = defaultTagsFrom(ctx, tagMap(t, map[string]string{"owner": "pipeline"}))
	if diags.HasError() || got.unknown || !reflect.DeepEqual(got.tags, map[string]string{"owner": "pipeline"}) {
		t.Errorf("known = %+v, %v", got, diags)
	}
	partly := types.MapValueMust(types.StringType, map[string]attr.Value{"run": types.StringUnknown()})
	for _, v := range []types.Map{types.MapUnknown(types.StringType), partly} {
		got, diags = defaultTagsFrom(ctx, v)
		if diags.HasError() || !got.unknown {
			t.Errorf("%v = %+v, %v; want unknown", v, got, diags)
		}
	}
	if !(*Meta)(nil).defaultTags().unknown {
		t.Error("no Meta should read as defaults not known yet")
	}
}

// read is where AWS's best-known pitfall lives: a resource tag whose value
// equals a default's. Taking every default-valued key out of `tags` turns
// that tag into a perpetual diff. The rule is AWS's fix
// (ResolveDuplicatesFramework): a key the prior `tags` held stays there.
func TestReadAttributesEachTagToItsSource(t *testing.T) {
	defaults := map[string]string{"env": "prod", "owner": "pipeline"}
	for _, tc := range []struct {
		name          string
		answer        map[string]string
		prior         map[string]string
		priorAll      map[string]string
		defaults      defaultTags
		wantTags      map[string]string
		wantTagsAll   map[string]string
		answerMissing bool
	}{
		{
			name:     "defaults stay out of tags",
			answer:   map[string]string{"env": "prod", "owner": "pipeline", "team": "ml"},
			prior:    map[string]string{"team": "ml"},
			defaults: knownDefaults(defaults),
			wantTags: map[string]string{"team": "ml"}, wantTagsAll: map[string]string{"env": "prod", "owner": "pipeline", "team": "ml"},
		},
		{
			name:     "a resource tag equal to a default stays in tags",
			answer:   map[string]string{"env": "prod", "owner": "pipeline"},
			prior:    map[string]string{"env": "prod"},
			defaults: knownDefaults(defaults),
			wantTags: map[string]string{"env": "prod"}, wantTagsAll: map[string]string{"env": "prod", "owner": "pipeline"},
		},
		{
			name:     "an override stays in tags",
			answer:   map[string]string{"env": "dev", "owner": "pipeline"},
			prior:    map[string]string{"env": "dev"},
			defaults: knownDefaults(defaults),
			wantTags: map[string]string{"env": "dev"}, wantTagsAll: map[string]string{"env": "dev", "owner": "pipeline"},
		},
		{
			// Today's behaviour: the provider owns the whole map, so a tag set
			// some other way is drift in `tags` and the next apply removes it.
			name:     "a tag set out of band is drift in tags",
			answer:   map[string]string{"owner": "pipeline", "added": "by-hand"},
			prior:    nil,
			priorAll: map[string]string{"owner": "pipeline"},
			defaults: knownDefaults(defaults),
			wantTags: map[string]string{"added": "by-hand"}, wantTagsAll: map[string]string{"owner": "pipeline", "added": "by-hand"},
		},
		{
			name:     "a default changed out of band is drift in tags",
			answer:   map[string]string{"owner": "someone-else"},
			priorAll: map[string]string{"owner": "pipeline"},
			defaults: knownDefaults(defaults),
			wantTags: map[string]string{"owner": "someone-else"}, wantTagsAll: map[string]string{"owner": "someone-else"},
		},
		{
			// The default was taken out of the provider block, and the
			// platform still has it. It's the plan's job to remove it, and
			// it shows that in tags_all alone rather than as a tag the
			// configuration never wrote.
			name:     "a default since removed is still the default's",
			answer:   map[string]string{"owner": "pipeline", "team": "ml"},
			prior:    map[string]string{"team": "ml"},
			priorAll: map[string]string{"owner": "pipeline", "team": "ml"},
			defaults: knownDefaults(nil),
			wantTags: map[string]string{"team": "ml"}, wantTagsAll: map[string]string{"owner": "pipeline", "team": "ml"},
		},
		{
			// A refresh while default_tags waits on another resource reads
			// with the attribution the state already had.
			name:     "defaults not known yet",
			answer:   map[string]string{"owner": "pipeline", "team": "ml"},
			prior:    map[string]string{"team": "ml"},
			priorAll: map[string]string{"owner": "pipeline", "team": "ml"},
			defaults: defaultTags{unknown: true},
			wantTags: map[string]string{"team": "ml"}, wantTagsAll: map[string]string{"owner": "pipeline", "team": "ml"},
		},
		{
			// An import has no prior, so a key with the default's value is
			// the default's.
			name:     "import",
			answer:   map[string]string{"env": "prod", "team": "ml"},
			defaults: knownDefaults(defaults),
			wantTags: map[string]string{"team": "ml"}, wantTagsAll: map[string]string{"env": "prod", "team": "ml"},
		},
		{
			name:     "only defaults, and no tags configured",
			answer:   map[string]string{"owner": "pipeline"},
			defaults: knownDefaults(defaults),
			wantTags: nil, wantTagsAll: map[string]string{"owner": "pipeline"},
		},
		{
			name:     "only defaults, and tags = {} configured",
			answer:   map[string]string{"owner": "pipeline"},
			prior:    map[string]string{},
			defaults: knownDefaults(defaults),
			wantTags: map[string]string{}, wantTagsAll: map[string]string{"owner": "pipeline"},
		},
		{
			name:          "no tags at all",
			answerMissing: true,
			defaults:      knownDefaults(nil),
			wantTags:      nil, wantTagsAll: map[string]string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer := tagMap(t, tc.answer)
			if tc.answerMissing {
				answer = types.MapNull(types.StringType)
			}
			prior := tenantMetadata{Tags: tagMap(t, tc.prior), TagsAll: tagMap(t, tc.priorAll)}
			if tc.priorAll == nil {
				prior.TagsAll = types.MapNull(types.StringType)
			}
			got := prior.read(metadataAnswer{Tags: answer, Defaults: tc.defaults})
			wantTags(t, "tags", got.Tags, tc.wantTags)
			wantTags(t, "tags_all", got.TagsAll, tc.wantTagsAll)
		})
	}
}

// The create sends tags_all, which is what the plan showed, and the create
// token goes on top of it. The token never reaches tags_all: the read takes
// it out before anything is attributed.
func TestCreateSendsTagsAllAndTheTokenStaysOutOfState(t *testing.T) {
	ctx := context.Background()
	plan := clusterModel{
		Tags:    tagMap(t, map[string]string{"team": "ml"}),
		TagsAll: tagMap(t, map[string]string{"team": "ml", "owner": "pipeline"}),
	}
	md, diags := plan.metadata().create(ctx)
	if diags.HasError() {
		t.Fatal(diags)
	}
	key := createKey{id: "c1", token: "t0k3n"}
	sent := key.tagged(md.Tags)
	want := map[string]string{"team": "ml", "owner": "pipeline", createTokenTag(): "t0k3n"}
	if sent == nil || !reflect.DeepEqual(*sent, want) {
		t.Fatalf("create sent %v, want %v", sent, want)
	}

	id := "c1"
	got, diags := clusterStateFrom(ctx, k0rdent.Cluster{Id: id, Tags: *sent}, plan,
		knownDefaults(map[string]string{"owner": "pipeline"}))
	if diags.HasError() {
		t.Fatal(diags)
	}
	wantTags(t, "tags", got.Tags, map[string]string{"team": "ml"})
	wantTags(t, "tags_all", got.TagsAll, map[string]string{"team": "ml", "owner": "pipeline"})
}

// The same for the other three kinds: the token is in what the platform
// answers, and never in tags_all.
func TestEveryTaggedKindKeepsTheTokenOutOfTagsAll(t *testing.T) {
	ctx := context.Background()
	defaults := knownDefaults(map[string]string{"owner": "pipeline"})
	answer := map[string]string{"owner": "pipeline", createTokenTag(): "t0k3n"}
	want := map[string]string{"owner": "pipeline"}

	net := k0rdent.Network{}
	if err := net.FromNetworkNVLink(k0rdent.NetworkNVLink{Id: ptr("n1"), Tags: &answer, Type: k0rdent.NetworkNVLinkTypeNvlink}); err != nil {
		t.Fatal(err)
	}
	n, diags := networkStateFrom(ctx, net, networkModel{}, defaults)
	if diags.HasError() {
		t.Fatal(diags)
	}
	wantTags(t, "network tags_all", n.TagsAll, want)

	freeform := map[string]any{"owner": "pipeline", createTokenTag(): "t0k3n"}
	g, diags := securityGroupStateFrom(ctx, k0rdent.SecurityGroup{Id: "g1", Tags: freeform}, securityGroupModel{}, defaults)
	if diags.HasError() {
		t.Fatal(diags)
	}
	wantTags(t, "security group tags_all", g.TagsAll, want)

	p, diags := vpcPeeringStateFrom(ctx, k0rdent.VPCPeering{Id: ptr("p1"), Tags: &answer}, "vpc", vpcPeeringModel{}, defaults)
	if diags.HasError() {
		t.Fatal(diags)
	}
	wantTags(t, "peering tags_all", p.TagsAll, want)
}

// A PATCH is decided by tags_all, the tags the platform will hold. A default
// that changes needs one, and a tag moved between the provider block and the
// resource with the same value needs none.
func TestPatchFollowsTagsAll(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name                string
		planTags, planAll   map[string]string
		stateTags, stateAll map[string]string
		wantChanged         bool
		wantSent            map[string]string
		wantNothingSent     bool
	}{
		{
			name:     "a default changed",
			planTags: map[string]string{"team": "ml"}, planAll: map[string]string{"team": "ml", "owner": "new"},
			stateTags: map[string]string{"team": "ml"}, stateAll: map[string]string{"team": "ml", "owner": "old"},
			wantChanged: true, wantSent: map[string]string{"team": "ml", "owner": "new"},
		},
		{
			name:     "the last default removed",
			planTags: nil, planAll: map[string]string{},
			stateTags: nil, stateAll: map[string]string{"owner": "old"},
			wantChanged: true, wantSent: map[string]string{},
		},
		{
			name:     "a tag moved from the provider to the resource",
			planTags: map[string]string{"owner": "pipeline"}, planAll: map[string]string{"owner": "pipeline"},
			stateTags: nil, stateAll: map[string]string{"owner": "pipeline"},
			wantChanged: false, wantSent: map[string]string{"owner": "pipeline"},
		},
		{
			name:     "no tags either side",
			planTags: nil, planAll: map[string]string{},
			stateTags: nil, stateAll: map[string]string{},
			wantChanged: false, wantNothingSent: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan := tenantMetadata{Tags: tagMap(t, tc.planTags), TagsAll: tagMap(t, tc.planAll)}
			state := tenantMetadata{Tags: tagMap(t, tc.stateTags), TagsAll: tagMap(t, tc.stateAll)}
			out, changed, diags := plan.patch(ctx, state)
			if diags.HasError() {
				t.Fatal(diags)
			}
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			switch {
			case tc.wantNothingSent && out.Tags != nil:
				t.Errorf("sent %v, want nothing", *out.Tags)
			case !tc.wantNothingSent && (out.Tags == nil || !reflect.DeepEqual(*out.Tags, tc.wantSent)):
				t.Errorf("sent %v, want %v", out.Tags, tc.wantSent)
			}
		})
	}
}

// A peering's tags can't change after creation, so a change to `tags` that
// leaves what the platform holds the same, such as writing out a tag a
// default already supplies, mustn't replace it.
func TestPeeringTagsReplaceOnlyWhenThePlatformsTagsWouldChange(t *testing.T) {
	unknown := defaultTags{unknown: true}
	for _, tc := range []struct {
		name                string
		stateTags, stateAll map[string]string
		planTags            map[string]string
		defaults            defaultTags
		want                bool
	}{
		{"a default written out", nil, map[string]string{"owner": "pipeline"},
			map[string]string{"owner": "pipeline"}, unknown, false},
		// The migration a configuration makes when it adopts default_tags:
		// a tag moved out of the peering into default_tags with the same
		// value. The platform's tags don't change, so neither does the
		// peering, which has no allow_replace to stop a replacement.
		{"a tag moved into default_tags", map[string]string{"team": "ml"},
			map[string]string{"team": "ml"}, nil, knownDefaults(map[string]string{"team": "ml"}), false},
		{"a tag moved into default_tags with another value", map[string]string{"team": "ml"},
			map[string]string{"team": "ml"}, nil, knownDefaults(map[string]string{"team": "ai"}), true},
		// Without the configured defaults, only state can say what a default
		// supplies, so a tag the resource stops writing is a change.
		{"a tag dropped, defaults not known", map[string]string{"owner": "pipeline"},
			map[string]string{"owner": "pipeline"}, nil, unknown, true},
		// The peering keeps an old default's value when the defaults change,
		// so an edit that leaves that value where it is isn't a change either.
		{"tags = {} while it keeps an old default", nil, map[string]string{"owner": "old"},
			map[string]string{}, knownDefaults(map[string]string{"owner": "new"}), false},
		{"a new tag", nil, map[string]string{"owner": "pipeline"},
			map[string]string{"team": "ml"}, knownDefaults(map[string]string{"owner": "pipeline"}), true},
		{"a default overridden", nil, map[string]string{"owner": "pipeline"},
			map[string]string{"owner": "me"}, knownDefaults(map[string]string{"owner": "pipeline"}), true},
		{"a tag removed", map[string]string{"team": "ml"}, map[string]string{"team": "ml"}, nil, knownDefaults(nil), true},
		{"a state from before tags_all", map[string]string{"team": "ml"}, nil,
			map[string]string{"team": "ai"}, knownDefaults(nil), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tagsChangeReplaces(tagMap(t, tc.stateTags), tagMap(t, tc.stateAll), tagMap(t, tc.planTags), tc.defaults)
			if got != tc.want {
				t.Errorf("replaces = %v, want %v", got, tc.want)
			}
		})
	}
}

// The framework builds a resource's schema, and so its plan modifiers, on an
// instance that's never configured, so the peering's tags modifier can't
// reach the provider's defaults through the resource. It reaches the defaults through
// the provider that registered it, and this is the migration above end to end:
// configure the provider, take the peering from its registrations, and run
// the modifier the plan would.
func TestThePeeringTagsModifierSeesTheConfiguredDefaults(t *testing.T) {
	ctx := context.Background()
	p, obj := providerSchema(t)
	p.lookupEnv = envMap(saEnv(nil))
	var configured fwprovider.ConfigureResponse
	p.Configure(ctx, fwprovider.ConfigureRequest{Config: configWith(t, obj, map[string]tftypes.Value{
		defaultTagsAttribute: tfTags(map[string]string{"team": "ml"}),
	})}, &configured)
	if configured.Diagnostics.HasError() {
		t.Fatal(configured.Diagnostics)
	}

	var peering resource.Resource
	for _, f := range p.Resources(ctx) {
		if r, ok := f().(*vpcPeeringResource); ok {
			peering = r
		}
	}
	if peering == nil {
		t.Fatal("no peering is registered")
	}
	prior := map[string]tftypes.Value{
		"id": str("p1"), "region": str("r"), "project": str("p"), "vpc": str("v"), "remote_vpc": str("/remote"),
		"tags":           tfTags(map[string]string{"team": "ml"}),
		tagsAllAttribute: tfTags(map[string]string{"team": "ml"}),
	}
	stateRaw, s := testValue(t, peering, prior)
	planRaw, _ := testValue(t, peering, with(prior, map[string]tftypes.Value{"tags": tfTags(nil)}))
	attr, ok := s.Schema.Attributes["tags"].(schema.MapAttribute)
	if !ok || len(attr.PlanModifiers) != 1 {
		t.Fatalf("tags = %#v, want a map with one plan modifier", s.Schema.Attributes["tags"])
	}
	req := planmodifier.MapRequest{
		Path:        path.Root("tags"),
		State:       tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:        tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
		Config:      tfsdk.Config{Raw: planRaw, Schema: s.Schema},
		StateValue:  tagMap(t, map[string]string{"team": "ml"}),
		PlanValue:   types.MapNull(types.StringType),
		ConfigValue: types.MapNull(types.StringType),
	}
	resp := &planmodifier.MapResponse{PlanValue: req.PlanValue}
	attr.PlanModifiers[0].PlanModifyMap(ctx, req, resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if resp.RequiresReplace {
		t.Error("moving a tag into default_tags with the same value replaces the peering")
	}
}

// A state from before tags_all had no defaults to merge, so the platform held
// exactly its tags. Starting tags_all there means a plan run without a
// refresh proposes nothing, and each tagged kind takes the same step.
func TestAnUpgradedStateStartsTagsAllAsItsTags(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state map[string]any
		want  map[string]any
	}{
		{"tags", map[string]any{"tags": map[string]any{"team": "ml"}}, map[string]any{"team": "ml"}},
		{"no tags", map[string]any{"tags": nil}, map[string]any{}},
		{"no tags attribute", map[string]any{}, map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tagsAllAdded.apply(tc.state); err != nil {
				t.Fatal(err)
			}
			if got := tc.state[tagsAllAttribute]; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("tags_all = %v, want %v", got, tc.want)
			}
		})
	}
	for _, suffix := range []string{clusterResourceSuffix, networkResourceSuffix, securityGroupResourceSuffix, vpcPeeringResourceSuffix} {
		if !slices.ContainsFunc(stateHistories[suffix].steps, func(s stateStep) bool { return s.why == tagsAllAdded.why }) {
			t.Errorf("%s: no step adds tags_all", suffix)
		}
	}
}

func tfTags(m map[string]string) tftypes.Value {
	typ := tftypes.Map{ElementType: tftypes.String}
	if m == nil {
		return tftypes.NewValue(typ, nil)
	}
	out := make(map[string]tftypes.Value, len(m))
	for k, v := range m {
		out[k] = str(v)
	}
	return tftypes.NewValue(typ, out)
}

func plannedTagsAll(t *testing.T, resp resource.ModifyPlanResponse) types.Map {
	t.Helper()
	var got types.Map
	if d := resp.Plan.GetAttribute(context.Background(), path.Root(tagsAllAttribute), &got); d.HasError() {
		t.Fatal(d)
	}
	return got
}

func taggedMeta(defaults map[string]string) *Meta {
	return &Meta{Region: "r", Project: "p", DefaultTags: knownDefaults(defaults)}
}

// The plan is where a reader sees the merge, so it's ModifyPlan that makes
// it, on each of the three kinds whose tags change in place. A change to the
// provider's defaults alone, with nothing in the resource's configuration
// moved, plans the new tags_all and marks the state the PATCH moves unknown.
func TestADefaultsChangeAlonePlansTheNewTagsAll(t *testing.T) {
	prior := map[string]tftypes.Value{
		"id": str("x1"), "region": str("r"), "project": str("p"),
		"tags":           tfTags(map[string]string{"team": "ml"}),
		tagsAllAttribute: tfTags(map[string]string{"team": "ml", "owner": "old"}),
		"state":          str("active"),
	}
	want := map[string]string{"team": "ml", "owner": "new"}
	for _, tc := range []struct {
		name  string
		r     resource.Resource
		extra map[string]tftypes.Value
	}{
		{"network", &networkResource{meta: taggedMeta(map[string]string{"owner": "new"})}, nil},
		{"security group", &securityGroupResource{meta: taggedMeta(map[string]string{"owner": "new"})}, nil},
		{"cluster", &clusterResource{meta: taggedMeta(map[string]string{"owner": "new"})},
			clusterValues(verRef(1), verRef(1), 2, "active")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := with(tc.extra, prior)
			resp := planOf(t, tc.r, before, before)
			if resp.Diagnostics.HasError() {
				t.Fatalf("plan failed: %v", resp.Diagnostics)
			}
			wantTags(t, "tags_all", plannedTagsAll(t, resp), want)
			if len(resp.RequiresReplace) > 0 {
				t.Errorf("a defaults change replaces: %v", resp.RequiresReplace)
			}
			// The update waits out what the PATCH moves, so a settled state
			// comes back as it was.
			var moved types.String
			resp.Plan.GetAttribute(context.Background(), path.Root("state"), &moved)
			if moved.ValueString() != "active" {
				t.Errorf("state = %v, want \"active\": the update settles it", moved)
			}

			same := planOf(t, tc.r, with(before, map[string]tftypes.Value{tagsAllAttribute: tfTags(want)}),
				with(before, map[string]tftypes.Value{tagsAllAttribute: tfTags(want)}))
			if same.Diagnostics.HasError() {
				t.Fatal(same.Diagnostics)
			}
			same.Plan.GetAttribute(context.Background(), path.Root("state"), &moved)
			if moved.IsUnknown() {
				t.Error("with the defaults unchanged, state went unknown, so the plan isn't empty")
			}
		})
	}
}

// A peering's tags are set at creation only, so a defaults change leaves the
// tags it has, and the plan says so with a warning rather than replacing it.
// A peering being replaced anyway is created with the new merge.
func TestAPeeringKeepsItsTagsUnlessReplaced(t *testing.T) {
	r := &vpcPeeringResource{meta: taggedMeta(map[string]string{"owner": "new"})}
	prior := map[string]tftypes.Value{
		"id": str("p1"), "region": str("r"), "project": str("p"), "vpc": str("v"), "remote_vpc": str("/remote"),
		"tags":           tfTags(nil),
		tagsAllAttribute: tfTags(map[string]string{"owner": "old"}),
	}
	kept := planOf(t, r, prior, prior)
	if kept.Diagnostics.HasError() {
		t.Fatal(kept.Diagnostics)
	}
	wantTags(t, "tags_all", plannedTagsAll(t, kept), map[string]string{"owner": "old"})
	if w := kept.Diagnostics.Warnings(); len(w) != 1 || !strings.Contains(w[0].Detail(), `owner = "new"`) {
		t.Errorf("warnings = %v, want one naming the defaults it doesn't apply", w)
	}

	replaced := planOf(t, r, prior, with(prior, map[string]tftypes.Value{"remote_vpc": str("/elsewhere")}))
	if replaced.Diagnostics.HasError() {
		t.Fatal(replaced.Diagnostics)
	}
	wantTags(t, "tags_all", plannedTagsAll(t, replaced), map[string]string{"owner": "new"})

	matching := &vpcPeeringResource{meta: taggedMeta(map[string]string{"owner": "old"})}
	if quiet := planOf(t, matching, prior, prior); len(quiet.Diagnostics) > 0 {
		t.Errorf("diagnostics with the defaults unchanged: %v", quiet.Diagnostics)
	}
}

// Configure takes default_tags whether or not it's known yet, and refuses the
// create token's key.
func TestConfigureReadsDefaultTags(t *testing.T) {
	meta, resp := configure(t, map[string]tftypes.Value{
		defaultTagsAttribute: tfTags(map[string]string{"owner": "pipeline"}),
	}, saEnv(nil))
	if resp.Diagnostics.HasError() || meta == nil {
		t.Fatalf("configure failed: %v", resp.Diagnostics)
	}
	if !reflect.DeepEqual(meta.DefaultTags, knownDefaults(map[string]string{"owner": "pipeline"})) {
		t.Errorf("defaults = %+v", meta.DefaultTags)
	}

	meta, resp = configure(t, map[string]tftypes.Value{
		defaultTagsAttribute: tftypes.NewValue(tftypes.Map{ElementType: tftypes.String}, tftypes.UnknownValue),
	}, saEnv(nil))
	if resp.Diagnostics.HasError() || meta == nil || !meta.DefaultTags.unknown {
		t.Errorf("an unknown default_tags: meta %+v, %v; want configured with defaults unknown", meta, resp.Diagnostics)
	}

	var v validator.MapResponse
	reservedTagKey{}.ValidateMap(context.Background(), validator.MapRequest{
		Path:        path.Root(defaultTagsAttribute),
		ConfigValue: tagMap(t, map[string]string{createTokenTag(): "mine"}),
	}, &v)
	if !v.Diagnostics.HasError() {
		t.Error("the create token's key was accepted")
	}
}

// A default_tags not known at plan plans every security group as an update,
// since tags_all can't be known. When the apply resolves it to what the group
// already holds, and nothing else moved, there's nothing to send.
func TestASecurityGroupUpdateThatChangesNothingSendsNothing(t *testing.T) {
	var rec patchRecorder
	server := rec.server(t, map[string]any{
		"id": "g1", "state": "active", "ruleCount": 0, "ownerKind": "tenant",
		"tags":  map[string]any{"team": "ml", "owner": "pipeline"},
		"rules": map[string]any{"ingress": []any{}, "egress": []any{}},
	})
	defer server.Close()

	r := &securityGroupResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	state := map[string]tftypes.Value{
		"id": str("g1"), "region": str("test"), "project": str("test"), "state": str("active"),
		"tags":           tfTags(map[string]string{"team": "ml"}),
		tagsAllAttribute: tfTags(map[string]string{"team": "ml", "owner": "pipeline"}),
	}
	stateRaw, s := testValue(t, r, state)
	planRaw, _ := testValue(t, r, with(state, map[string]tftypes.Value{
		"state": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
	}))
	resp := resource.UpdateResponse{State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}
	r.Update(context.Background(), resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("update: %v", resp.Diagnostics.Errors())
	}
	if rec.sent != nil {
		t.Errorf("sent a PATCH %v, want none", rec.sent)
	}

	// A rule that moved still goes.
	rec.sent = nil
	ruled := with(state, map[string]tftypes.Value{"ingress": ingressValue(t, s)})
	planRaw, _ = testValue(t, r, ruled)
	r.Update(context.Background(), resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}, &resp)
	if rec.sent == nil {
		t.Error("a rule change sent no PATCH")
	}
}

// ingressValue is one ingress rule, typed from the schema.
func ingressValue(t *testing.T, s resource.SchemaResponse) tftypes.Value {
	t.Helper()
	obj, ok := s.Schema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema is not an object")
	}
	list, ok := obj.AttributeTypes["ingress"].(tftypes.List)
	if !ok {
		t.Fatalf("ingress is %T", obj.AttributeTypes["ingress"])
	}
	rule, ok := list.ElementType.(tftypes.Object)
	if !ok {
		t.Fatalf("an ingress rule is %T", list.ElementType)
	}
	values := map[string]tftypes.Value{}
	for name, typ := range rule.AttributeTypes {
		values[name] = tftypes.NewValue(typ, nil)
	}
	for name, v := range map[string]string{"name": "ssh", "action": "permit", "protocol": "tcp",
		"source_prefix": "10.0.0.0/8", "destination_prefix": "0.0.0.0/0"} {
		values[name] = str(v)
	}
	return tftypes.NewValue(list, []tftypes.Value{tftypes.NewValue(rule, values)})
}

// Every shape the contract answers tags in, read every way the provider
// reads them. The create token never reaches any reading, a pointer the
// platform left nil is no tags, and a required map left empty is the same.
func TestATagAnswerReadsTheSameWhateverItsShape(t *testing.T) {
	token := createTokenTag()
	none := map[string]string(nil)
	empty := map[string]string{}
	team := map[string]string{"team": "a"}
	configured := tagMap(t, map[string]string{"team": "a"})
	null := types.MapNull(types.StringType)
	for _, tc := range []struct {
		name       string
		answer     tagAnswer
		prior      types.Map
		resource   map[string]string
		dataSource map[string]string
		list       map[string]string
		warnings   int
	}{
		{"pointer left out", tagAnswerOf[string](nil), null, none, none, none, 0},
		{"pointer to {}", tagAnswerOf(&map[string]string{}), null, empty, empty, none, 0},
		{"pointer to the token alone", tagAnswerOf(&map[string]string{token: "t"}), null, empty, none, none, 0},
		{"pointer to the token and a tag", tagAnswerOf(&map[string]string{token: "t", "team": "a"}), null, team, team, team, 0},
		{"required {}", tagAnswerOfRequired(map[string]string{}), null, none, none, none, 0},
		{"required token alone", tagAnswerOfRequired(map[string]string{token: "t"}), null, empty, none, none, 0},
		{"required token and a tag", tagAnswerOfRequired(map[string]string{token: "t", "team": "a"}), null, team, team, team, 0},
		{"freeform {}", tagAnswerOfRequired(map[string]any{}), null, none, none, none, 0},
		{"freeform token alone", tagAnswerOfRequired(map[string]any{token: "t"}), null, empty, none, none, 0},
		{"freeform token and a tag", tagAnswerOfRequired(map[string]any{token: "t", "team": "a"}), null, team, team, team, 0},
		{"freeform tag beside a number", tagAnswerOfRequired(map[string]any{"team": "a", "n": 1.0}), null, team, team, team, 1},
		{"freeform numbers alone, nothing configured", tagAnswerOfRequired(map[string]any{"n": 1.0, "m": true}), null, empty, empty, none, 2},
		{"freeform numbers alone keep what was configured", tagAnswerOfRequired(map[string]any{token: "t", "n": 1.0}), configured, team, empty, none, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, diags := tc.answer.forResource(tc.prior)
			wantTags(t, "resource", got, tc.resource)
			if diags.HasError() || diags.WarningsCount() != tc.warnings {
				t.Errorf("resource diagnostics = %v, want %d warnings", diags, tc.warnings)
			}
			wantTags(t, "data source", tc.answer.forDataSource(), tc.dataSource)
			wantTags(t, "list", tc.answer.forList(), tc.list)
			if !reflect.DeepEqual(tc.answer.tags, tc.list) && len(tc.answer.tags)+len(tc.list) > 0 {
				t.Errorf("filter = %v, want %v", tc.answer.tags, tc.list)
			}
			if _, ok := tc.answer.tags[token]; ok {
				t.Errorf("the filter sees the create token")
			}
		})
	}
}
