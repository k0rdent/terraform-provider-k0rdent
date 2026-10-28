package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// oneEgressRule is a security group's `egress` holding one permit-all rule,
// for a test about something other than its rules. oneEgressRuleBody is the
// same rule as the platform answers it.
func oneEgressRule(t *testing.T) tftypes.Value {
	t.Helper()
	var s resource.SchemaResponse
	(&securityGroupResource{}).Schema(context.Background(), resource.SchemaRequest{}, &s)
	object, ok := s.Schema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema is not an object")
	}
	listType, ok := object.AttributeTypes["egress"].(tftypes.List)
	if !ok {
		t.Fatal("egress is not a list")
	}
	ruleType, ok := listType.ElementType.(tftypes.Object)
	if !ok {
		t.Fatal("a rule is not an object")
	}
	rule := map[string]tftypes.Value{}
	for name, typ := range ruleType.AttributeTypes {
		rule[name] = tftypes.NewValue(typ, nil)
	}
	for name, v := range map[string]string{
		"action": "permit", "protocol": "any", "source_prefix": "0.0.0.0/0", "destination_prefix": "0.0.0.0/0",
	} {
		rule[name] = tftypes.NewValue(tftypes.String, v)
	}
	return tftypes.NewValue(listType, []tftypes.Value{tftypes.NewValue(ruleType, rule)})
}

var oneEgressRuleBody = map[string]any{"ingress": []any{}, "egress": []any{map[string]any{
	"action": "permit", "protocol": "any", "sourcePrefix": "0.0.0.0/0", "destinationPrefix": "0.0.0.0/0",
}}}

// A create or a change that leaves a group with no rules is refused. A group
// in state with none that plans no change gets a warning, and a destroy gets
// nothing, so it can be deleted. A list a `dynamic` block builds from a value
// not known yet may still hold a rule, so the check waits for it.
func TestSecurityGroupNeedsARuleOnCreateOrChange(t *testing.T) {
	r := &securityGroupResource{}
	_, s := testValue(t, r, nil)
	object, ok := s.Schema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema is not an object")
	}
	var oneRule []tftypes.Value
	if err := oneEgressRule(t).As(&oneRule); err != nil {
		t.Fatal(err)
	}
	empty, unknown := []tftypes.Value{}, any(tftypes.UnknownValue)

	value := func(ingress, egress any, set map[string]tftypes.Value) tftypes.Value {
		all := map[string]tftypes.Value{
			"ingress": tftypes.NewValue(object.AttributeTypes["ingress"], ingress),
			"egress":  tftypes.NewValue(object.AttributeTypes["egress"], egress),
			"id":      tftypes.NewValue(tftypes.String, "g"),
		}
		for k, v := range set {
			all[k] = v
		}
		raw, _ := testValue(t, r, all)
		return raw
	}
	null := tftypes.NewValue(object, nil)
	renamed := map[string]tftypes.Value{"display_name": str("renamed")}
	ownedByVPC := map[string]tftypes.Value{"owner_kind": str("vpc")}

	const refused, warned, silent = "refused", "warned", "silent"
	for name, tt := range map[string]struct {
		config, state, plan tftypes.Value
		want                string
	}{
		"a create with no blocks":         {value(nil, nil, nil), null, value(nil, nil, nil), refused},
		"a create with empty lists":       {value(empty, empty, nil), null, value(empty, empty, nil), refused},
		"a create with one egress rule":   {value(empty, oneRule, nil), null, value(empty, oneRule, nil), silent},
		"a create with ingress unknown":   {value(unknown, empty, nil), null, value(unknown, empty, nil), silent},
		"a create with egress unknown":    {value(nil, unknown, nil), null, value(nil, unknown, nil), silent},
		"a change to a group with none":   {value(empty, empty, renamed), value(empty, empty, nil), value(empty, empty, renamed), refused},
		"a change that empties the rules": {value(empty, empty, nil), value(empty, oneRule, nil), value(empty, empty, nil), refused},
		"a change to a group with a rule": {value(empty, oneRule, renamed), value(empty, oneRule, nil), value(empty, oneRule, renamed), silent},
		"no change to a group with none":  {value(empty, empty, nil), value(empty, empty, nil), value(empty, empty, nil), warned},
		"a destroy of a group with none":  {value(empty, empty, nil), value(empty, empty, nil), null, silent},
		"no change to a VPC's default":    {value(empty, empty, ownedByVPC), value(empty, empty, ownedByVPC), value(empty, empty, ownedByVPC), silent},
	} {
		t.Run(name, func(t *testing.T) {
			req := resource.ModifyPlanRequest{
				Config: tfsdk.Config{Raw: tt.config, Schema: s.Schema},
				State:  tfsdk.State{Raw: tt.state, Schema: s.Schema},
				Plan:   tfsdk.Plan{Raw: tt.plan, Schema: s.Schema},
			}
			resp := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Raw: tt.plan, Schema: s.Schema}}
			planNeedsARule(context.Background(), req, &resp)

			got := silent
			for _, d := range resp.Diagnostics {
				switch d.Summary() {
				case "A security group needs at least one rule":
					got = refused
				case "This security group has no rules":
					got = warned
				default:
					t.Errorf("unexpected diagnostic: %s: %s", d.Summary(), d.Detail())
				}
			}
			if len(resp.Diagnostics) > 1 {
				t.Errorf("%d diagnostics, want at most one: %v", len(resp.Diagnostics), resp.Diagnostics)
			}
			if got != tt.want {
				t.Errorf("%s, want %s", got, tt.want)
			}
		})
	}
}

func TestKeepPortSpelling(t *testing.T) {
	s := types.StringValue
	tests := []struct {
		prior, got types.String
		want       string
	}{
		{s("80-80"), s("80"), "80-80"},
		{s("80"), s("80"), "80"},
		{s("80-81"), s("80-81"), "80-81"},
		{s("80-80"), s("443"), "443"},
		{types.StringNull(), s("80"), "80"},
	}
	for _, tt := range tests {
		if got := keepPortSpelling(tt.prior, tt.got); got.ValueString() != tt.want {
			t.Errorf("keepPortSpelling(%s, %s) = %s, want %s", tt.prior, tt.got, got, tt.want)
		}
	}
}
