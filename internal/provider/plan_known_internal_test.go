package provider

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// planPair is prior state and a plan for one resource, built from its own
// schema. A nil prior is a create.
type planPair struct {
	state tfsdk.State
	plan  tfsdk.Plan
}

func pairOf(t *testing.T, r resource.Resource, prior, planned map[string]tftypes.Value) planPair {
	t.Helper()
	planRaw, s := testValue(t, r, planned)
	stateRaw := tftypes.NewValue(planRaw.Type(), nil)
	if prior != nil {
		stateRaw, _ = testValue(t, r, prior)
	}
	return planPair{
		state: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}
}

// get reads one top-level attribute from a plan or a state.
func get[T attr.Value](t *testing.T, from interface {
	GetAttribute(context.Context, path.Path, any) diag.Diagnostics
}, name string) T {
	t.Helper()
	var v T
	if d := from.GetAttribute(context.Background(), path.Root(name), &v); d.HasError() {
		t.Fatalf("read %s: %v", name, d)
	}
	return v
}

var unknownNumber = tftypes.NewValue(tftypes.Number, tftypes.UnknownValue)
var unknownString = tftypes.NewValue(tftypes.String, tftypes.UnknownValue)

// keepString runs a String modifier on a computed attribute the
// configuration leaves out.
func keepString(t *testing.T, m planmodifier.String, p planPair, name string) types.String {
	t.Helper()
	req := planmodifier.StringRequest{
		Path: path.Root(name), Plan: p.plan, State: p.state,
		PlanValue: get[types.String](t, p.plan, name), ConfigValue: types.StringNull(),
	}
	if !p.state.Raw.IsNull() {
		req.StateValue = get[types.String](t, p.state, name)
	}
	resp := planmodifier.StringResponse{PlanValue: req.PlanValue}
	m.PlanModifyString(context.Background(), req, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return resp.PlanValue
}

func keepInt64(t *testing.T, m planmodifier.Int64, p planPair, name string) types.Int64 {
	t.Helper()
	req := planmodifier.Int64Request{
		Path: path.Root(name), Plan: p.plan, State: p.state,
		PlanValue: get[types.Int64](t, p.plan, name), ConfigValue: types.Int64Null(),
	}
	if !p.state.Raw.IsNull() {
		req.StateValue = get[types.Int64](t, p.state, name)
	}
	resp := planmodifier.Int64Response{PlanValue: req.PlanValue}
	m.PlanModifyInt64(context.Background(), req, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return resp.PlanValue
}

func keepList(t *testing.T, m planmodifier.List, p planPair, name string) types.List {
	t.Helper()
	planned := get[types.List](t, p.plan, name)
	req := planmodifier.ListRequest{
		Path: path.Root(name), Plan: p.plan, State: p.state,
		PlanValue: planned, ConfigValue: types.ListNull(planned.ElementType(context.Background())),
	}
	if !p.state.Raw.IsNull() {
		req.StateValue = get[types.List](t, p.state, name)
	}
	resp := planmodifier.ListResponse{PlanValue: req.PlanValue}
	m.PlanModifyList(context.Background(), req, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return resp.PlanValue
}

func keepMap(t *testing.T, m planmodifier.Map, p planPair, name string) types.Map {
	t.Helper()
	planned := get[types.Map](t, p.plan, name)
	req := planmodifier.MapRequest{
		Path: path.Root(name), Plan: p.plan, State: p.state,
		PlanValue: planned, ConfigValue: types.MapNull(planned.ElementType(context.Background())),
	}
	if !p.state.Raw.IsNull() {
		req.StateValue = get[types.Map](t, p.state, name)
	}
	resp := planmodifier.MapResponse{PlanValue: req.PlanValue}
	m.PlanModifyMap(context.Background(), req, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	return resp.PlanValue
}

func webhookValues(version tftypes.Value, extra map[string]tftypes.Value) map[string]tftypes.Value {
	return with(map[string]tftypes.Value{
		"id": str("6d3e8f6a-0d6e-4a57-9d77-4b1c3f1d6a10"), "url": str("https://hooks.example.com/k0rdent"),
		"secret_wo_version": version, "rotation_overlap": tftypes.NewValue(tftypes.Bool, true),
		"secret_version": num(3), "is_active": tftypes.NewValue(tftypes.Bool, true),
		"rotated_at":                 str("2026-10-01T00:00:00Z"),
		"previous_secret_expires_at": tftypes.NewValue(tftypes.String, nil),
	}, extra)
}

// What a rename-like change keeps, and what a change to an input doesn't.
func TestKeepUnlessChanged(t *testing.T) {
	r := &webhookSubscriptionResource{}
	prior := webhookValues(num(1), nil)
	unknownComputed := map[string]tftypes.Value{"secret_version": unknownNumber, "rotated_at": unknownString,
		"previous_secret_expires_at": unknownString}
	keep := keepUnlessChanged("secret_wo_version")

	for _, tc := range []struct {
		name    string
		prior   map[string]tftypes.Value
		planned map[string]tftypes.Value
		// want is what the plan holds for rotated_at, nil for unknown.
		want *string
	}{
		{"the input as it was keeps the value",
			prior, with(webhookValues(num(1), unknownComputed), map[string]tftypes.Value{"rotation_overlap": tftypes.NewValue(tftypes.Bool, false)}),
			ptr("2026-10-01T00:00:00Z")},
		{"a new input leaves it unknown", prior, webhookValues(num(2), unknownComputed), nil},
		{"an input known only at apply leaves it unknown", prior, webhookValues(unknownNumber, unknownComputed), nil},
		{"an input taken away leaves it unknown", prior, webhookValues(tftypes.NewValue(tftypes.Number, nil), unknownComputed), nil},
		{"a create is left alone", nil, webhookValues(num(1), unknownComputed), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := keepString(t, keep, pairOf(t, r, tc.prior, tc.planned), "rotated_at")
			switch {
			case tc.want == nil && !got.IsUnknown():
				t.Errorf("rotated_at = %s, want unknown", got)
			case tc.want != nil && got.ValueString() != *tc.want:
				t.Errorf("rotated_at = %s, want %q", got, *tc.want)
			}
		})
	}

	t.Run("a value the configuration knows is never touched", func(t *testing.T) {
		p := pairOf(t, r, prior, webhookValues(num(1), map[string]tftypes.Value{"rotated_at": str("given")}))
		if got := keepString(t, keep, p, "rotated_at"); got.ValueString() != "given" {
			t.Errorf("rotated_at = %s, want the planned value", got)
		}
	})

	t.Run("whileNull keeps only a null", func(t *testing.T) {
		null := keepUnlessChanged("secret_wo_version").whileNull()
		p := pairOf(t, r, prior, webhookValues(num(1), unknownComputed))
		if got := keepString(t, null, p, "previous_secret_expires_at"); !got.IsNull() {
			t.Errorf("a null window = %s, want null", got)
		}
		open := webhookValues(num(1), map[string]tftypes.Value{"previous_secret_expires_at": str("2026-10-02T00:00:00Z")})
		p = pairOf(t, r, open, webhookValues(num(1), unknownComputed))
		if got := keepString(t, null, p, "previous_secret_expires_at"); !got.IsUnknown() {
			t.Errorf("an open window = %s, want unknown: it can close before the apply", got)
		}
	})
}

// A value the platform fills in while a resource is being made is kept only
// once it has settled: a subnet imported while `creating` has no CIDR yet.
func TestOnceSettledWaitsForTheResourceToSettle(t *testing.T) {
	r := &subnetResource{}
	subnet := func(state, cidr tftypes.Value, name string) map[string]tftypes.Value {
		return map[string]tftypes.Value{
			"id": str("s1"), "region": str("r"), "project": str("p"), "network": str("n1"),
			"state": state, "cidr": cidr, "display_name": str(name),
		}
	}
	keep := keepUnlessChanged().onceSettled(subnetStates.settle)
	null := tftypes.NewValue(tftypes.String, nil)

	p := pairOf(t, r, subnet(str("active"), str("10.0.1.0/24"), "a"), subnet(unknownString, unknownString, "b"))
	if got := keepString(t, keep, p, "cidr"); got.ValueString() != "10.0.1.0/24" {
		t.Errorf("an active subnet's cidr = %s, want it kept", got)
	}
	p = pairOf(t, r, subnet(str("creating"), null, "a"), subnet(unknownString, unknownString, "b"))
	if got := keepString(t, keep, p, "cidr"); !got.IsUnknown() {
		t.Errorf("a creating subnet's cidr = %s, want unknown: the allocation is still to come", got)
	}
	p = pairOf(t, r, subnet(null, null, "a"), subnet(unknownString, unknownString, "b"))
	if got := keepString(t, keep, p, "cidr"); !got.IsUnknown() {
		t.Errorf("with no state reported, cidr = %s, want unknown", got)
	}
}

// ruleList builds an `ingress` or `egress` list of n rules.
func ruleList(t *testing.T, r resource.Resource, list string, n int) tftypes.Value {
	t.Helper()
	var s resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &s)
	typ, ok := s.Schema.Type().TerraformType(context.Background()).(tftypes.Object).AttributeTypes[list].(tftypes.List)
	if !ok {
		t.Fatalf("%s is not a list", list)
	}
	obj, ok := typ.ElementType.(tftypes.Object)
	if !ok {
		t.Fatalf("a %s rule is not an object", list)
	}
	rules := make([]tftypes.Value, n)
	for i := range rules {
		v := map[string]tftypes.Value{}
		for name, at := range obj.AttributeTypes {
			v[name] = tftypes.NewValue(at, nil)
		}
		v["action"], v["protocol"] = str("permit"), str("tcp")
		v["source_prefix"], v["destination_prefix"] = str("10.0.0.0/8"), str("0.0.0.0/0")
		v["destination_port_range"] = str(fmt.Sprint(22 + i))
		rules[i] = tftypes.NewValue(obj, v)
	}
	return tftypes.NewValue(typ, rules)
}

// rule_count is the two lists' lengths added up, so a changed rule set plans
// the new count, and a rename the count it had.
func TestRuleCountFollowsTheRules(t *testing.T) {
	r := &securityGroupResource{}
	group := func(ingress, egress tftypes.Value, count tftypes.Value, name string) map[string]tftypes.Value {
		return map[string]tftypes.Value{
			"id": str("sg1"), "region": str("r"), "project": str("p"), "display_name": str(name),
			"ingress": ingress, "egress": egress, "rule_count": count,
		}
	}
	in2, eg1 := ruleList(t, r, "ingress", 2), ruleList(t, r, "egress", 1)
	prior := group(in2, eg1, num(3), "before")

	for _, tc := range []struct {
		name    string
		prior   map[string]tftypes.Value
		planned map[string]tftypes.Value
		want    *int64
	}{
		{"a rename keeps the count", prior, group(in2, eg1, unknownNumber, "after"), ptr(int64(3))},
		{"a changed port keeps the count", prior, group(in2, ruleList(t, r, "egress", 1), unknownNumber, "after"), ptr(int64(3))},
		{"an added rule counts it", prior, group(ruleList(t, r, "ingress", 3), eg1, unknownNumber, "before"), ptr(int64(4))},
		{"every rule removed is zero", prior, group(ruleList(t, r, "ingress", 0), ruleList(t, r, "egress", 0), unknownNumber, "before"), ptr(int64(0))},
		{"rules from an unknown for_each stay unknown",
			prior, group(tftypes.NewValue(in2.Type(), tftypes.UnknownValue), eg1, unknownNumber, "before"), nil},
		{"a count the platform never reported stays unknown when the rules change",
			group(in2, eg1, tftypes.NewValue(tftypes.Number, nil), "before"), group(ruleList(t, r, "ingress", 3), eg1, unknownNumber, "before"), nil},
		{"a create is left alone", nil, group(in2, eg1, unknownNumber, "before"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := keepInt64(t, ruleCount{}, pairOf(t, r, tc.prior, tc.planned), "rule_count")
			switch {
			case tc.want == nil && !got.IsUnknown():
				t.Errorf("rule_count = %s, want unknown", got)
			case tc.want != nil && (got.IsUnknown() || got.ValueInt64() != *tc.want):
				t.Errorf("rule_count = %s, want %d", got, *tc.want)
			}
		})
	}
}

func groupIDs(items ...string) tftypes.Value {
	typ := tftypes.List{ElementType: tftypes.String}
	out := make([]tftypes.Value, len(items))
	for i, s := range items {
		out[i] = str(s)
		if s == "?" {
			out[i] = unknownString
		}
	}
	return tftypes.NewValue(typ, out)
}

// A binding with groups in it reads back as the groups sent, and an empty
// one as a default group only a read can name.
func TestAttachedFollowsAListWithGroups(t *testing.T) {
	r := &vpcSecurityGroupsResource{}
	binding := func(groups, attached tftypes.Value) map[string]tftypes.Value {
		return map[string]tftypes.Value{
			"vpc": str("vpc-1"), "region": str("r"), "project": str("p"),
			"security_groups": groups, "attached": attached,
		}
	}
	unknownList := tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, tftypes.UnknownValue)

	for _, tc := range []struct {
		name    string
		prior   map[string]tftypes.Value
		planned map[string]tftypes.Value
		want    []string
	}{
		{"the same groups keep what was attached",
			binding(groupIDs("sg-a"), groupIDs("sg-a")), binding(groupIDs("sg-a"), unknownList), []string{"sg-a"}},
		{"an empty list kept keeps the default group",
			binding(groupIDs(), groupIDs("sg-default")), binding(groupIDs(), unknownList), []string{"sg-default"}},
		{"new groups are what's attached",
			binding(groupIDs("sg-a"), groupIDs("sg-a")), binding(groupIDs("sg-b", "sg-a"), unknownList), []string{"sg-b", "sg-a"}},
		{"emptying the list leaves the default group to the read",
			binding(groupIDs("sg-a"), groupIDs("sg-a")), binding(groupIDs(), unknownList), nil},
		{"a group known only at apply stays unknown",
			binding(groupIDs("sg-a"), groupIDs("sg-a")), binding(groupIDs("sg-a", "?"), unknownList), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := keepList(t, attachedGroups{}, pairOf(t, r, tc.prior, tc.planned), "attached")
			if tc.want == nil {
				if !got.IsUnknown() {
					t.Errorf("attached = %s, want unknown", got)
				}
				return
			}
			var ids []string
			if d := got.ElementsAs(context.Background(), &ids, false); d.HasError() {
				t.Fatalf("attached = %s: %v", got, d)
			}
			if !slices.Equal(ids, tc.want) {
				t.Errorf("attached = %v, want %v", ids, tc.want)
			}
		})
	}
}

