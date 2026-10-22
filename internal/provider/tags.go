package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Provider-level default tags.
//
// `default_tags` on the provider block puts the same tags on every resource
// that carries `tags`, so a pipeline can stamp `cost-center` or `owner` on
// everything it makes without repeating it in every resource. Each of those
// resources has a computed `tags_all`: the defaults with the resource's own
// `tags` on top, which is what the platform holds.
//
// # Whose shape this is
//
// The shape is AWS's (`default_tags`, `tags_all`), with Google's flat map for
// the provider attribute. Google's `default_labels` made `labels`
// non-authoritative: a label set some other way shows only in
// `effective_labels` and is never removed. This provider's `tags` has always
// been authoritative, a tag set some other way being drift the next apply
// removes, and the platform keeps its own in `systemTags`, which no resource
// reads. So there's nothing for Google's third attribute to hold, and `tags`
// keeps its meaning.
//
// # The merge is planned
//
// ModifyPlan computes `tags_all`, so the plan shows exactly the tags the
// platform will hold, and a change to the defaults alone plans an in-place
// update even though no resource's configuration moved. When either half
// isn't known yet, `tags_all` is unknown, which AWS's framework resources do
// too: a known value guessed from one half would fail the apply.
//
// # The read splits the answer
//
// The platform answers one map. `tags_all` is all of it, less the create
// token (tagAnswer), and `tags` is what the configuration wrote. A
// key goes to the defaults when the configuration didn't write it and its
// value is a default's, or was one at the last read. AWS's first version
// sent every default-valued key to the defaults, which made a resource tag
// equal to a default a perpetual diff, and its fix
// (ResolveDuplicatesFramework) keeps a key the prior `tags` held. That's the
// first half of the rule here. The second half means a default taken out of
// the provider block plans as a change to `tags_all` alone, rather than as a
// tag the configuration never wrote.

// defaultTagsAttribute is the provider attribute's name.
const defaultTagsAttribute = "default_tags"

// tagsAllAttribute is every tagged resource's computed merge.
const tagsAllAttribute = "tags_all"

// defaultTags is the provider's default_tags as one plan, apply or read sees
// the provider block. The zero value is known and empty.
type defaultTags struct {
	// unknown is true when default_tags, or a value in it, waits on
	// something not applied yet.
	unknown bool
	tags    map[string]string
}

// defaultTagsFrom reads the provider attribute.
func defaultTagsFrom(ctx context.Context, v types.Map) (defaultTags, diag.Diagnostics) {
	if v.IsNull() {
		return defaultTags{}, nil
	}
	if !fullyKnown(v) {
		return defaultTags{unknown: true}, nil
	}
	out := map[string]string{}
	diags := v.ElementsAs(ctx, &out, false)
	return defaultTags{tags: out}, diags
}

// defaultTags is the provider's defaults, unknown without a Meta: the
// framework hands none over before Configure has run, and guessing empty
// there would plan tags the apply then contradicts.
func (m *Meta) defaultTags() defaultTags {
	if m == nil {
		return defaultTags{unknown: true}
	}
	return m.DefaultTags
}

// merge is tags_all for a plan: the defaults with the resource's tags on
// top. It's known and never null once both halves are known.
func (d defaultTags) merge(ctx context.Context, tags types.Map) (types.Map, diag.Diagnostics) {
	if d.unknown || !fullyKnown(tags) {
		return types.MapUnknown(types.StringType), nil
	}
	own := map[string]string{}
	var diags diag.Diagnostics
	if !tags.IsNull() {
		diags.Append(tags.ElementsAs(ctx, &own, false)...)
	}
	return types.MapValueFrom(ctx, types.StringType, overlay(d.tags, own))
}

// overlay is base with top's keys written over it, as a new map.
func overlay(base, top map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(top))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range top {
		out[k] = v
	}
	return out
}

