package provider

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/listplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// The guard finds replacements by the framework's modifier type. A framework
// upgrade that renamed it would leave every resource unguarded and every other
// test green, so the name is pinned here for each kind this provider uses.
func TestReplacementModifiersAreRecognised(t *testing.T) {
	for name, m := range map[string]any{
		"string":    stringplanmodifier.RequiresReplace(),
		"string if": stringplanmodifier.RequiresReplaceIf(nil, "", ""),
		"int64":     int64planmodifier.RequiresReplace(),
		"bool":      boolplanmodifier.RequiresReplace(),
		"list":      listplanmodifier.RequiresReplace(),
		"set":       setplanmodifier.RequiresReplace(),
		"map":       mapplanmodifier.RequiresReplace(),
		"object":    objectplanmodifier.RequiresReplace(),
		"cluster":   clusterTypeRefReplacement(),
	} {
		if !IsReplacementModifier(m) {
			t.Errorf("%s: %T is not recognised as a replacement", name, m)
		}
	}
	for name, m := range map[string]any{
		"use state": stringplanmodifier.UseStateForUnknown(),
		"pkey":      recordPkey{},
		"nil":       nil,
	} {
		if IsReplacementModifier(m) {
			t.Errorf("%s: %T is taken for a replacement", name, m)
		}
	}
}

// tenantStateResources is every resource that holds tenant data, runs tenant
// workloads, or is the identity they authenticate as. The conformance matrix
// keeps its own copy, so that dropping one here does not also drop the check
// on it.
var tenantStateResources = map[string]func() resource.Resource{
	clusterResourceSuffix:       newClusterResource,
	instanceGroupResourceSuffix: newInstanceGroupResource,
	fileStoreResourceSuffix:     newFileStoreResource,
	subnetResourceSuffix:        newSubnetResource,
	networkResourceSuffix:       newNetworkResource,
	projectResourceSuffix:       newProjectResource,
}

func schemaOf(t *testing.T, r resource.Resource) schema.Schema {
	t.Helper()
	var resp resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("schema: %v", resp.Diagnostics)
	}
	return resp.Schema
}

// Which attributes replace which resource, written out. This is the table the
// generated documentation states, and a change to it is a change in what a
// configuration edit can destroy, so it should be a visible one.
func TestReplacingAttributesPerResource(t *testing.T) {
	want := map[string][]string{
		clusterResourceSuffix:           {"cluster_type", "id", "project", "region", "ssh_access"},
		instanceGroupResourceSuffix:     {"cluster_type", "id", "node_pool", "project", "region", "ssh_access"},
		fileStoreResourceSuffix:         {"id", "project", "region", "share_type"},
		subnetResourceSuffix:            {"address_pool", "allocate_gateway", "description", "display_name", "exclude_ranges", "id", "network", "prefix_length", "project", "region"},
		networkResourceSuffix:           {"evpn", "id", "infiniband", "nvlink", "project", "region"},
		projectResourceSuffix:           {"id", "organization_id", "region"},
		securityGroupResourceSuffix:     {"id", "project", "region"},
		vpcPeeringResourceSuffix:        {"description", "display_name", "id", "project", "region", "remote_vpc", "tags", "vpc"},
		vpcSecurityGroupsResourceSuffix: {"project", "region", "vpc"},
		// Never secret_wo: a new secret is a rotation of the same subscription.
		webhookSubscriptionResourceSuffix: {"event_types", "url"},
	}
	p := New("test")()
	for _, newResource := range p.Resources(context.Background()) {
		r := newResource()
		var meta resource.MetadataResponse
		r.Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &meta)
		suffix := strings.TrimPrefix(meta.TypeName, ResourcePrefix())
		got := ReplacingAttributes(schemaOf(t, r))
		if !slices.Equal(got, want[suffix]) {
			t.Errorf("%s replaces on %q, want %q", suffix, got, want[suffix])
		}
	}
}