// grantsValue builds a consumer_status map, one entry per ref, each in the
// given state with no reason.
func grantsValue(t *testing.T, r resource.Resource, state string, refs ...string) tftypes.Value {
	t.Helper()
	var s resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &s)
	typ, ok := s.Schema.Attributes["consumer_status"].GetType().TerraformType(context.Background()).(tftypes.Map)
	if !ok {
		t.Fatal("consumer_status is not a map")
	}
	elem, ok := typ.ElementType.(tftypes.Object)
	if !ok {
		t.Fatal("a consumer status is not an object")
	}
	addresses, ok := elem.AttributeTypes["resolved_addresses"].(tftypes.Set)
	if !ok {
		t.Fatal("resolved_addresses is not a set")
	}
	out := map[string]tftypes.Value{}
	for _, ref := range refs {
		out[ref] = tftypes.NewValue(elem, map[string]tftypes.Value{
			"state": str(state), "state_reason": tftypes.NewValue(tftypes.String, nil),
			"resolved_addresses": tftypes.NewValue(addresses, []tftypes.Value{str(ref)}),
		})
	}
	return tftypes.NewValue(typ, out)
}

// A file store's grants survive an update only where nothing can move them.
func TestGrantStatusKeepsOnlyActiveAddressGrants(t *testing.T) {
	r := &fileStoreResource{}
	unknownStatus := tftypes.NewValue(grantsValue(t, r, "active").Type(), tftypes.UnknownValue)
	store := func(name string, capacity int64, consumers, status tftypes.Value) map[string]tftypes.Value {
		return fileStorePlan(map[string]tftypes.Value{"display_name": str(name), "state": str("active"),
			"capacity_gib": num(capacity), "consumers": consumers, "consumer_status": status})
	}
	creating := func(m map[string]tftypes.Value) map[string]tftypes.Value {
		return with(m, map[string]tftypes.Value{"state": str("creating")})
	}
	one := consumersValue(t, r, "10.0.0.31")
	active := grantsValue(t, r, "active", "10.0.0.31")

	for _, tc := range []struct {
		name    string
		prior   map[string]tftypes.Value
		planned map[string]tftypes.Value
		keeps   bool
	}{
		{"a rename keeps an active address grant",
			store("a", 100, one, active), store("b", 100, one, unknownStatus), true},
		{"a new consumer leaves it unknown",
			store("a", 100, one, active), store("a", 100, consumersValue(t, r, "10.0.0.31", "10.0.0.32"), unknownStatus), false},
		{"an expansion leaves it unknown",
			store("a", 100, one, active), store("a", 200, one, unknownStatus), false},
		{"a grant still being made leaves it unknown",
			store("a", 100, one, grantsValue(t, r, "creating", "10.0.0.31")), store("b", 100, one, unknownStatus), false},
		{"a revoke still running leaves it unknown",
			store("a", 100, one, grantsValue(t, r, "active", "10.0.0.31", "10.0.0.40")), store("b", 100, one, unknownStatus), false},
		{"a group grant leaves it unknown: membership moves it",
			store("a", 100, groupConsumerValue(t, r, "trainers"), grantsValue(t, r, "active", "trainers")),
			store("b", 100, groupConsumerValue(t, r, "trainers"), unknownStatus), false},
		{"a consumer with no grant reported yet leaves it unknown",
			store("a", 100, one, grantsValue(t, r, "active")), store("b", 100, one, unknownStatus), false},
		{"a store still being made leaves it unknown",
			creating(store("a", 100, one, active)), store("b", 100, one, unknownStatus), false},
		{"a store with no consumers keeps none",
			store("a", 100, consumersValue(t, r), grantsValue(t, r, "active")),
			store("b", 100, consumersValue(t, r), unknownStatus), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := keepMap(t, keptGrantStatus{settledBy: fileStoreStates.settle}, pairOf(t, r, tc.prior, tc.planned), "consumer_status")
			if tc.keeps == got.IsUnknown() {
				t.Errorf("consumer_status = %s, keeps = %v", got, tc.keeps)
			}
		})
	}
}

