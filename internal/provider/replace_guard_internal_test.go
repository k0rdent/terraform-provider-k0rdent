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
	modifier, ok := r.(resource.ResourceWithModifyPlan)
	if !ok {
		t.Fatalf("%T has no ModifyPlan", r)
	}
	modifier.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: planRaw, Schema: s.Schema},
		Plan:   tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
		State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
	}, &resp)
	return resp
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

// The refusal and the opt-in, on each tenant-state resource, through the
// resource's own ModifyPlan. Every change here is one Terraform would make by
// replacement.
func TestTenantStateReplacementIsRefused(t *testing.T) {
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
			if got := errorSummaries(refused.Diagnostics); len(got) != 1 || !strings.Contains(got[0], "Changing `"+tc.attribute+"` would replace") {
				t.Fatalf("errors = %q, want one refusal naming %s", got, tc.attribute)
			}
			if d, ok := refused.Diagnostics.Errors()[0].(diag.DiagnosticWithPath); !ok || d.Path().String() != tc.attribute {
				t.Errorf("the refusal does not point at %s: %v", tc.attribute, refused.Diagnostics.Errors()[0])
			}

			permitted := planOf(t, tc.r, tc.prior, with(tc.prior, with(tc.change, map[string]tftypes.Value{allowReplaceAttribute: allow})))
			if permitted.Diagnostics.HasError() {
				t.Errorf("refused with the permission: %q", errorSummaries(permitted.Diagnostics))
			}

			// A value known only at apply cannot be shown to be true.
			pending := planOf(t, tc.r, tc.prior, with(tc.prior, with(tc.change, map[string]tftypes.Value{allowReplaceAttribute: unknown})))
			if !pending.Diagnostics.HasError() {
				t.Error("an unknown allow_replace let the replacement through")
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
		if got := planOf(t, newClusterResource(), prior, with(prior, change)); got.Diagnostics.HasError() {
			t.Errorf("%s: refused: %q", name, errorSummaries(got.Diagnostics))
		}
	}
}

// A moved region or project is decided in ModifyPlan rather than by a schema
// modifier, so the guard reads it from the response.
func TestAScopeMoveIsRefused(t *testing.T) {
	r := guardReplacements(&subnetResource{meta: &Meta{Region: "test", Project: "test"}}, subnetTenantState)
	prior := map[string]tftypes.Value{"id": str("s1"), "network": str("n1"), "region": str("test"), "project": str("test")}
	resp := planOf(t, r, prior, with(prior, map[string]tftypes.Value{"project": str("elsewhere")}))
	if got := errorSummaries(resp.Diagnostics); len(got) != 1 || got[0] != "Changing `project` would replace this subnet" {
		t.Errorf("errors = %q", got)
	}
	if !slices.ContainsFunc(resp.RequiresReplace, path.Root("project").Equal) {
		t.Error("the move is no longer planned as a replacement")
	}
}

// Terraform plans a refresh before every destroy as if applying, and a
// refusal there stops the destroy. The refusal says how to destroy anyway.
func TestTheRefusalSaysHowToDestroyAnyway(t *testing.T) {
	var diags diag.Diagnostics
	refuseReplacement(&diags, path.Root("ssh_access"), types.StringNull(), types.StringValue("x"), clusterTenantState)
	if !diags.HasError() || !strings.Contains(diags.Errors()[0].Detail(), "terraform destroy -refresh=false") {
		t.Errorf("the refusal doesn't say how to destroy anyway:\n%v", diags)
	}
}
