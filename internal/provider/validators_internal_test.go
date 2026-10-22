package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func runStringValidator(v validator.String, value types.String) []string {
	req := validator.StringRequest{Path: path.Root("attr"), ConfigValue: value}
	resp := &validator.StringResponse{}
	v.ValidateString(context.Background(), req, resp)

	var out []string
	for _, d := range resp.Diagnostics.Errors() {
		out = append(out, d.Summary())
	}
	return out
}

// The named case is the point of each row: a CIDR whose host bits are set
// parses fine and filters something other than what it reads as, which is the
// one `net.ParseCIDR` would have silently accepted.
func TestCIDRHostBitsZero(t *testing.T) {
	tests := []struct {
		name  string
		value types.String
		want  string
	}{
		{"an IPv4 prefix", types.StringValue("10.0.0.0/8"), ""},
		{"an IPv6 prefix", types.StringValue("2001:db8::/32"), ""},
		{"the IPv4 default route", types.StringValue("0.0.0.0/0"), ""},
		{"the IPv6 default route", types.StringValue("::/0"), ""},
		{"a single host", types.StringValue("10.0.0.5/32"), ""},
		{"host bits set", types.StringValue("10.0.0.5/24"), "CIDR prefix has host bits set"},
		{"IPv6 host bits set", types.StringValue("2001:db8::1/32"), "CIDR prefix has host bits set"},
		{"a bare address", types.StringValue("10.0.0.0"), "Not a CIDR prefix"},
		{"a prefix length past the family", types.StringValue("10.0.0.0/33"), "Not a CIDR prefix"},
		{"nothing at all", types.StringValue("everywhere"), "Not a CIDR prefix"},
		{"null", types.StringNull(), ""},
		{"unknown", types.StringUnknown(), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runStringValidator(cidrHostBitsZero{}, tt.value)
			switch {
			case tt.want == "" && len(got) > 0:
				t.Fatalf("rejected a valid prefix: %v", got)
			case tt.want != "" && len(got) == 0:
				t.Fatalf("accepted %q, wanted %q", tt.value, tt.want)
			case tt.want != "" && got[0] != tt.want:
				t.Fatalf("got %q, wanted %q", got[0], tt.want)
			}
		})
	}
}

func TestPortRange(t *testing.T) {
	tests := []struct {
		name  string
		value types.String
		want  string
	}{
		{"a single port", types.StringValue("22"), ""},
		{"a range", types.StringValue("80-81"), ""},
		{"the whole space", types.StringValue("1-65535"), ""},
		{"a range of one", types.StringValue("443-443"), ""},
		{"backwards", types.StringValue("443-80"), "Port range runs backwards"},
		{"port zero, which the contract allows", types.StringValue("0"), ""},
		{"the whole space from zero", types.StringValue("0-65535"), ""},
		{"past the space", types.StringValue("65536"), "Not a port range"},
		{"a service name", types.StringValue("ssh"), "Not a port range"},
		{"an open range", types.StringValue("80-"), "Not a port range"},
		{"null", types.StringNull(), ""},
		{"unknown", types.StringUnknown(), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runStringValidator(portRange{}, tt.value)
			switch {
			case tt.want == "" && len(got) > 0:
				t.Fatalf("rejected a valid port range: %v", got)
			case tt.want != "" && len(got) == 0:
				t.Fatalf("accepted %q, wanted %q", tt.value, tt.want)
			case tt.want != "" && got[0] != tt.want:
				t.Fatalf("got %q, wanted %q", got[0], tt.want)
			}
		})
	}
}

// urlSafeIdentifier is the schema-level half of the rule validatePathSegment
// enforces for region and project. The project surface is addressed by
// `organization_id` and `id` instead, so nothing was checking either until this
// existed, and `organization_id = ".."` reached the request builder.
//
// The accepted rows matter as much as the refused ones: a hyphenated slug and a
// digit-bearing one are what real organization and project ids look like, and a
// rule that refused them would be found at the worst possible moment.
func TestURLSafeIdentifier(t *testing.T) {
	const refused = "Not a valid identifier"

	tests := []struct {
		name  string
		value types.String
		want  string
	}{
		{"an organization id", types.StringValue("org-demo"), ""},
		{"a project slug", types.StringValue("cp-production"), ""},
		{"one letter", types.StringValue("a"), ""},
		{"digits after the first letter", types.StringValue("gpu-training-2"), ""},
		{"the parent directory", types.StringValue(".."), refused},
		{"the current directory", types.StringValue("."), refused},
		{"a traversal with a tail", types.StringValue("../other"), refused},
		{"an injected segment", types.StringValue("a/b"), refused},
		{"a query string", types.StringValue("a?b"), refused},
		{"a fragment", types.StringValue("a#b"), refused},
		{"percent-encoded dots", types.StringValue("a%2e%2e"), refused},
		{"upper case", types.StringValue("A"), refused},
		{"leading digit", types.StringValue("1a"), refused},
		{"an underscore", types.StringValue("a_b"), refused},
		{"a trailing hyphen", types.StringValue("a-"), refused},
		{"a space", types.StringValue("a b"), refused},
		// An empty string is a value, not an absence: it plans clean and then
		// fails at apply. Null is the schema's job and an unknown is not
		// knowable at plan time, so neither is this validator's to refuse.
		{"empty", types.StringValue(""), refused},
		{"null", types.StringNull(), ""},
		{"unknown", types.StringUnknown(), ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := runStringValidator(urlSafeIdentifier{}, tt.value)
			switch {
			case tt.want == "" && len(got) > 0:
				t.Fatalf("rejected a valid identifier: %v", got)
			case tt.want != "" && len(got) == 0:
				t.Fatalf("accepted %q, wanted %q", tt.value, tt.want)
			case tt.want != "" && got[0] != tt.want:
				t.Fatalf("got %q, wanted %q", got[0], tt.want)
			}
		})
	}
}
