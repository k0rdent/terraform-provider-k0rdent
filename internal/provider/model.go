package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Conversions between Terraform values and the generated client's pointers.
//
// The pointers are the point. ADR D4 keeps the generated types as generated
// precisely so that unset and zero stay distinguishable: a `*string` with
// `omitempty` drops when nil and serialises when it points at "". Terraform has
// the same distinction: null is not "". So these helpers are a straight
// mapping, and the one thing they must never do is turn a null into a zero.

// optionalString maps a Terraform string to a request pointer. Null and unknown
// both become nil, so the field is omitted from the request body rather than
// sent as "".
func optionalString(v types.String) *string {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	s := v.ValueString()
	return &s
}

// stringOrNull maps a response pointer back to a Terraform string. An absent
// value is null, never "".
func stringOrNull(p *string) types.String {
	if p == nil {
		return types.StringNull()
	}
	return types.StringValue(*p)
}

// textOrNull is stringOrNull for a field the platform always sends, as "" when
// there's nothing in it. Only for read-only attributes: a configured "" would
// read back as null.
func textOrNull(p *string) types.String {
	if p == nil || *p == "" {
		return types.StringNull()
	}
	return types.StringValue(*p)
}

// optionalStringMap maps a Terraform map to a request pointer, preserving the
// difference between "no map" and "an empty map".
func optionalStringMap(ctx context.Context, v types.Map) (*map[string]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if v.IsNull() || v.IsUnknown() {
		return nil, diags
	}
	out := map[string]string{}
	diags.Append(v.ElementsAs(ctx, &out, false)...)
	if diags.HasError() {
		return nil, diags
	}
	return &out, diags
}

// configureMeta pulls the provider's Meta out of the framework's untyped
// ProviderData. It tolerates nil, which the framework passes during the
// validate and early plan phases before Configure has run.
func configureMeta(providerData any, diags *diag.Diagnostics) *Meta {
	if providerData == nil {
		return nil
	}
	meta, ok := providerData.(*Meta)
	if !ok {
		diags.AddError(
			"Unexpected provider data",
			fmt.Sprintf("Expected *provider.Meta, got %T. This is always a bug in the provider.", providerData),
		)
		return nil
	}
	return meta
}

// providerConfigured reports whether Configure handed over a Meta, adding the
// error when it did not. Terraform configures the provider before it calls
// anything else, and Configure either sets the Meta or fails the run, so this
// is the provider's own invariant. Checking it turns a broken invariant into a
// diagnostic rather than a nil dereference that kills the plugin.
func providerConfigured(meta *Meta, diags *diag.Diagnostics) bool {
	if meta != nil {
		return true
	}
	// Local: no request made and no upstream code.
	diags.AddError(
		"The provider was not configured",
		"This was called before the provider's configuration was applied, so there is no "+
			"client to send the request with. This is always a bug in the provider.",
	)
	return false
}

// importID parses a resource import address.
//
// Two accepted forms, and both are a permanent public API once anyone scripts
// them, so they are stated rather than grown:
//
//	<id>                      use the provider's configured region and project
//	<region>/<project>/<id>   ignore the provider's targeting for this resource
//
// Every part becomes a segment of the request URL and the import id reaches
// state without passing a schema validator, so the id is checked here with
// the region and project, rather than left to the request it would rewrite.
//
// The triple writes region and project into state. State alone does not hold
// the resource there: the resource block has to repeat them, or the next plan
// resolves the scope from the provider block and proposes a replacement.
func importID(raw, resourceName string) (id, region, project string, diags diag.Diagnostics) {
	id = strings.TrimSpace(raw)

	switch strings.Count(id, "/") {
	case 0:
		if id == "" {
			diags.AddError(
				"Empty import id",
				fmt.Sprintf("Import %s with the resource's id, for example:\n\n"+
					"    terraform import %s.example my-cluster\n\n"+
					"The region and project come from the provider configuration.",
					resourceName, resourceName),
			)
			return id, "", "", diags
		}
		diags.Append(validatePathSegment("id", id)...)
		return id, "", "", diags
	case 2:
		parts := strings.Split(id, "/")
		for i, part := range parts {
			if strings.TrimSpace(part) == "" {
				diags.AddError(
					"Incomplete import id",
					fmt.Sprintf("Got %q. Segment %d of <region>/<project>/<id> is empty.", id, i+1),
				)
				return "", "", "", diags
			}
		}
		diags.Append(validateRegion(parts[0])...)
		diags.Append(validateProject(parts[1])...)
		diags.Append(validatePathSegment("id", parts[2])...)
		if diags.HasError() {
			return "", "", "", diags
		}
		return parts[2], parts[0], parts[1], diags
	default:
		diags.AddError(
			"Unrecognised import id",
			fmt.Sprintf("Got %q. Import %s as <id> or <region>/<project>/<id>.",
				id, resourceName),
		)
		return "", "", "", diags
	}
}

