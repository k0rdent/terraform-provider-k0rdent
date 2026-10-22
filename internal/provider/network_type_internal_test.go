package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// A create plans `type` from the fabric block it sets, so it isn't "known
// after apply".
func TestNetworkTypeFollowsTheFabric(t *testing.T) {
	r := newNetworkResource()
	_, s := testValue(t, r, nil)
	object, ok := s.Schema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema is not an object")
	}
	set := func(block string) tftypes.Value {
		ty, ok := object.AttributeTypes[block].(tftypes.Object)
		if !ok {
			t.Fatalf("%s is not an object", block)
		}
		empty := map[string]tftypes.Value{}
		for name, at := range ty.AttributeTypes {
			empty[name] = tftypes.NewValue(at, nil)
		}
		return tftypes.NewValue(ty, empty)
	}
	unknown := func(block string) tftypes.Value {
		return tftypes.NewValue(object.AttributeTypes[block], tftypes.UnknownValue)
	}

	tests := []struct {
		name   string
		blocks map[string]tftypes.Value
		want   types.String
	}{
		{"evpn", map[string]tftypes.Value{"evpn": set("evpn")}, types.StringValue("evpn")},
		{"infiniband", map[string]tftypes.Value{"infiniband": set("infiniband")}, types.StringValue("infiniband")},
		{"nvlink", map[string]tftypes.Value{"nvlink": set("nvlink")}, types.StringValue("nvlink")},
		{"a block not known yet", map[string]tftypes.Value{"evpn": unknown("evpn")}, types.StringUnknown()},
		{"another block not known yet", map[string]tftypes.Value{"evpn": set("evpn"), "nvlink": unknown("nvlink")}, types.StringUnknown()},
		{"two blocks, which validation refuses", map[string]tftypes.Value{"evpn": set("evpn"), "nvlink": set("nvlink")}, types.StringUnknown()},
		{"no block", nil, types.StringUnknown()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, _ := testValue(t, r, tt.blocks)
			req := planmodifier.StringRequest{
				Plan:      tfsdk.Plan{Raw: raw, Schema: s.Schema},
				PlanValue: types.StringUnknown(),
			}
			resp := planmodifier.StringResponse{PlanValue: req.PlanValue}
			networkTypeFromFabric{}.PlanModifyString(context.Background(), req, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatal(resp.Diagnostics)
			}
			if !resp.PlanValue.Equal(tt.want) {
				t.Errorf("type planned %s, want %s", resp.PlanValue, tt.want)
			}
		})
	}

	// A destroy plans nothing.
	destroy := planmodifier.StringRequest{
		Plan:      tfsdk.Plan{Raw: tftypes.NewValue(object, nil), Schema: s.Schema},
		PlanValue: types.StringNull(),
	}
	resp := planmodifier.StringResponse{PlanValue: destroy.PlanValue}
	networkTypeFromFabric{}.PlanModifyString(context.Background(), destroy, &resp)
	if !resp.PlanValue.IsNull() || resp.Diagnostics.HasError() {
		t.Errorf("a destroy planned %s, %v", resp.PlanValue, resp.Diagnostics)
	}
}
