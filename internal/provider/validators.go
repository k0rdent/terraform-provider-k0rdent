package provider

import (
	"context"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// The two format-constrained strings on the networking surface that
// terraform-plugin-framework-validators has no ready answer for.
//
// Both exist for the same reason as the enum validators beside them: the server
// answers 422, and a 422 arrives halfway through an apply the user has already
// approved. A plan-time message costs nothing and lands before the approval.

// cidrHostBitsZero accepts a prefix whose host bits are all zero.
//
// `net/netip` rather than `net.ParseCIDR`, because ParseCIDR masks the address
// for you and so accepts `10.0.0.5/24` silently. The platform does not: the
// contract says host bits must be zero, and a rule written with the wrong one
// filters something other than what it reads as.
type cidrHostBitsZero struct{}

func (cidrHostBitsZero) Description(context.Context) string {
	return "must be a CIDR prefix with all host bits zero, such as `10.0.0.0/24`"
}

func (v cidrHostBitsZero) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (cidrHostBitsZero) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	raw := req.ConfigValue.ValueString()

	prefix, err := netip.ParsePrefix(raw)
	if err != nil {
		// Local: configuration validation, no request made.
		resp.Diagnostics.AddAttributeError(req.Path, "Not a CIDR prefix",
			fmt.Sprintf("%q is not an address and prefix length, such as `10.0.0.0/24` or `::/0`.", raw))
		return
	}
	if prefix.Masked() != prefix {
		// Local: as above.
		resp.Diagnostics.AddAttributeError(req.Path, "CIDR prefix has host bits set",
			fmt.Sprintf("%q sets bits below its prefix length. The platform rejects it. Write %q.",
				raw, prefix.Masked()))
	}
}

// portRange accepts a single port or an inclusive `low-high` range.
//
// A string rather than a number because the contract models it as one, and a
// range has to be expressible. Ordering is checked here too: `443-80` parses as
// two valid ports and matches nothing.
type portRange struct{}

func (portRange) Description(context.Context) string {
	return "must be a port (`\"80\"`) or an inclusive range (`\"80-81\"`)"
}

func (v portRange) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (portRange) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	raw := req.ConfigValue.ValueString()

	low, high, isRange := strings.Cut(raw, "-")
	lowPort, err := parsePort(low)
	if err != nil {
		// Local: configuration validation, no request made.
		resp.Diagnostics.AddAttributeError(req.Path, "Not a port range", err.Error())
		return
	}
	if !isRange {
		return
	}
	highPort, err := parsePort(high)
	if err != nil {
		// Local: as above.
		resp.Diagnostics.AddAttributeError(req.Path, "Not a port range", err.Error())
		return
	}
	if highPort < lowPort {
		// Local: as above.
		resp.Diagnostics.AddAttributeError(req.Path, "Port range runs backwards",
			fmt.Sprintf("%q ends below where it starts, so it matches no port. Write %q.",
				raw, fmt.Sprintf("%d-%d", highPort, lowPort)))
	}
}

// parsePort takes digits only. strconv.Atoi alone takes a sign, so `+80`
// passed here and was refused by the platform, whose pattern is
// `^[0-9]{1,5}(-[0-9]{1,5})?$`.
var portDigits = regexp.MustCompile(`^[0-9]{1,5}$`)

func parsePort(s string) (int, error) {
	if !portDigits.MatchString(s) {
		return 0, fmt.Errorf("%q is not a port number", s)
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a port number", s)
	}
	if n < 0 || n > 65535 {
		return 0, fmt.Errorf("port %d is outside 0-65535", n)
	}
	return n, nil
}

// refusePlatformOwned stops a plan that would write to an object the platform
// owns rather than the tenant.
//
// Three resources expose a Computed attribute that says so: `cluster_id` on a
// network or subnet the cluster lifecycle created, and `owner_kind` on a
// VPC's DEFAULT security group. The tenant API answers 409 to any write to one,
// so without this an import plans cleanly and fails partway through the apply.
//
// A destroy is deliberately not refused here; see forgetPlatformOwned.
//
// The check is on prior state, so it only ever fires on something imported or
// previously read. A create cannot reach it.
func refusePlatformOwned(ctx context.Context, attribute, owned string, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	if req.Plan.Raw.Equal(req.State.Raw) {
		return // a refresh with no change: reading one is allowed, writing is not
	}

	value, ok := platformOwner(ctx, attribute, owned, req.State)
	if !ok {
		return
	}
	// Local: read from prior state, no request made. The upstream 409 this
	// replaces would have carried a code and a request id; the point is that it
	// is never sent.
	resp.Diagnostics.AddError(
		"This object is owned by the platform, not by the tenant",
		fmt.Sprintf("`%s` is %q, which means the platform provisioned this object and the tenant "+
			"API rejects any write to it. Changing it would fail partway through the apply. "+
			"Remove it from the configuration and from state with `terraform state rm`.",
			attribute, value),
	)
}