// zeroPtr allocates the zero value of whatever a pointer field points at.
//
// The contract nests several one-field objects, such as a network's `vlan`
// and an NVLink's `nvlink`, and the generator renders those as pointers to
// anonymous structs. An anonymous struct type cannot be written down a second time
// without repeating its json tags exactly, and a tag that drifts silently
// changes the wire format. Allocating from the field itself cannot drift.
func zeroPtr[T any](_ *T) *T { return new(T) }

// keepConfigured chooses between what the user wrote and what the platform
// returned, for an immutable attribute the platform may resolve or default.
//
// Taking the platform's value unconditionally is what produces a permanent diff
// on `vlan_id = "auto"`: the config says `auto`, the response says `100`, and
// every subsequent plan proposes replacing a working network. So a configured
// value stays, and the resolved value is published beside it. Same shape as
// `cluster_type` / `cluster_type_resolved`.
func keepConfigured(prior types.String, returned *string) types.String {
	if !prior.IsNull() && !prior.IsUnknown() {
		return prior
	}
	return stringOrNull(returned)
}

// optionalBool and optionalInt are optionalString for the other two scalar
// kinds. Same rule: a null stays out of the request body.
func optionalBool(v types.Bool) *bool {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	b := v.ValueBool()
	return &b
}

func optionalInt(v types.Int64) *int {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	n := int(v.ValueInt64())
	return &n
}

// optionalStringList maps a Terraform list to a request pointer, preserving the
// difference between "no list" and "an empty list".
func optionalStringList(ctx context.Context, v types.List) (*[]string, diag.Diagnostics) {
	var diags diag.Diagnostics
	if v.IsNull() || v.IsUnknown() {
		return nil, diags
	}
	out := []string{}
	diags.Append(v.ElementsAs(ctx, &out, false)...)
	if diags.HasError() {
		return nil, diags
	}
	return &out, diags
}

// stringSetOrNull is stringListOrNull for a collection the server declares no
// order for. Separate rather than a flag, because which one a caller wants is a
// statement about the API and belongs at the call site.
func stringSetOrNull(ctx context.Context, p *[]string) (types.Set, diag.Diagnostics) {
	if p == nil {
		return types.SetNull(types.StringType), nil
	}
	return types.SetValueFrom(ctx, types.StringType, *p)
}

func stringListOrNull(ctx context.Context, p *[]string) (types.List, diag.Diagnostics) {
	if p == nil {
		return types.ListNull(types.StringType), nil
	}
	return types.ListValueFrom(ctx, types.StringType, *p)
}

func stringMapOrNull(ctx context.Context, p *map[string]string) (types.Map, diag.Diagnostics) {
	if p == nil {
		return types.MapNull(types.StringType), nil
	}
	return types.MapValueFrom(ctx, types.StringType, *p)
}