// attribute splits a read's tags, the create token already out, into the
// configuration's and the whole map. priorTags and priorAll are the prior
// state's or the plan's. See the rule at the top of this file.
func (d defaultTags) attribute(answer, priorTags, priorAll types.Map) (tags, all types.Map) {
	answered := stringElements(answer)
	all = types.MapValueMust(types.StringType, mapElements(answered))
	if answer.IsNull() {
		return answer, all
	}

	written := stringElements(priorTags)
	inherited := inheritedTags(priorTags, priorAll)
	own := map[string]string{}
	for k, v := range answered {
		if _, wrote := written[k]; !wrote && (d.supplies(k, v) || isPair(inherited, k, v)) {
			continue
		}
		own[k] = v
	}
	return types.MapValueMust(types.StringType, mapElements(own)), all
}

// supplies reports whether the defaults are known and set k to v.
func (d defaultTags) supplies(k, v string) bool {
	return !d.unknown && isPair(d.tags, k, v)
}

func isPair(m map[string]string, k, v string) bool {
	got, ok := m[k]
	return ok && got == v
}

// inheritedTags is what the last read gave the defaults: tags_all's keys that
// tags doesn't have.
func inheritedTags(tags, all types.Map) map[string]string {
	written := stringElements(tags)
	out := map[string]string{}
	for k, v := range stringElements(all) {
		if _, ok := written[k]; !ok {
			out[k] = v
		}
	}
	return out
}

// stringElements is a map of strings' known elements, empty for a null or
// unknown map.
func stringElements(m types.Map) map[string]string {
	out := map[string]string{}
	if m.IsNull() || m.IsUnknown() {
		return out
	}
	for k, v := range m.Elements() {
		if s, ok := v.(types.String); ok && !s.IsNull() && !s.IsUnknown() {
			out[k] = s.ValueString()
		}
	}
	return out
}

// tagAnswer is one platform answer's tags, read once so every reader agrees.
// The create token is dropped on read, so it never reaches state or drift.
type tagAnswer struct {
	present   bool // the answer had a tags field
	onlyToken bool // the create token was its only tag
	// tags are the string-valued tags, which a list's `tags` filter matches.
	tags map[string]string
	// unreadable are the non-string values security-group tags may hold (see
	// freeformTags).
	unreadable map[string]any
}

// tagAnswerOf reads an optional tags field. V is string, or any for freeform
// tags.
func tagAnswerOf[V any](tags *map[string]V) tagAnswer {
	if tags == nil {
		return tagAnswer{}
	}
	a := tagAnswer{present: true, tags: make(map[string]string, len(*tags))}
	_, token := (*tags)[createTokenTag()]
	a.onlyToken = token && len(*tags) == 1
	for k, v := range *tags {
		if k == createTokenTag() {
			continue
		}
		if s, ok := any(v).(string); ok {
			a.tags[k] = s
			continue
		}
		if a.unreadable == nil {
			a.unreadable = map[string]any{}
		}
		a.unreadable[k] = v
	}
	return a
}

// tagAnswerOfRequired reads a required tags field, where empty means absent
// (see presentMap).
func tagAnswerOfRequired[V any](tags map[string]V) tagAnswer {
	return tagAnswerOf(presentMap(tags))
}

// forResource is the answer for a resource's read, with prior as the prior
// state's or plan's tags. A token-only map reads as {}, not null. A non-string
// tag is warned about and left out: keeping prior would hide the difference,
// and rendering it would propose a change on every plan. If only such tags
// remain and prior isn't null, prior stands.
func (a tagAnswer) forResource(prior types.Map) (types.Map, diag.Diagnostics) {
	var diags diag.Diagnostics
	if !a.present {
		return types.MapNull(types.StringType), diags
	}
	keys := make([]string, 0, len(a.unreadable))
	for k := range a.unreadable {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		diags.AddWarning(
			"A tag on this resource is not a string",
			fmt.Sprintf("Tag %q holds a %T, which a Terraform map of strings cannot represent. "+
				"It was set through something other than this provider and is left alone.", k, a.unreadable[k]),
		)
	}
	if len(a.tags) == 0 && len(a.unreadable) > 0 && !prior.IsNull() {
		return prior, diags
	}
	return types.MapValueMust(types.StringType, mapElements(a.tags)), diags
}