// Every tenant-state resource takes the opt-in, and nothing else does: an
// opt-in on a resource the guard does not cover would promise a refusal that
// never comes.
func TestOnlyTenantStateResourcesTakeTheOptIn(t *testing.T) {
	p := New("test")()
	for _, newResource := range p.Resources(context.Background()) {
		r := newResource()
		var meta resource.MetadataResponse
		r.Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &meta)
		suffix := strings.TrimPrefix(meta.TypeName, ResourcePrefix())
		s := schemaOf(t, r)
		a, has := s.Attributes[allowReplaceAttribute]
		_, tenant := tenantStateResources[suffix]
		switch {
		case tenant && !has:
			t.Errorf("%s holds tenant state and has no %s", suffix, allowReplaceAttribute)
		case !tenant && has:
			t.Errorf("%s holds no tenant state and has %s", suffix, allowReplaceAttribute)
		case has:
			if b, ok := a.(schema.BoolAttribute); !ok || !b.Optional || b.Computed || b.Default != nil {
				t.Errorf("%s.%s must be an optional bool with no default, so leaving it out keeps state null", suffix, allowReplaceAttribute)
			}
		}
		if tenant {
			if _, ok := r.(interface{ guardedTenantState() tenantState }); !ok {
				t.Errorf("%s isn't registered through guardReplacements", suffix)
			}
			if _, ok := r.(resource.ResourceWithModifyPlan); !ok {
				t.Errorf("%s has no ModifyPlan to run the guard from", suffix)
			}
			if !strings.Contains(s.MarkdownDescription, "`allow_replace = true`") {
				t.Errorf("%s's description does not say how to permit a replacement", suffix)
			}
		}
	}
}

// replacedWithoutAsking is every registered resource that has an attribute
// whose change replaces it and isn't guarded, each with the reason. A new
// resource with a replacing attribute has to be guarded or named here, so
// leaving it unguarded is a decision someone wrote down rather than a step
// someone forgot.
var replacedWithoutAsking = map[string]string{
	securityGroupResourceSuffix:     "holds no tenant state: its rules are all in configuration",
	vpcPeeringResourceSuffix:        "holds no tenant state: it's a link between two VPCs",
	vpcSecurityGroupsResourceSuffix: "holds no tenant state: it's a binding of groups to a VPC",
	webhookSubscriptionResourceSuffix: "holds no tenant state: a replacement is a new subscription to " +
		"the same events",
}

// Every registered resource that a configuration edit can replace is either
// guarded, through guardReplacements, or named in replacedWithoutAsking.
// A guarded resource is a tenant-state one, so this and
// TestOnlyTenantStateResourcesTakeTheOptIn together pin the whole list.
func TestEveryReplaceableResourceIsGuardedOrSaysWhy(t *testing.T) {
	seen := map[string]bool{}
	for _, newResource := range New("test")().Resources(context.Background()) {
		r := newResource()
		suffix := suffixOf(r)
		seen[suffix] = true
		replacing := ReplacingAttributes(schemaOf(t, r))
		_, guarded := r.(interface{ guardedTenantState() tenantState })
		why, exempt := replacedWithoutAsking[suffix]
		switch {
		case guarded && exempt:
			t.Errorf("%s is guarded and also listed as replaced without asking (%s)", suffix, why)
		case guarded:
			if _, tenant := tenantStateResources[suffix]; !tenant {
				t.Errorf("%s is guarded but isn't a tenant-state resource", suffix)
			}
		case len(replacing) > 0 && !exempt:
			t.Errorf("%s is replaced by a change to %q and isn't guarded. Register it through "+
				"guardReplacements, or add it to replacedWithoutAsking with the reason", suffix, replacing)
		case exempt && len(replacing) == 0:
			t.Errorf("%s is listed as replaced without asking, and nothing replaces it", suffix)
		}
	}
	for suffix := range replacedWithoutAsking {
		if !seen[suffix] {
			t.Errorf("%s is listed as replaced without asking and isn't registered", suffix)
		}
	}
}