// forgetPlatformOwned reports whether Delete should drop the resource from
// state without calling the API.
//
// Refusing the destroy as well would be consistent and would also be a trap: an
// object adopted by mistake could then never leave the state file through
// Terraform, and one of them would block `terraform destroy` for the whole
// configuration at plan time, with no partial progress. The delete would 409
// anyway, so there is nothing to call.
//
// This is what AWS does with `aws_default_vpc` and its siblings, for the same
// reason: the resource adopts something it did not create, and destroying it
// stops tracking it rather than deleting it. The warning is not optional: a
// practitioner reading "1 destroyed" has to be told the object is still there.
func forgetPlatformOwned(ctx context.Context, attribute, owned string, state tfsdk.State, diags *diag.Diagnostics) bool {
	value, ok := platformOwner(ctx, attribute, owned, state)
	if !ok {
		return false
	}
	// Local: as above, and nothing is deleted.
	diags.AddWarning(
		"Stopped tracking a platform-owned object rather than deleting it",
		fmt.Sprintf("`%s` is %q, so the platform provisioned this object and the tenant API "+
			"rejects any delete. Terraform has removed it from state and the object still "+
			"exists. It goes away when whatever owns it does.", attribute, value),
	)
	return true
}

// platformOwner reads the ownership attribute out of prior state, and reports
// whether it names the platform.
func platformOwner(ctx context.Context, attribute, owned string, state tfsdk.State) (string, bool) {
	if state.Raw.IsNull() {
		return "", false
	}
	var value types.String
	if d := state.GetAttribute(ctx, path.Root(attribute), &value); d.HasError() {
		return "", false
	}
	if value.IsNull() || value.ValueString() == "" {
		return "", false
	}
	// An empty `owned` means any value counts: `cluster_id` is either set or
	// absent. `owner_kind` carries a named kind and only `vpc` is declared.
	if owned != "" && value.ValueString() != owned {
		return "", false
	}
	return value.ValueString(), true
}

// urlSafeIdentifier refuses a value that would edit the request URL rather than
// fill a slot in it.
//
// `validatePathSegment` in config.go already does this for the provider block's
// `region` and `project`, and for the per-resource overrides of both, at read
// time. The project surface needs the same rule and cannot reuse that path: a
// project is addressed by `organization_id` and `id` on the global tier and
// carries neither of the two scope attributes, so nothing was checking either
// of them and `organization_id = ".."` sent the request to a different route.
//
// It is a schema validator rather than a check inside Create, because a
// resource's Create runs at apply time and the whole point of the rule beside
// it is that a 422 arrives after the user has already approved the plan. A
// data source is read during the plan either way, so this costs it nothing.
//
// See validatePathSegment for the measurement behind the pattern. The short
// version: the generated client escapes `/` and not `.`, and the request
// builder ends in RFC 3986 reference resolution, which removes dot segments.
type urlSafeIdentifier struct{}

func (urlSafeIdentifier) Description(context.Context) string {
	return "must be lower-case letters, digits and hyphens, starting with a letter and not " +
		"ending with a hyphen (`[a-z]([-a-z0-9]*[a-z0-9])?`), because it becomes a segment of a request URL"
}

func (v urlSafeIdentifier) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (urlSafeIdentifier) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	raw := req.ConfigValue.ValueString()
	if pathSegment.MatchString(raw) {
		return
	}
	// Local: configuration validation, no request made.
	resp.Diagnostics.AddAttributeError(req.Path, "Not a valid identifier",
		fmt.Sprintf("%q has to match `[a-z]([-a-z0-9]*[a-z0-9])?`: lower-case letters, digits and "+
			"hyphens, starting with a letter. It becomes a segment of the request URL, and a "+
			"value carrying `.`, `..`, `/`, `?` or `#` rewrites that URL instead of filling "+
			"the segment.", raw))
}

// positiveDuration is the check on a timeout. The timeouts library accepts any
// Go duration, "-1.5h" included, and withTimeout replaces one of zero or less
// with the provider's default without a word, so a configuration that meant
// "no limit" would get 30 minutes. Refused at plan time instead, with the
// reason.
type positiveDuration struct{}

func (positiveDuration) Description(context.Context) string {
	return "must be a positive duration of hours, minutes or seconds, such as `45m`, `2h` or `1h30m`"
}

func (v positiveDuration) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (positiveDuration) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	raw := req.ConfigValue.ValueString()
	d, err := time.ParseDuration(raw)
	if err == nil && d > 0 {
		return
	}
	// Local: configuration validation, no request made.
	resp.Diagnostics.AddAttributeError(req.Path, "Not a usable timeout",
		fmt.Sprintf("%q isn't a positive duration. Write one such as `45m`, `2h` or `1h30m`. Leave the "+
			"entry out to use the provider's `operation_timeout` instead.", raw))
}
