package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// tenantMetadata is the display name, description and tags a resource
// carries: the three fields every resource names itself with, and the only
// ones a tenant can change on most of them.
//
// Each verb has one rule for them, and getting any one wrong is a visible
// failure. A create leaves out what the configuration left out. A PATCH clears
// what the configuration removed, because leaving the key out keeps the old
// value, and sends nothing at all when nothing changed. A read gives a
// configuration back what it wrote, even where the platform answers an unset
// field as "" or {}. Those rules live here so a resource asks once per verb
// rather than choosing helpers.
//
// A resource without tags leaves Tags as its zero value, and every rule below
// reads that as no tags.
//
// TagsAll is the computed merge with the provider's default_tags (tags.go),
// and it's what a request carries, since it's what the platform holds. A
// resource without tags_all leaves it null, and Tags is sent instead.
type tenantMetadata struct {
	DisplayName types.String
	Description types.String
	Tags        types.Map
	TagsAll     types.Map
}

// sent is the map a request carries for these tags: tags_all where the
// resource has it and it holds something, the configured tags otherwise.
func (m tenantMetadata) sent() types.Map {
	if m.TagsAll.IsNull() || m.TagsAll.IsUnknown() || len(m.TagsAll.Elements()) == 0 {
		return m.Tags
	}
	return m.TagsAll
}

// metadataFields is the trio as a request body carries it. The generated
// request types stay as generated (ADR D4), so a resource copies these into
// its own body.
type metadataFields struct {
	DisplayName *string
	Description *string
	Tags        *map[string]string
}

// metadataAnswer is the trio as the platform answered it. Tags are every tag
// the platform holds but its own `systemTags`, with the create token already
// taken out, and null when the answer had no tags field. Defaults are the
// provider's default_tags, which decide which of those tags the
// configuration wrote.
type metadataAnswer struct {
	DisplayName *string
	Description *string
	Tags        types.Map
	Defaults    defaultTags
}

// create is the trio for a create body. A null stays out of the request.
func (m tenantMetadata) create(ctx context.Context) (metadataFields, diag.Diagnostics) {
	tags, diags := optionalStringMap(ctx, m.sent())
	return metadataFields{
		DisplayName: optionalString(m.DisplayName),
		Description: optionalString(m.Description),
		Tags:        tags,
	}, diags
}

// patch is the trio for a PATCH from the plan m and the prior state, and
// whether any of it changed. Unchanged means no request is needed for it: an
// update that changed only `timeouts` or `allow_replace` makes none.
//
// The fields are complete either way, for an operation that sends a PATCH for
// other reasons: a value still configured is sent as it is, and one removed
// from configuration is sent as "" or {}, which is how the platform clears it.
func (m tenantMetadata) patch(ctx context.Context, state tenantMetadata) (metadataFields, bool, diag.Diagnostics) {
	changed := !m.DisplayName.Equal(state.DisplayName) ||
		!m.Description.Equal(state.Description) ||
		!sameTags(m.sent(), state.sent())

	out := metadataFields{
		DisplayName: optionalString(m.DisplayName),
		Description: optionalString(m.Description),
	}
	empty := ""
	if removedFromConfig(m.DisplayName, state.DisplayName) {
		out.DisplayName = &empty
	}
	if removedFromConfig(m.Description, state.Description) {
		out.Description = &empty
	}
	if plan, prior := m.sent(), state.sent(); plan.IsNull() && !prior.IsNull() && !prior.IsUnknown() ||
		len(plan.Elements()) == 0 && len(prior.Elements()) > 0 {
		out.Tags = &map[string]string{}
		return out, changed, nil
	}
	tags, diags := optionalStringMap(ctx, m.sent())
	out.Tags = tags
	return out, changed, diags
}

// read is the trio as state should hold it after the platform's answer, with m
// as the prior state or plan.
//
// The platform answers a field that was never set, or that a PATCH cleared,
// as "" rather than leaving it out, and that is null to a configuration that
// never set it. Tags are the same with {}, and the other way round as well: a
// platform that answers an empty map by leaving it out has not contradicted a
// configuration that wrote `tags = {}`. Strings are the same both ways too: a
// PATCH that sends "" can come back with the field left out, and that hasn't
// contradicted a configuration that wrote "".
//
// Tags are split first: tags_all is the whole answer, and tags is the part
// the configuration wrote (tags.go says how that's decided).
func (m tenantMetadata) read(answer metadataAnswer) tenantMetadata {
	tags, all := answer.Defaults.attribute(answer.Tags, m.Tags, m.TagsAll)
	return tenantMetadata{
		DisplayName: answeredString(answer.DisplayName, m.DisplayName),
		Description: answeredString(answer.Description, m.Description),
		Tags:        answeredTags(tags, m.Tags),
		TagsAll:     all,
	}
}

func removedFromConfig(plan, state types.String) bool {
	return plan.IsNull() && !state.IsNull() && !state.IsUnknown()
}

// sameTags is Equal with a resource that has no tags in mind: two null maps
// are the same however they were made.
func sameTags(a, b types.Map) bool {
	if a.IsNull() && b.IsNull() {
		return true
	}
	return a.Equal(b)
}

func answeredString(p *string, prior types.String) types.String {
	empty := p == nil || *p == ""
	switch {
	case empty && prior.IsNull():
		return types.StringNull()
	case empty && !prior.IsUnknown() && prior.ValueString() == "":
		return prior
	}
	return stringOrNull(p)
}

// answeredTags keeps any tags the answer has, then a prior empty map whatever
// the answer, then an answered empty map only where the configuration had
// tags to clear.
func answeredTags(answer, prior types.Map) types.Map {
	switch {
	case !answer.IsNull() && len(answer.Elements()) > 0:
		return answer
	case !prior.IsNull() && !prior.IsUnknown() && len(prior.Elements()) == 0:
		return prior
	case !answer.IsNull() && !prior.IsNull():
		return answer
	default:
		return types.MapNull(types.StringType)
	}
}
