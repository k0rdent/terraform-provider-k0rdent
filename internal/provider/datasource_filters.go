package provider

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// The filters a plural data source takes on what its objects carry: `tags`,
// `display_name_regex`, and `state` or `status` under the name the objects
// themselves use. A module can then find "the network tagged env=prod"
// without hardcoding its id.
//
// The regex is `display_name_regex` rather than AWS's `name_regex` because
// AWS names the regex after the attribute it matches (`aws_ami`'s
// `name_regex` matches `name`), and here that attribute is `display_name`.
// Every object in this provider documents its `id` as its name, so a
// `name_regex` would read as matching the id.
//
// Most of these lists take no filter at all, so most filtering happens here,
// after every page is read. Where the operation does take the filter, it's
// sent, and what comes back is checked again anyway: a platform that ignored
// the parameter would otherwise hand back rows the configuration said to
// leave out, and the check costs nothing.

// objectFilter is one data source's configured filters. A zero part matches
// everything.
type objectFilter struct {
	tags        map[string]string
	displayName *regexp.Regexp
	state       string
}

// filterSubject is what one listed object offers the filter. A nil
// displayName is an object with none, which no regex matches.
type filterSubject struct {
	tags        map[string]string
	displayName *string
	state       string
}

func (f objectFilter) keeps(s filterSubject) bool {
	if !tagsMatch(f.tags, s.tags) {
		return false
	}
	if f.displayName != nil && (s.displayName == nil || !f.displayName.MatchString(*s.displayName)) {
		return false
	}
	return f.state == "" || s.state == f.state
}

