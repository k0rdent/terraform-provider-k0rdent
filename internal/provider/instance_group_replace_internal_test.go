package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

func poolMap(t *testing.T, sizes map[string]attr.Value) types.Map {
	t.Helper()
	elems := map[string]attr.Value{}
	for id, size := range sizes {
		if size == nil {
			elems[id] = types.ObjectUnknown(instanceGroupPoolObjectType().AttrTypes)
			continue
		}
		o, d := types.ObjectValue(instanceGroupPoolObjectType().AttrTypes, map[string]attr.Value{"size": size})
		if d.HasError() {
			t.Fatal(d)
		}
		elems[id] = o
	}
	m, d := types.MapValue(instanceGroupPoolObjectType(), elems)
	if d.HasError() {
		t.Fatal(d)
	}
	return m
}

// A size known only at apply, such as one read from another resource, may be
// a grow. Replacing on it releases every host in the group, so the decision
// waits for the value, and Terraform makes it again when the value is known.
func TestInstanceGroupPoolsReplace(t *testing.T) {
	n, u := types.Int64Value, types.Int64Unknown()
	have := map[string]attr.Value{"gpu": n(1)}
	tests := []struct {
		name   string
		config map[string]attr.Value
		want   bool
	}{
		{"a grow scales", map[string]attr.Value{"gpu": n(3)}, false},
		{"a shrink replaces", map[string]attr.Value{"gpu": n(0)}, true},
		{"a size known only at apply waits", map[string]attr.Value{"gpu": u}, false},
		{"a pool known only at apply waits", map[string]attr.Value{"gpu": nil}, false},
		{"a new pool replaces, whatever its size", map[string]attr.Value{"gpu": n(1), "cpu": u}, true},
		{"a renamed pool replaces", map[string]attr.Value{"cpu": u}, true},
	}
	raw := tftypes.NewValue(tftypes.String, "set")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := poolMap(t, tt.config)
			var resp mapplanmodifier.RequiresReplaceIfFuncResponse
			instanceGroupPoolsRequireReplace(context.Background(), planmodifier.MapRequest{
				State: tfsdk.State{Raw: raw}, Plan: tfsdk.Plan{Raw: raw},
				StateValue: poolMap(t, have), ConfigValue: config, PlanValue: config,
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