// networkValues is a network in prior state, or planned with the given state.
func networkValues(name string, state tftypes.Value) map[string]tftypes.Value {
	return map[string]tftypes.Value{
		"id": str("n1"), "region": str("r"), "project": str("p"), "display_name": str(name),
		"type": str("evpn"), "state": state,
	}
}

// A settled state is what an update ends in, and anything else is unknown.
func TestPlanSettledState(t *testing.T) {
	r := &networkResource{}
	for _, tc := range []struct {
		name    string
		prior   tftypes.Value
		renamed bool
		want    *string
	}{
		{"active stays active", str("active"), true, ptr("active")},
		{"updating is unknown", str("updating"), true, nil},
		{"failed is unknown", str("failed"), true, nil},
		{"no state reported is unknown", tftypes.NewValue(tftypes.String, nil), true, nil},
		{"a state nobody declared is unknown", str("hibernating"), true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := pairOf(t, r, networkValues("before", tc.prior), networkValues("after", unknownString))
			resp := resource.ModifyPlanResponse{Plan: p.plan}
			planSettledState(context.Background(), networkStates.settle,
				resource.ModifyPlanRequest{Plan: p.plan, State: p.state}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatal(resp.Diagnostics)
			}
			got := get[types.String](t, resp.Plan, "state")
			switch {
			case tc.want == nil && !got.IsUnknown():
				t.Errorf("state = %s, want unknown", got)
			case tc.want != nil && got.ValueString() != *tc.want:
				t.Errorf("state = %s, want %q", got, *tc.want)
			}
		})
	}

	t.Run("a plan with no change is left alone", func(t *testing.T) {
		p := pairOf(t, r, networkValues("same", str("updating")), networkValues("same", str("updating")))
		resp := resource.ModifyPlanResponse{Plan: p.plan}
		planSettledState(context.Background(), networkStates.settle,
			resource.ModifyPlanRequest{Plan: p.plan, State: p.state}, &resp)
		if got := get[types.String](t, resp.Plan, "state"); got.ValueString() != "updating" {
			t.Errorf("state = %s, want it as it was", got)
		}
	})

	t.Run("a create is left alone", func(t *testing.T) {
		p := pairOf(t, r, nil, networkValues("new", unknownString))
		resp := resource.ModifyPlanResponse{Plan: p.plan}
		planSettledState(context.Background(), networkStates.settle,
			resource.ModifyPlanRequest{Plan: p.plan, State: p.state}, &resp)
		if got := get[types.String](t, resp.Plan, "state"); !got.IsUnknown() {
			t.Errorf("state = %s, want unknown", got)
		}
	})
}

