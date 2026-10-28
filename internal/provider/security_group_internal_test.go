package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// A group with no rules is refused only when both lists are known. A `dynamic`
// block over a variable is unknown under `terraform validate`, and it may yet
// hold a rule, so the check waits for it.
func TestSecurityGroupNeedsARuleOnceBothListsAreKnown(t *testing.T) {
	r := &securityGroupResource{}
	_, s := testValue(t, r, nil)
	object, ok := s.Schema.Type().TerraformType(context.Background()).(tftypes.Object)
	if !ok {
		t.Fatal("schema is not an object")
	}
	list := func(direction string, value any) tftypes.Value {
		return tftypes.NewValue(object.AttributeTypes[direction], value)
	}
	for name, tt := range map[string]struct {
		ingress, egress tftypes.Value
		refused         bool
	}{
		"both null":                     {list("ingress", nil), list("egress", nil), true},
		"both empty":                    {list("ingress", []tftypes.Value{}), list("egress", []tftypes.Value{}), true},
		"ingress unknown, egress empty": {list("ingress", tftypes.UnknownValue), list("egress", []tftypes.Value{}), false},
		"egress unknown, ingress null":  {list("ingress", nil), list("egress", tftypes.UnknownValue), false},
		"both unknown":                  {list("ingress", tftypes.UnknownValue), list("egress", tftypes.UnknownValue), false},
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := testValue(t, r, map[string]tftypes.Value{"ingress": tt.ingress, "egress": tt.egress})
			var resp resource.ValidateConfigResponse
			r.ValidateConfig(context.Background(), resource.ValidateConfigRequest{
				Config: tfsdk.Config{Raw: raw, Schema: s.Schema},
			}, &resp)
			refused := false
			for _, d := range resp.Diagnostics.Errors() {
				if d.Summary() == "A security group needs at least one rule" {
					refused = true
				} else {
					t.Errorf("unexpected error: %s: %s", d.Summary(), d.Detail())
				}
			}
			if refused != tt.refused {
				t.Errorf("refused = %v, want %v", refused, tt.refused)
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
