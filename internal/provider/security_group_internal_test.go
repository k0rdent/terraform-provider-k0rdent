package provider

import (
	"testing"

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
