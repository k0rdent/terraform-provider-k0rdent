package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/objectplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var evpnTypes = map[string]attr.Type{
	"service_type": types.StringType, "mtu": types.Int64Type, "dhcp_enabled": types.BoolType,
	"dns_servers": types.ListType{ElemType: types.StringType},
	"vlan_id":     types.StringType, "vlan_id_resolved": types.StringType,
}

func evpnObject(t *testing.T, vlan, resolved, mtu attr.Value) types.Object {
	t.Helper()
	o, d := types.ObjectValue(evpnTypes, map[string]attr.Value{
		"service_type": types.StringValue("l3vpn"), "mtu": mtu, "dhcp_enabled": types.BoolValue(false),
		"dns_servers": types.ListNull(types.StringType), "vlan_id": vlan, "vlan_id_resolved": resolved,
	})
	if d.HasError() {
		t.Fatal(d)
	}
	return o
}

func TestEVPNReplace(t *testing.T) {
	s, u := types.StringValue, types.StringUnknown()
	mtu := types.Int64Value(9000)
	tests := []struct {
		name          string
		state, config types.Object
		plan          types.Object
		want          bool
	}{
		{
			name:   "auto is satisfied by the id an import recorded",
			state:  evpnObject(t, s("4001"), s("4001"), mtu),
			config: evpnObject(t, s("auto"), types.StringNull(), mtu),
			plan:   evpnObject(t, s("auto"), u, mtu),
		},
		{
			name:   "a configured id is satisfied by the same id reached through auto",
			state:  evpnObject(t, s("auto"), s("100"), mtu),
			config: evpnObject(t, s("100"), types.StringNull(), mtu),
			plan:   evpnObject(t, s("100"), u, mtu),
		},
		{
			name:   "a different id replaces",
			state:  evpnObject(t, s("auto"), s("100"), mtu),
			config: evpnObject(t, s("200"), types.StringNull(), mtu),
			plan:   evpnObject(t, s("200"), u, mtu),
			want:   true,
		},
		{
			name:   "an unknown the configuration set is a change",
			state:  evpnObject(t, s("100"), s("100"), mtu),
			config: evpnObject(t, s("100"), types.StringNull(), types.Int64Unknown()),
			plan:   evpnObject(t, s("100"), u, types.Int64Unknown()),
			want:   true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var resp objectplanmodifier.RequiresReplaceIfFuncResponse
			evpnRequiresReplace(context.Background(), planmodifier.ObjectRequest{
				StateValue: tt.state, ConfigValue: tt.config, PlanValue: tt.plan,
			}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatal(resp.Diagnostics)
			}
			if resp.RequiresReplace != tt.want {
				t.Errorf("RequiresReplace = %v, want %v", resp.RequiresReplace, tt.want)
			}
		})
	}
}