func suffixOf(r resource.Resource) string {
	var meta resource.MetadataResponse
	r.Metadata(context.Background(), resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &meta)
	return strings.TrimPrefix(meta.TypeName, ResourcePrefix())
}

// frameworkInterfaces is every optional resource interface the framework
// looks for. The framework decides what a resource can do by which of these
// it implements, so a wrapper that dropped one would quietly drop a feature.
var frameworkInterfaces = map[string]reflect.Type{
	"Configure":        reflect.TypeFor[resource.ResourceWithConfigure](),
	"ConfigValidators": reflect.TypeFor[resource.ResourceWithConfigValidators](),
	"Identity":         reflect.TypeFor[resource.ResourceWithIdentity](),
	"ImportState":      reflect.TypeFor[resource.ResourceWithImportState](),
	"ModifyPlan":       reflect.TypeFor[resource.ResourceWithModifyPlan](),
	"MoveState":        reflect.TypeFor[resource.ResourceWithMoveState](),
	"UpgradeIdentity":  reflect.TypeFor[resource.ResourceWithUpgradeIdentity](),
	"UpgradeState":     reflect.TypeFor[resource.ResourceWithUpgradeState](),
	"ValidateConfig":   reflect.TypeFor[resource.ResourceWithValidateConfig](),
}

// The guard keeps every interface the resource it wraps has, and adds only
// ModifyPlan, which it runs the refusal from, and ValidateConfig, which
// does nothing when the resource has none.
func TestTheGuardKeepsWhatTheResourceImplements(t *testing.T) {
	for _, newResource := range New("test")().Resources(context.Background()) {
		r := newResource()
		g := guardedOf(r)
		if g == nil {
			continue
		}
		for name, iface := range frameworkInterfaces {
			has := reflect.TypeOf(g.inner).Implements(iface)
			wraps := reflect.TypeOf(r).Implements(iface)
			switch {
			case has && !wraps:
				t.Errorf("%s implements %s and the guarded one doesn't", suffixOf(r), name)
			case wraps && !has && name != "ModifyPlan" && name != "ValidateConfig":
				t.Errorf("the guarded %s implements %s and the resource doesn't", suffixOf(r), name)
			}
		}
	}
}

func guardedOf(r resource.Resource) *guardedResource {
	switch g := r.(type) {
	case *guardedResource:
		return g
	case guardedMovableResource:
		return g.guardedResource
	}
	return nil
}

// The guard reaches top-level attributes and those inside a single nested
// object. A replacement modifier inside a list, set or map of objects would
// have a path per element, which it does not walk.
func TestNoReplacementTheGuardCannotReach(t *testing.T) {
	p := New("test")()
	for _, newResource := range p.Resources(context.Background()) {
		r := newResource()
		if got := UnguardableReplacements(schemaOf(t, r)); len(got) > 0 {
			t.Errorf("%T: replacement modifiers the guard cannot reach: %q", r, got)
		}
	}
}

// planOf runs a resource's ModifyPlan from prior to planned, with planned as
// the configuration too.
func planOf(t *testing.T, r resource.Resource, prior, planned map[string]tftypes.Value) resource.ModifyPlanResponse {
	t.Helper()
	stateRaw, s := testValue(t, r, prior)
	planRaw, _ := testValue(t, r, planned)
	resp := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}
	freshPrivate(t, &resp.Private, nil)
	modifier, ok := r.(resource.ResourceWithModifyPlan)
	if !ok {
		t.Fatalf("%T has no ModifyPlan", r)
	}
	req := resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: planRaw, Schema: s.Schema},
		Plan:   tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
		State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
	}
	runServedReplacementModifiers(t, req, &resp)
	modifier.ModifyPlan(context.Background(), req, &resp)
	return resp
}