// forDataSource is the answer for a singular data source. A map holding only
// the token reads as null, as it did before the provider sent one.
func (a tagAnswer) forDataSource() types.Map {
	if !a.present || a.onlyToken {
		return types.MapNull(types.StringType)
	}
	return types.MapValueMust(types.StringType, mapElements(a.tags))
}

// forList is a listed object's tags: null when it has none, as a resource
// reads them (see presentMap).
func (a tagAnswer) forList() types.Map {
	if len(a.tags) == 0 {
		return types.MapNull(types.StringType)
	}
	return types.MapValueMust(types.StringType, mapElements(a.tags))
}

// planTagsAll is the ModifyPlan step every tagged resource takes first: it
// puts the merge in the plan. A destroy has no plan to put it in.
func planTagsAll(ctx context.Context, meta *Meta, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var tags types.Map
	resp.Diagnostics.Append(resp.Plan.GetAttribute(ctx, path.Root("tags"), &tags)...)
	if resp.Diagnostics.HasError() {
		return
	}
	all, diags := meta.defaultTags().merge(ctx, tags)
	resp.Diagnostics.Append(diags...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(tagsAllAttribute), all)...)
}

// planTagsAllAtCreation is planTagsAll for a kind whose tags can only be set
// when it's created, the VPC peering. A create or a replacement takes the
// merge. Anything else keeps the tags it has, because a change to those would
// replace it and a change to the provider block's defaults shouldn't
// replace every peering it reaches. When the defaults no longer match, the
// plan says so and how to apply the new ones.
func planTagsAllAtCreation(ctx context.Context, meta *Meta, noun string, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	if req.State.Raw.IsNull() {
		planTagsAll(ctx, meta, req, resp)
		return
	}
	replacing, ok := plannedReplacements(ctx, req, resp)
	if !ok || len(replacing) > 0 {
		planTagsAll(ctx, meta, req, resp)
		return
	}

	var tags, held types.Map
	resp.Diagnostics.Append(resp.Plan.GetAttribute(ctx, path.Root("tags"), &tags)...)
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root(tagsAllAttribute), &held)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root(tagsAllAttribute), held)...)

	want, diags := meta.defaultTags().merge(ctx, tags)
	resp.Diagnostics.Append(diags...)
	if want.IsUnknown() || held.IsNull() || want.Equal(held) {
		return
	}
	resp.Diagnostics.AddAttributeWarning(path.Root(tagsAllAttribute),
		fmt.Sprintf("This %s keeps the tags it was created with", noun),
		fmt.Sprintf("The provider's %s would give it %s, and it has %s. The API sets a %s's tags only "+
			"when it's created, so a change to the defaults isn't applied to one that exists. "+
			"`terraform apply -replace` recreates it with the new tags.",
			defaultTagsAttribute, describeTags(want), describeTags(held), noun))
}

// describeTags is a map of tags as a plan reader would write it.
func describeTags(m types.Map) string {
	elems := stringElements(m)
	if len(elems) == 0 {
		return "no tags"
	}
	keys := make([]string, 0, len(elems))
	for k := range elems {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s = %q", escapeControl(k), elems[k]))
	}
	return "{ " + strings.Join(parts, ", ") + " }"
}

// tagsChangeReplaces is the replacement rule for tags that can only be set
// at creation: a change to `tags` replaces only when it changes the tags the
// platform would hold. Two readings of "would hold" keep the resource: the
// configured defaults with the planned tags on top, and what the last read
// gave the defaults with the planned tags on top. The first is a tag moved
// between the resource and default_tags with the same value. The second is a
// resource that kept an old default's value when the defaults changed, which
// planTagsAllAtCreation promises it does. With the defaults not known, only
// the second can be asked.
func tagsChangeReplaces(stateTags, stateAll, planTags types.Map, defaults defaultTags) bool {
	if stateAll.IsNull() || stateAll.IsUnknown() || !fullyKnown(planTags) {
		return true
	}
	held := stringElements(stateAll)
	planned := stringElements(planTags)
	if sameTagMap(overlay(inheritedTags(stateTags, stateAll), planned), held) {
		return false
	}
	return defaults.unknown || !sameTagMap(overlay(defaults.tags, planned), held)
}

func sameTagMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !isPair(b, k, v) {
			return false
		}
	}
	return true
}

// replaceTagsIfTheyChange is tagsChangeReplaces as a plan modifier. The
// framework builds plan modifiers from a schema it asks an unconfigured
// resource for, so the defaults come from the provider that registered the
// resource rather than from the resource's Meta.
func replaceTagsIfTheyChange(defaults *providerDefaults) planmodifier.Map {
	const why = "Replaces the resource when the tags the platform would hold change, and not when it " +
		"writes out a tag the provider's `" + defaultTagsAttribute + "` already supplies with the same " +
		"value, or leaves one there."
	return mapplanmodifier.RequiresReplaceIf(func(ctx context.Context, req planmodifier.MapRequest, resp *mapplanmodifier.RequiresReplaceIfFuncResponse) {
		var stateAll types.Map
		resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root(tagsAllAttribute), &stateAll)...)
		resp.RequiresReplace = tagsChangeReplaces(req.StateValue, stateAll, req.PlanValue, defaults.load())
	}, why, why)
}

// providerDefaults is the provider block's default_tags as Configure last
// resolved the block, shared by the provider with the resources it registers. A
// plan modifier reads it, since it can't reach a Meta (see
// replaceTagsIfTheyChange). Terraform runs one provider process per provider
// configuration, so there's one set of defaults to hold.
type providerDefaults struct {
	mu   sync.Mutex
	set  bool
	tags defaultTags
}

func (c *providerDefaults) store(d defaultTags) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.set, c.tags = true, d
}

// load is the defaults, unknown before Configure has stored any or with no
// provider to ask.
func (c *providerDefaults) load() defaultTags {
	if c == nil {
		return defaultTags{unknown: true}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.set {
		return defaultTags{unknown: true}
	}
	return c.tags
}

// tagsAllSchemaAttribute is the computed merge every tagged resource carries.
func tagsAllSchemaAttribute(note string) schema.MapAttribute {
	return schema.MapAttribute{
		Computed:    true,
		ElementType: types.StringType,
		MarkdownDescription: "Every tag the platform holds for this resource: the provider's `" +
			defaultTagsAttribute + "` with this resource's own `tags` on top, a resource tag winning over " +
			"a default with the same key. Known at plan whenever both are, so the plan shows the result. " +
			"The provider's create token never appears here." + note,
	}
}

// reservedTagKey refuses the create token's key in a map of tags. The
// provider writes that key on every create and takes it out of every read, so
// a value set in configuration would vanish from state on the first refresh.
type reservedTagKey struct{}

func (reservedTagKey) Description(context.Context) string {
	return "The key `" + createTokenTag() + "` is the provider's own."
}

func (v reservedTagKey) MarkdownDescription(ctx context.Context) string { return v.Description(ctx) }

func (reservedTagKey) ValidateMap(_ context.Context, req validator.MapRequest, resp *validator.MapResponse) {
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	if _, ok := req.ConfigValue.Elements()[createTokenTag()]; ok {
		resp.Diagnostics.AddAttributeError(req.Path, "A tag key the provider reserves",
			fmt.Sprintf("`%s` carries the provider's create token, which it sets on every create and "+
				"takes out of every read. A value written here would never read back. Use another key.",
				createTokenTag()))
	}
}

// mapElements is a map of strings as the framework's elements.
func mapElements(m map[string]string) map[string]attr.Value {
	out := make(map[string]attr.Value, len(m))
	for k, v := range m {
		out[k] = types.StringValue(v)
	}
	return out
}

// fullyKnown reports whether a map and every value in it are known. A null
// map is known.
func fullyKnown(m types.Map) bool {
	if m.IsUnknown() {
		return false
	}
	for _, v := range m.Elements() {
		if v.IsUnknown() {
			return false
		}
	}
	return true
}
