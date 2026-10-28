package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

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

// The platform adds no rules to a tenant group, and on the nico fabric
// traffic that no rule permits is denied both ways. The description once said
// the platform appended a permit-all ingress rule, which told people to end
// every group with a deny.
func TestTheDescriptionSaysUnmatchedTrafficIsDenied(t *testing.T) {
	var s resource.SchemaResponse
	(&securityGroupResource{}).Schema(context.Background(), resource.SchemaRequest{}, &s)
	desc := s.Schema.MarkdownDescription
	for _, want := range []string{"adds no rules to a group", "no rule permits is denied, in both directions"} {
		if !strings.Contains(desc, want) {
			t.Errorf("the security group description doesn't say %q: %s", want, desc)
		}
	}
	for _, old := range []string{"permit all ingress", "filters nothing"} {
		if strings.Contains(desc, old) {
			t.Errorf("the security group description still says %q: %s", old, desc)
		}
	}
}