// runServedReplacementModifiers does what the framework does before a
// resource's ModifyPlan, for the modifiers the guard wraps: runs each served
// one, which records what it drops in resp.Private. A plan test that calls
// ModifyPlan directly calls this first.
func runServedReplacementModifiers(t *testing.T, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	t.Helper()
	ctx := context.Background()
	s, ok := req.Plan.Schema.(schema.Schema)
	if !ok {
		t.Fatal("the plan's schema isn't a resource schema")
	}
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return // the framework asks nothing to replace on a create or a destroy
	}
	in := probeInput{ctx: ctx, req: req, plan: req.Plan}
	var walk func(attrs map[string]schema.Attribute, parent path.Path)
	walk = func(attrs map[string]schema.Attribute, parent path.Path) {
		for name, a := range attrs {
			p := parent.AtName(name)
			mods := reflect.ValueOf(a).FieldByName("PlanModifiers")
			for i := 0; mods.IsValid() && i < mods.Len(); i++ {
				m := mods.Index(i).Interface()
				if _, wrapped := m.(replacementWrapper); !wrapped {
					continue
				}
				var diags diag.Diagnostics
				switch m := m.(type) {
				case planmodifier.String:
					var c, pl, st types.String
					if values(in, p, &c, &pl, &st) {
						out := planmodifier.StringResponse{PlanValue: pl, Private: resp.Private}
						m.PlanModifyString(ctx, planmodifier.StringRequest{Path: p, Config: req.Config, ConfigValue: c,
							Plan: req.Plan, PlanValue: pl, State: req.State, StateValue: st, Private: resp.Private}, &out)
						diags = out.Diagnostics
					}
				case planmodifier.Int64:
					var c, pl, st types.Int64
					if values(in, p, &c, &pl, &st) {
						out := planmodifier.Int64Response{PlanValue: pl, Private: resp.Private}
						m.PlanModifyInt64(ctx, planmodifier.Int64Request{Path: p, Config: req.Config, ConfigValue: c,
							Plan: req.Plan, PlanValue: pl, State: req.State, StateValue: st, Private: resp.Private}, &out)
						diags = out.Diagnostics
					}
				case planmodifier.Bool:
					var c, pl, st types.Bool
					if values(in, p, &c, &pl, &st) {
						out := planmodifier.BoolResponse{PlanValue: pl, Private: resp.Private}
						m.PlanModifyBool(ctx, planmodifier.BoolRequest{Path: p, Config: req.Config, ConfigValue: c,
							Plan: req.Plan, PlanValue: pl, State: req.State, StateValue: st, Private: resp.Private}, &out)
						diags = out.Diagnostics
					}
				case planmodifier.List:
					var c, pl, st types.List
					if values(in, p, &c, &pl, &st) {
						out := planmodifier.ListResponse{PlanValue: pl, Private: resp.Private}
						m.PlanModifyList(ctx, planmodifier.ListRequest{Path: p, Config: req.Config, ConfigValue: c,
							Plan: req.Plan, PlanValue: pl, State: req.State, StateValue: st, Private: resp.Private}, &out)
						diags = out.Diagnostics
					}
				case planmodifier.Set:
					var c, pl, st types.Set
					if values(in, p, &c, &pl, &st) {
						out := planmodifier.SetResponse{PlanValue: pl, Private: resp.Private}
						m.PlanModifySet(ctx, planmodifier.SetRequest{Path: p, Config: req.Config, ConfigValue: c,
							Plan: req.Plan, PlanValue: pl, State: req.State, StateValue: st, Private: resp.Private}, &out)
						diags = out.Diagnostics
					}
				case planmodifier.Map:
					var c, pl, st types.Map
					if values(in, p, &c, &pl, &st) {
						out := planmodifier.MapResponse{PlanValue: pl, Private: resp.Private}
						m.PlanModifyMap(ctx, planmodifier.MapRequest{Path: p, Config: req.Config, ConfigValue: c,
							Plan: req.Plan, PlanValue: pl, State: req.State, StateValue: st, Private: resp.Private}, &out)
						diags = out.Diagnostics
					}
				case planmodifier.Object:
					var c, pl, st types.Object
					if values(in, p, &c, &pl, &st) {
						out := planmodifier.ObjectResponse{PlanValue: pl, Private: resp.Private}
						m.PlanModifyObject(ctx, planmodifier.ObjectRequest{Path: p, Config: req.Config, ConfigValue: c,
							Plan: req.Plan, PlanValue: pl, State: req.State, StateValue: st, Private: resp.Private}, &out)
						diags = out.Diagnostics
					}
				}
				resp.Diagnostics.Append(diags...)
			}
			if nested, ok := a.(schema.SingleNestedAttribute); ok {
				walk(nested.Attributes, p)
			}
		}
	}
	walk(s.Attributes, path.Empty())
}