// Every kind that plans a settled state settles on `active`, so the value a
// plan promises is one its wait can end on.
func TestEverySettleTableSettlesActive(t *testing.T) {
	for name, table := range map[string]stateTable{
		"network": networkStates.settle, "subnet": subnetStates.settle, "security group": securityGroupStates.settle,
		"peering": vpcPeeringStates.settle, "vpc": vpcStates.settle, "instance group": instanceGroupStates.settle,
		"file store": fileStoreStates.settle, "cluster": clusterStates.settle,
	} {
		if !table.settles("active") {
			t.Errorf("%s: active doesn't settle", name)
		}
		for _, busy := range []string{"creating", "updating", "deleting", "failed"} {
			if table.settles(busy) {
				t.Errorf("%s: %s settles", name, busy)
			}
		}
	}
}

// settledRead waits out a resource that went busy after the wait looked, and
// only when the plan promised a settled state.
func TestSettledRead(t *testing.T) {
	type read struct {
		state string
		found bool
	}
	run := func(planned types.String, reads []read, waitErr error) (string, int, error) {
		at, waits := 0, 0
		got, _, _, err := settledRead(context.Background(), planned, networkStates.settle,
			func(context.Context) (read, bool, diag.Diagnostics) {
				r := reads[min(at, len(reads)-1)]
				at++
				return r, r.found, nil
			},
			func(r read) types.String {
				if r.state == "" {
					return types.StringNull()
				}
				return types.StringValue(r.state)
			},
			func(context.Context) error { waits++; return waitErr })
		return got.state, waits, err
	}

	active := types.StringValue("active")
	if got, waits, err := run(active, []read{{"active", true}}, nil); got != "active" || waits != 0 || err != nil {
		t.Errorf("settled at once: %q after %d waits, %v", got, waits, err)
	}
	if got, waits, err := run(active, []read{{"updating", true}, {"active", true}}, nil); got != "active" || waits != 1 || err != nil {
		t.Errorf("busy, then settled: %q after %d waits, %v", got, waits, err)
	}
	if got, waits, err := run(types.StringUnknown(), []read{{"updating", true}, {"active", true}}, nil); got != "updating" || waits != 0 || err != nil {
		t.Errorf("a plan that promised nothing waited: %q after %d waits, %v", got, waits, err)
	}
	failed := errors.New("the platform reports the network as failed")
	if _, _, err := run(active, []read{{"failed", true}}, failed); !errors.Is(err, failed) {
		t.Errorf("a failed network: %v, want the wait's error", err)
	}
	if got, waits, err := run(active, []read{{"", true}}, nil); got != "" || waits != 0 || err != nil {
		t.Errorf("no state reported: %q after %d waits, want the read as it is, %v", got, waits, err)
	}
	if _, waits, err := run(active, []read{{"updating", false}}, nil); waits != 0 || err != nil {
		t.Errorf("a resource that's gone waited %d times, %v", waits, err)
	}
}