// int64OrNull is generic because the contract sets `format: int32` on some
// counts and leaves others plain, so the generated widths differ across
// operations. Terraform has one integer type either way.
func int64OrNull[T ~int | ~int32 | ~int64](p *T) types.Int64 {
	if p == nil {
		return types.Int64Null()
	}
	return types.Int64Value(int64(*p))
}

func boolOrNull(p *bool) types.Bool {
	if p == nil {
		return types.BoolNull()
	}
	return types.BoolValue(*p)
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// stateString flattens one of the contract's per-resource state enums, each of
// which is its own named string type with the same five members.
func stateString[T ~string](p *T) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

// joinWithAnd renders a list of names the way a sentence needs them.
func joinWithAnd(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], "`, `") + "` and `" + names[len(names)-1]
	}
}

// nestedImportID parses the import address of a resource that lives under a
// parent object.
//
// A subnet's id is unique only inside its network, a peering's only under its
// VPC, and every operation on either takes both, so the parent is part of the
// address rather than something the provider could look up. Two forms, matching
// importID's two:
//
//	<parent>/<id>
//	<region>/<project>/<parent>/<id>
//
// The parent and the id are checked as importID checks its id.
func nestedImportID(raw, resourceName, parentName, example string) (parent, id, region, project string, diags diag.Diagnostics) {
	parts := strings.Split(strings.TrimSpace(raw), "/")
	for i, part := range parts {
		if strings.TrimSpace(part) == "" {
			diags.AddError(
				"Incomplete import id",
				fmt.Sprintf("Got %q. Segment %d is empty.", raw, i+1),
			)
			return "", "", "", "", diags
		}
	}

	switch len(parts) {
	case 2:
		diags.Append(validatePathSegment(parentName, parts[0])...)
		diags.Append(validatePathSegment("id", parts[1])...)
		if diags.HasError() {
			return "", "", "", "", diags
		}
		return parts[0], parts[1], "", "", diags
	case 4:
		diags.Append(validateRegion(parts[0])...)
		diags.Append(validateProject(parts[1])...)
		diags.Append(validatePathSegment(parentName, parts[2])...)
		diags.Append(validatePathSegment("id", parts[3])...)
		if diags.HasError() {
			return "", "", "", "", diags
		}
		return parts[2], parts[3], parts[0], parts[1], diags
	default:
		diags.AddError(
			"Unrecognised import id",
			fmt.Sprintf("Got %q. Import %s as <%s>/<id> or <region>/<project>/<%s>/<id>, "+
				"for example:\n\n    terraform import %s.example %s",
				raw, resourceName, parentName, parentName, resourceName, example),
		)
		return "", "", "", "", diags
	}
}

// freeformTags maps the string tags a request carries onto the contract's
// freeform tag type.
//
// The contract types security-group tag values as `additionalProperties: true`,
// so any JSON value is legal on the wire. A Terraform map has one element type,
// and a string is the only one every value survives a round trip through, so
// the schema takes strings and this widens them. The cost is that a tag someone
// set to a number or an object through another client cannot be represented.
// tagAnswer.forResource says so rather than silently rewriting it.
func freeformTags(strs *map[string]string) *map[string]any {
	if strs == nil {
		return nil
	}
	out := make(map[string]any, len(*strs))
	for k, s := range *strs {
		out[k] = s
	}
	return &out
}

// presentMap is a collection v1.3.0-dev.4 requires on every read, as the
// pointer the readers here were written for. Before dev.4 the contract let
// the platform leave tags out, and a reader took the nil that left as null,
// "nothing here". Now the platform has to answer {} instead, and the address
// of a value is never nil, so an empty map is the nil it replaces. Without
// that, a configuration that sets no tags would read {} back where it reads
// null, and plan a change it can never apply.
func presentMap[V any](m map[string]V) *map[string]V {
	if len(m) == 0 {
		return nil
	}
	return &m
}

// presentList is presentMap for a list, such as `excludeRanges`.
func presentList(s []string) *[]string {
	if len(s) == 0 {
		return nil
	}
	return &s
}