func with(base map[string]tftypes.Value, changes map[string]tftypes.Value) map[string]tftypes.Value {
	out := make(map[string]tftypes.Value, len(base)+len(changes))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range changes {
		out[k] = v
	}
	return out
}

// replacementWarnings is the summary of each warning the guard gave.
func replacementWarnings(diags diag.Diagnostics) []string {
	var out []string
	for _, d := range diags.Warnings() {
		if strings.Contains(d.Summary(), " would replace this ") {
			out = append(out, d.Summary())
		}
	}
	return out
}

// refusalRecorded is each path the plan recorded for the Update to refuse.
func refusalRecorded(t *testing.T, private any) []string {
	t.Helper()
	p, ok := private.(privateState)
	if !ok {
		t.Fatal("private state cannot be read")
	}
	var diags diag.Diagnostics
	var out []string
	for _, r := range readRefusal(context.Background(), p, &diags) {
		out = append(out, pathOf(r.Path).String())
	}
	if diags.HasError() {
		t.Fatal(diags)
	}
	return out
}

// The warning and the opt-in, on each tenant-state resource, through the
// resource's own ModifyPlan. Every change here is one Terraform would make by
// replacement. Without the opt-in the plan warns, naming the attribute, and
// records the refusal for the apply. It never fails, so `terraform destroy`,
// which runs this plan first, goes ahead.
func TestTenantStateReplacementWarnsAndIsRecordedForTheApply(t *testing.T) {
	allow := tftypes.NewValue(tftypes.Bool, true)
	unknown := tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue)
	subnet := map[string]tftypes.Value{
		"id": str("s1"), "network": str("n1"), "address_pool": str("/pool"),
		"prefix_length": tftypes.NewValue(tftypes.Number, 24), "display_name": str("a"),
	}
	for _, tc := range []struct {
		name      string
		r         resource.Resource
		prior     map[string]tftypes.Value
		change    map[string]tftypes.Value
		attribute string
	}{
		{"subnet display name", newSubnetResource(), subnet, map[string]tftypes.Value{"display_name": str("b")}, "display_name"},
		{"subnet prefix", newSubnetResource(), subnet, map[string]tftypes.Value{"prefix_length": tftypes.NewValue(tftypes.Number, 25)}, "prefix_length"},
		{"subnet network", newSubnetResource(), subnet, map[string]tftypes.Value{"network": str("n2")}, "network"},
		{"file store class", newFileStoreResource(), map[string]tftypes.Value{"id": str("f1"), "share_type": str("general"),
			"capacity_gib": tftypes.NewValue(tftypes.Number, 10)}, map[string]tftypes.Value{"share_type": str("high-speed")}, "share_type"},
		{"cluster id", newClusterResource(), clusterValues(verRef(1), verRef(1), 2, "active"), map[string]tftypes.Value{"id": str("c2")}, "id"},
		{"cluster type", newClusterResource(), clusterValues(verRef(1), verRef(1), 2, "active"),
			map[string]tftypes.Value{"cluster_type": str("/v1/regions/test/compute/cluster-types/cpu")}, "cluster_type"},
		{"project organization", newProjectResource(), map[string]tftypes.Value{"id": str("p1"), "organization_id": str("o1")},
			map[string]tftypes.Value{"organization_id": str("o2")}, "organization_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refused := planOf(t, tc.r, tc.prior, with(tc.prior, tc.change))
			if refused.Diagnostics.HasError() {
				t.Fatalf("the plan failed, so a destroy would too: %q", errorSummaries(refused.Diagnostics))
			}
			if got := replacementWarnings(refused.Diagnostics); len(got) != 1 || !strings.Contains(got[0], "Changing `"+tc.attribute+"` would replace") {
				t.Fatalf("warnings = %q, want one naming %s", got, tc.attribute)
			}
			if d, ok := refused.Diagnostics.Warnings()[0].(diag.DiagnosticWithPath); !ok || d.Path().String() != tc.attribute {
				t.Errorf("the warning does not point at %s: %v", tc.attribute, refused.Diagnostics.Warnings()[0])
			}
			if got := refusalRecorded(t, refused.Private); !slices.Equal(got, []string{tc.attribute}) {
				t.Errorf("recorded for the apply: %q, want %s", got, tc.attribute)
			}
			if len(refused.RequiresReplace) != 0 {
				t.Errorf("RequiresReplace = %v, want the update the apply refuses", refused.RequiresReplace)
			}

			permitted := planOf(t, tc.r, tc.prior, with(tc.prior, with(tc.change, map[string]tftypes.Value{allowReplaceAttribute: allow})))
			if permitted.Diagnostics.HasError() || len(replacementWarnings(permitted.Diagnostics)) != 0 {
				t.Errorf("refused with the permission: %v", permitted.Diagnostics)
			}
			if got := refusalRecorded(t, permitted.Private); len(got) != 0 {
				t.Errorf("recorded with the permission: %q", got)
			}

			// A value known only at apply cannot be shown to be true.
			pending := planOf(t, tc.r, tc.prior, with(tc.prior, with(tc.change, map[string]tftypes.Value{allowReplaceAttribute: unknown})))
			if got := refusalRecorded(t, pending.Private); len(got) != 1 {
				t.Error("an unknown allow_replace let the replacement through")
			}
			if w := pending.Diagnostics.Warnings(); len(w) != 1 || !strings.Contains(w[0].Detail(), "known only after apply, so this plan can't count it") {
				t.Errorf("the warning doesn't say the opt-in isn't known yet: %v", w)
			}
		})
	}
}