// computedDecisions says, for every Computed attribute that has no plan
// modifier and no default, where its plan is decided instead. A new one
// fails TestEveryComputedAttributeIsPlanned until it's decided and listed,
// so none is left to show "known after apply" on every update by default.
var computedDecisions = map[string]string{
	"region":   "modifyScopePlan in ModifyPlan, or planRegion on a project",
	"project":  "modifyScopePlan in ModifyPlan",
	"tags_all": "planTagsAll, or planTagsAllAtCreation, in ModifyPlan",
	"state":    "planSettledState in ModifyPlan",
}

// The kinds whose `state` is planned in ModifyPlan, as computedDecisions says.
var settledStateKinds = []string{
	"network", "subnet", "security_group", "vpc_peering", "vpc_security_groups", "instance_group",
	"file_store", "kubernetes_cluster",
}

func TestEveryComputedAttributeIsPlanned(t *testing.T) {
	ctx := context.Background()
	for _, s := range docSurfaces(ctx) {
		if s.kind != "resource" {
			continue
		}
		kind := strings.TrimPrefix(s.name, ResourcePrefix())
		var walk func(m reflect.Value, prefix string)
		walk = func(m reflect.Value, prefix string) {
			if !m.IsValid() || m.Kind() != reflect.Map {
				return
			}
			for _, k := range m.MapKeys() {
				v := concrete(m.MapIndex(k))
				if !v.IsValid() {
					continue
				}
				name := prefix + k.String()
				computed := boolField(v, "Computed")
				if computed && len(listField(v, "PlanModifiers")) == 0 && !hasDefault(v) {
					why, ok := computedDecisions[name]
					switch {
					case !ok:
						t.Errorf("%s.%s: Computed, with no plan modifier and no entry in computedDecisions", s.name, name)
					case name == "state" && !slices.Contains(settledStateKinds, kind):
						t.Errorf("%s.state: %s, and the kind isn't in settledStateKinds", s.name, why)
					}
				}
				// What a computed parent plans covers everything inside it.
				if computed {
					continue
				}
				for _, nested := range nestedMaps(v) {
					walk(nested, name+".")
				}
			}
		}
		walk(s.attributes, "")
		walk(s.blocks, "")
	}
}

func hasDefault(v reflect.Value) bool {
	f := v.FieldByName("Default")
	return f.IsValid() && f.Kind() == reflect.Interface && !f.IsNil()
}