// tagsMatch is true when every pair in want is in have with the same value.
// Tags beyond those are ignored, so a filter on one tag finds objects that
// carry more.
func tagsMatch(want, have map[string]string) bool {
	for k, v := range want {
		if got, ok := have[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// readObjectFilter turns the configured filter attributes into an
// objectFilter. Pass a null for the ones a data source doesn't have.
//
// The regex was checked at plan time when it was known then. One that only
// became known at apply is compiled here for the first time, so a bad one is
// still an error on the attribute rather than a crash.
func readObjectFilter(ctx context.Context, tags types.Map, displayNameRegex, state types.String) (objectFilter, diag.Diagnostics) {
	var f objectFilter
	var diags diag.Diagnostics
	if !tags.IsNull() && !tags.IsUnknown() {
		diags.Append(tags.ElementsAs(ctx, &f.tags, false)...)
	}
	if !displayNameRegex.IsNull() && !displayNameRegex.IsUnknown() {
		re, err := regexp.Compile(displayNameRegex.ValueString())
		if err != nil {
			// Local: configuration validation, no request made.
			diags.AddAttributeError(path.Root("display_name_regex"), "Not a regular expression", err.Error())
		}
		f.displayName = re
	}
	if !state.IsNull() && !state.IsUnknown() {
		f.state = state.ValueString()
	}
	return f, diags
}

// The values each `state` or `status` filter accepts, read from the
// contract's own enums so a value the platform adds is accepted on the next
// re-pin rather than refused by a list that missed it.
var (
	networkStatesFilter       = enumStrings(k0rdent.NetworkEVPNStateValues()...)
	subnetStatesFilter        = enumStrings(k0rdent.SubnetStateValues()...)
	securityGroupStatesFilter = enumStrings(k0rdent.ListSecurityGroupsParamsStateValues()...)
	vpcStatesFilter           = enumStrings(k0rdent.VPCStateValues()...)
	peeringStatesFilter       = enumStrings(k0rdent.VPCPeeringStateValues()...)
	addressPoolStatesFilter   = enumStrings(k0rdent.AddressPoolStateValues()...)
	shareTypeStatesFilter     = enumStrings(k0rdent.ShareTypeStateValues()...)
	instanceStatesFilter      = enumStrings(k0rdent.ListInstancesParamsInStatesValues()...)
	projectStatusesFilter     = enumStrings(k0rdent.ListProjectsParamsStatusValues()...)
	clusterTypeStatusesFilter = enumStrings(k0rdent.ClusterTypeVersionStatusValues()...)
)

func enumStrings[T ~string](values ...T) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = string(v)
	}
	return out
}

// oneOfEnum accepts exactly the members of a generated enum, as its Values()
// lists them.
func oneOfEnum[T ~string](values []T) validator.String {
	return stringvalidator.OneOf(enumStrings(values...)...)
}

// enumProse is a generated enum's members for a description, in its Values()
// order, such as "`permit` or `deny`", so the text names what the validator
// beside it accepts.
func enumProse[T ~string](values []T) string {
	quoted := make([]string, len(values))
	for i, v := range values {
		quoted[i] = "`" + string(v) + "`"
	}
	if len(quoted) < 2 {
		return strings.Join(quoted, "")
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + " or " + quoted[len(quoted)-1]
}

// listedTagsAttribute is `tags` on one listed object, so that what a `tags`
// filter matched on can be read back.
func listedTagsAttribute() schema.MapAttribute {
	return schema.MapAttribute{
		Computed:            true,
		ElementType:         types.StringType,
		MarkdownDescription: "User-defined labels, as key-value pairs. Null when the object has none.",
	}
}

const filteredInProvider = "Filtered in the provider, after the whole list is read: the list operation takes no such filter."

// tagsFilterAttribute is the `tags` filter for a list of `noun`.
func tagsFilterAttribute(noun string) schema.MapAttribute {
	return schema.MapAttribute{
		Optional:    true,
		ElementType: types.StringType,
		MarkdownDescription: "Return only the " + noun + " carrying every one of these tags with " +
			"exactly this value. Tags beyond these don't matter. " + filteredInProvider,
		Validators: []validator.Map{noNullTagValue{}},
	}
}

// displayNameRegexAttribute is the `display_name_regex` filter for a list of
// `noun`.
func displayNameRegexAttribute(noun string) schema.StringAttribute {
	return schema.StringAttribute{
		Optional: true,
		MarkdownDescription: "Return only the " + noun + " whose `display_name` matches this " +
			"regular expression, in the RE2 syntax Terraform's own `regex` function takes. The " +
			"match isn't anchored, so `prod` finds `preprod-a`: write `^prod-` for a prefix. One " +
			"with no `display_name` never matches. " + filteredInProvider,
		Validators: []validator.String{validRegex{}},
	}
}

// stateFilterAttribute is the `state` or `status` filter for a list of
// `noun`, whose objects report one of values under the attribute `attr`.
// sentToPlatform says the list operation takes the filter itself.
func stateFilterAttribute(noun, attr string, values []string, sentToPlatform bool) schema.StringAttribute {
	where := filteredInProvider
	if sentToPlatform {
		where = "Sent to the platform as the list operation's own filter, and checked again on " +
			"what comes back."
	}
	return schema.StringAttribute{
		Optional:            true,
		MarkdownDescription: "Return only the " + noun + " whose `" + attr + "` is this value. " + where,
		Validators:          []validator.String{stringvalidator.OneOf(values...)},
	}
}

// noNullTagValue refuses a `tags` filter with a null value, such as one
// taken from a variable that wasn't set. Left to the read, it would fail as
// the framework's own conversion error with no word on what to change. An
// unknown value passes, as every validator here lets one through, and is
// caught at read the same way.
type noNullTagValue struct{}

func (noNullTagValue) Description(context.Context) string {
	return "every tag value must be set: a null value matches nothing"
}

func (v noNullTagValue) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (noNullTagValue) ValidateMap(_ context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	for k, v := range req.ConfigValue.Elements() {
		if v.IsNull() {
			// Local: configuration validation, no request made.
			resp.Diagnostics.AddAttributeError(req.Path.AtMapKey(k), "A tag filter value is null",
				fmt.Sprintf("Tag %q has no value. A filter matches a tag by its value, so give it one, "+
					"or leave the key out to match regardless of that tag.", k))
		}
	}
}

// validRegex refuses a pattern Go's regexp package can't compile, at plan
// time rather than on the read that would use it. Go's package is RE2, which
// is what Terraform's `regex` function uses too, so a pattern that works in
// one works in the other. Perl-only syntax such as lookahead is refused by
// both.
type validRegex struct{}

func (validRegex) Description(context.Context) string {
	return "must be a regular expression in RE2 syntax"
}

func (v validRegex) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (validRegex) ValidateString(_ context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, err := regexp.Compile(req.ConfigValue.ValueString()); err != nil {
		// Local: configuration validation, no request made.
		resp.Diagnostics.AddAttributeError(req.Path, "Not a regular expression",
			fmt.Sprintf("%q isn't a regular expression RE2 accepts: %v. RE2 is the syntax "+
				"Terraform's own `regex` function takes, and it has no lookaround or backreferences.",
				req.ConfigValue.ValueString(), err))
	}
}