// The other half of 3.2: a change made in place is never refused, and the
// opt-in on its own plans nothing but itself.
func TestInPlaceChangesAreNotRefused(t *testing.T) {
	prior := clusterValues(verRef(1), verRef(1), 2, "active")
	for name, change := range map[string]map[string]tftypes.Value{
		"display name": {"display_name": str("renamed")},
		"scale":        {"node_pool": workersValue(3)},
		"a version":    {"cluster_type": str(verRef(4))},
		"the opt-in":   {allowReplaceAttribute: tftypes.NewValue(tftypes.Bool, true)},
	} {
		got := planOf(t, newClusterResource(), prior, with(prior, change))
		if got.Diagnostics.HasError() || len(replacementWarnings(got.Diagnostics)) != 0 {
			t.Errorf("%s: refused: %v", name, got.Diagnostics)
		}
		if r := refusalRecorded(t, got.Private); len(r) != 0 {
			t.Errorf("%s: recorded for the apply: %q", name, r)
		}
	}
}

// A moved region or project is decided in ModifyPlan rather than by a schema
// modifier, so the guard reads it from the response, and takes it out so the
// plan is the update the apply refuses.
func TestAScopeMoveIsRefusedAtApply(t *testing.T) {
	r := guardReplacements(&subnetResource{meta: &Meta{Region: "test", Project: "test"}}, subnetTenantState)
	prior := map[string]tftypes.Value{"id": str("s1"), "network": str("n1"), "region": str("test"), "project": str("test")}
	resp := planOf(t, r, prior, with(prior, map[string]tftypes.Value{"project": str("elsewhere")}))
	if got := replacementWarnings(resp.Diagnostics); len(got) != 1 || got[0] != "Changing `project` would replace this subnet, so the apply will refuse it" {
		t.Errorf("warnings = %q", got)
	}
	if resp.Diagnostics.HasError() {
		t.Errorf("errors = %q", errorSummaries(resp.Diagnostics))
	}
	if len(resp.RequiresReplace) != 0 {
		t.Errorf("RequiresReplace = %v, want the move planned as an update", resp.RequiresReplace)
	}
	if got := refusalRecorded(t, resp.Private); !slices.Equal(got, []string{"project"}) {
		t.Errorf("recorded for the apply: %q", got)
	}

	// The move comes from the provider block, so the configuration leaves
	// `project` null. The plan still shows where it would move.
	moved := guardReplacements(&subnetResource{meta: &Meta{Region: "test", Project: "elsewhere"}}, subnetTenantState)
	stateRaw, s := testValue(t, moved, prior)
	configRaw, _ := testValue(t, moved, with(prior, map[string]tftypes.Value{"project": tftypes.NewValue(tftypes.String, nil)}))
	fromDefault := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Raw: stateRaw, Schema: s.Schema}}
	freshPrivate(t, &fromDefault.Private, nil)
	movedModifier, ok := moved.(resource.ResourceWithModifyPlan)
	if !ok {
		t.Fatal("a guarded resource has no ModifyPlan")
	}
	movedModifier.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema},
		Plan:   tfsdk.Plan{Raw: stateRaw, Schema: s.Schema},
		State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
	}, &fromDefault)
	var project types.String
	fromDefault.Plan.GetAttribute(context.Background(), path.Root("project"), &project)
	if got := refusalRecorded(t, fromDefault.Private); !slices.Equal(got, []string{"project"}) || project.ValueString() != "elsewhere" {
		t.Errorf("recorded %q, planned project %v: want the move refused and still shown", got, project)
	}

	permitted := planOf(t, r, prior, with(prior, map[string]tftypes.Value{"project": str("elsewhere"),
		allowReplaceAttribute: tftypes.NewValue(tftypes.Bool, true)}))
	if !slices.ContainsFunc(permitted.RequiresReplace, path.Root("project").Equal) {
		t.Error("a permitted move is no longer planned as a replacement")
	}
}

// The plan's warning and the apply's error each say what would replace the
// object, that the apply refuses before it sends any request, that
// `terraform destroy` isn't affected, and the two ways out.
func TestTheWarningAndTheRefusalSayWhatHappensAndWhatToDo(t *testing.T) {
	var diags diag.Diagnostics
	change := "`ssh_access` goes from nothing to \"x\"."
	warnReplacement(&diags, path.Root("ssh_access"), change, false, true, clusterTenantState)
	refuseAtApply(&diags, []refusedReplacement{{Path: []string{"ssh_access"}, Change: change}}, clusterTenantState)
	if len(diags.Warnings()) != 1 || len(diags.Errors()) != 1 {
		t.Fatalf("diagnostics = %v, want a warning and an error", diags)
	}
	for _, d := range []diag.Diagnostic{diags.Warnings()[0], diags.Errors()[0]} {
		text := d.Summary() + " " + strings.Join(strings.Fields(d.Detail()), " ")
		for _, want := range []string{
			"Changing `ssh_access` would replace this cluster",
			change,
			"Terraform would destroy this cluster and create another",
			clusterTenantState.loses,
			"any request",
			"`terraform destroy` isn't affected",
			"add `allow_replace = true` to this resource",
			"set `ssh_access` back to its current value",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s doesn't say %q:\n%s", d.Severity(), want, text)
			}
		}
		if d.Severity() == diag.SeverityWarning {
			for _, want := range []string{"unless Terraform replaces the cluster anyway, as `-replace` asks",
				"a resource this plan replaces because it depends on this change is destroyed before the apply refuses"} {
				if !strings.Contains(text, want) {
					t.Errorf("the warning doesn't say %q:\n%s", want, text)
				}
			}
		}
		if strings.Contains(text, "-refresh=false") {
			t.Errorf("%s still sends a destroy to -refresh=false:\n%s", d.Severity(), text)
		}
	}
}
