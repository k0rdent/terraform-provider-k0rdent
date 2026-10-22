package provider

import (
	"context"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/hashicorp/terraform-plugin-framework-timeouts/resource/timeouts"
	"github.com/hashicorp/terraform-plugin-framework/action"
	aschema "github.com/hashicorp/terraform-plugin-framework/action/schema"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	eschema "github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/list"
	lschema "github.com/hashicorp/terraform-plugin-framework/list/schema"
	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/defaults"
)

// The per-attribute reference.
//
// A registry page lists each attribute with whatever its description says.
// Whether changing it forces replacement, what it defaults to, which values it
// accepts and whether it is sensitive are all declared in the schema already,
// as plan modifiers, defaults, validators and flags, and none of them reach the
// page on their own. Written by hand they drift: replacement was in prose on
// some attributes and missing from others. DocumentationGaps reads each fact
// from the schema Terraform enforces and fails when a description doesn't
// carry it, so an attribute added tomorrow can't skip it.

// docSurface is one registered resource, data source, ephemeral resource,
// list resource, action or the provider block, as the documentation sees it.
type docSurface struct {
	// kind is "resource", "data source", "ephemeral resource", "list
	// resource", "action" or "provider".
	kind string
	// name is the Terraform type name, as ResourceName builds it.
	name string
	// page is the generated page's path under docs/, without ".md":
	// "resources/subnet". Empty for the provider block.
	page string
	// doc is the schema's own markdown description.
	doc string
	// attributes and blocks are the schema's maps, whatever package's types
	// they hold. Read through reflection, so one walk covers all four schema
	// packages and every attribute kind, including ones added later.
	attributes reflect.Value
	blocks     reflect.Value
	// importable is true for a resource that implements ImportState.
	importable bool
	// deprecation is the schema's own DeprecationMessage. deprecation.go
	// holds it to the deprecation policy.
	deprecation string
}

// pageDirs maps a surface kind to the directory the documentation generator
// writes its pages to.
var pageDirs = map[string]string{
	"resource":           "resources",
	"data source":        "data-sources",
	"ephemeral resource": "ephemeral-resources",
	"list resource":      "list-resources",
	"action":             "actions",
}

func pagePath(kind, name string) string {
	return pageDirs[kind] + "/" + strings.TrimPrefix(name, ResourcePrefix())
}

// docSurfaces asks the provider for every surface it registers, through the
// same interface Terraform asks.
func docSurfaces(ctx context.Context) []docSurface {
	p := New("docs")()

	var ps fwprovider.SchemaResponse
	p.Schema(ctx, fwprovider.SchemaRequest{}, &ps)
	out := []docSurface{{
		kind: "provider", name: ProviderTypeName, doc: ps.Schema.MarkdownDescription,
		attributes: reflect.ValueOf(ps.Schema.Attributes), blocks: reflect.ValueOf(ps.Schema.Blocks),
		deprecation: ps.Schema.DeprecationMessage,
	}}

	for _, newResource := range p.Resources(ctx) {
		r := newResource()
		var md resource.MetadataResponse
		r.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
		var sr resource.SchemaResponse
		r.Schema(ctx, resource.SchemaRequest{}, &sr)
		_, importable := r.(resource.ResourceWithImportState)
		out = append(out, docSurface{
			kind: "resource", name: md.TypeName, page: pagePath("resource", md.TypeName),
			doc:        sr.Schema.MarkdownDescription,
			attributes: reflect.ValueOf(sr.Schema.Attributes), blocks: reflect.ValueOf(sr.Schema.Blocks),
			importable: importable, deprecation: sr.Schema.DeprecationMessage,
		})
	}

	for _, newDataSource := range p.DataSources(ctx) {
		d := newDataSource()
		var md datasource.MetadataResponse
		d.Metadata(ctx, datasource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
		var sr datasource.SchemaResponse
		d.Schema(ctx, datasource.SchemaRequest{}, &sr)
		out = append(out, docSurface{
			kind: "data source", name: md.TypeName, page: pagePath("data source", md.TypeName),
			doc:        sr.Schema.MarkdownDescription,
			attributes: reflect.ValueOf(sr.Schema.Attributes), blocks: reflect.ValueOf(sr.Schema.Blocks),
			deprecation: sr.Schema.DeprecationMessage,
		})
	}

	if ep, ok := p.(fwprovider.ProviderWithEphemeralResources); ok {
		for _, newEphemeral := range ep.EphemeralResources(ctx) {
			e := newEphemeral()
			var md ephemeral.MetadataResponse
			e.Metadata(ctx, ephemeral.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
			var sr ephemeral.SchemaResponse
			e.Schema(ctx, ephemeral.SchemaRequest{}, &sr)
			out = append(out, docSurface{
				kind: "ephemeral resource", name: md.TypeName, page: pagePath("ephemeral resource", md.TypeName),
				doc:        sr.Schema.MarkdownDescription,
				attributes: reflect.ValueOf(sr.Schema.Attributes), blocks: reflect.ValueOf(sr.Schema.Blocks),
				deprecation: sr.Schema.DeprecationMessage,
			})
		}
	}

	if lp, ok := p.(fwprovider.ProviderWithListResources); ok {
		for _, newList := range lp.ListResources(ctx) {
			l := newList()
			var md resource.MetadataResponse
			l.Metadata(ctx, resource.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
			var sr list.ListResourceSchemaResponse
			l.ListResourceConfigSchema(ctx, list.ListResourceSchemaRequest{}, &sr)
			out = append(out, docSurface{
				kind: "list resource", name: md.TypeName, page: pagePath("list resource", md.TypeName),
				doc:        sr.Schema.MarkdownDescription,
				attributes: reflect.ValueOf(sr.Schema.Attributes), blocks: reflect.ValueOf(sr.Schema.Blocks),
			})
		}
	}

	if ap, ok := p.(fwprovider.ProviderWithActions); ok {
		for _, newAction := range ap.Actions(ctx) {
			a := newAction()
			var md action.MetadataResponse
			a.Metadata(ctx, action.MetadataRequest{ProviderTypeName: ProviderTypeName}, &md)
			var sr action.SchemaResponse
			a.Schema(ctx, action.SchemaRequest{}, &sr)
			out = append(out, docSurface{
				kind: "action", name: md.TypeName, page: pagePath("action", md.TypeName),
				doc:        sr.Schema.MarkdownDescription,
				attributes: reflect.ValueOf(sr.Schema.Attributes), blocks: reflect.ValueOf(sr.Schema.Blocks),
			})
		}
	}
	return out
}

// The markers the fact pass writes. The checks below look for these exact
// words, not for anything that reads like them, so prose saying "never
// forces replacement" can't stand in for the marker.
const (
	forcesReplacementMarker   = "**Forces replacement**"
	canForceReplacementMarker = "**Can force replacement**"
)

// factMarkers start the facts the fact pass appends. A description up to
// the first of them is the part somebody wrote.
var factMarkers = []string{
	forcesReplacementMarker, canForceReplacementMarker, "**Constraints:**", "Defaults to `",
	"**Sensitive:**", "**Write-only:**",
}

// minimumProseWords is the shortest hand-written description that can say
// what an attribute is for. "Id." passes an emptiness check and tells a
// reader nothing.
const minimumProseWords = 3

// Prose that contradicts what the schema does about replacement.
var (
	// deniesReplacement says a change never replaces.
	deniesReplacement = regexp.MustCompile(`(?i)\b(never|doesn't|does not|won't|will not|can't|cannot)\s+(force|replace|be replaced)`)
	// claimsAlwaysReplaced says every change replaces, or that none can be made.
	claimsAlwaysReplaced = regexp.MustCompile(`(?i)\bimmutable\b|\bcan(not|'t)\s+(be\s+)?change`)
	// claimsReplacement says a change replaces at all.
	claimsReplacement = regexp.MustCompile(`(?i)\bimmutable\b|\bcan(not|'t)\s+(be\s+)?change|\bforces? replacement|\breplaces (it|the)\b`)
)

// prose is the hand-written part of a description.
func prose(doc string) string {
	end := len(doc)
	for _, m := range factMarkers {
		if i := strings.Index(doc, m); i >= 0 && i < end {
			end = i
		}
	}
	return strings.TrimSpace(doc[:end])
}

// replacement says how an attribute replaces its resource: never, on every
// change, or under the condition a RequiresReplaceIf names.
type replacement int

const (
	replacesNever replacement = iota
	replacesAlways
	replacesSometimes
)

func replacementOf(ctx context.Context, v reflect.Value, scope bool) replacement {
	if scope {
		return replacesAlways
	}
	descs := replacementDescriptions(ctx, v)
	if len(descs) == 0 {
		return replacesNever
	}
	for _, d := range descs {
		if d == "" || d == requiresReplaceDescription {
			return replacesAlways
		}
	}
	return replacesSometimes
}

// DocumentationGaps lists every place a registered surface's description
// leaves out something its schema declares: an empty description, a
// replacement it doesn't mention, a default, a validator's limits, or a
// sensitive or write-only flag. Empty when the reference is complete.
func DocumentationGaps(ctx context.Context) []string {
	var gaps []string
	for _, s := range docSurfaces(ctx) {
		if strings.TrimSpace(s.doc) == "" {
			gaps = append(gaps, fmt.Sprintf("%s %s: no description", s.kind, s.name))
		}
		walkDocumented(s.attributes, s.kind+" "+s.name+".", s.kind == "resource", func(path string, v reflect.Value, scope bool) {
			gaps = append(gaps, attributeGaps(ctx, path, v, scope)...)
		})
		walkDocumented(s.blocks, s.kind+" "+s.name+".", false, func(path string, v reflect.Value, _ bool) {
			gaps = append(gaps, attributeGaps(ctx, path, v, false)...)
		})
	}
	slices.Sort(gaps)
	return gaps
}

// walkDocumented calls visit for every attribute or block in a schema map and
// everything nested inside it. scopeTop says whether this is a resource's top
// level, where region and project replace the resource from ModifyPlan rather
// than through a plan modifier (scope.go).
func walkDocumented(m reflect.Value, prefix string, scopeTop bool, visit func(path string, v reflect.Value, scope bool)) {
	if !m.IsValid() || m.Kind() != reflect.Map {
		return
	}
	keys := m.MapKeys()
	slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
	for _, k := range keys {
		v := concrete(m.MapIndex(k))
		if !v.IsValid() {
			continue
		}
		name := k.String()
		visit(prefix+name, v, scopeTop && (name == "region" || name == "project"))
		for _, nested := range nestedMaps(v) {
			walkDocumented(nested, prefix+name+".", false, visit)
		}
	}
}

// concrete unwraps a map entry, an interface holding a struct, to the struct.
func concrete(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer) {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	return v
}

// nestedMaps returns the attribute and block maps nested in an attribute or
// block: Attributes and Blocks on a single nested one, and the same on the
// NestedObject of a list, set or map of them.
func nestedMaps(v reflect.Value) []reflect.Value {
	var out []reflect.Value
	holders := []reflect.Value{v}
	if obj := v.FieldByName("NestedObject"); obj.IsValid() && obj.Kind() == reflect.Struct {
		holders = append(holders, obj)
	}
	for _, h := range holders {
		for _, field := range []string{"Attributes", "Blocks"} {
			if f := h.FieldByName(field); f.IsValid() && f.Kind() == reflect.Map && !f.IsNil() {
				out = append(out, f)
			}
		}
	}
	return out
}

// attributeGaps checks one attribute or block's description against its
// schema.
func attributeGaps(ctx context.Context, path string, v reflect.Value, scope bool) []string {
	doc := description(v)
	if strings.TrimSpace(doc) == "" {
		return []string{path + ": no description"}
	}
	var gaps []string
	written := prose(doc)
	if len(strings.Fields(written)) < minimumProseWords {
		gaps = append(gaps, fmt.Sprintf("%s: %q is too short to say what it is for", path, written))
	}
	switch replacementOf(ctx, v, scope) {
	case replacesAlways:
		if !strings.Contains(doc, forcesReplacementMarker) {
			gaps = append(gaps, path+": forces replacement, and its description doesn't say so")
		}
		if deniesReplacement.MatchString(written) {
			gaps = append(gaps, path+": forces replacement, and its prose says it doesn't")
		}
	case replacesSometimes:
		if !strings.Contains(doc, canForceReplacementMarker) {
			gaps = append(gaps, path+": can force replacement, and its description doesn't say so")
		}
		if claimsAlwaysReplaced.MatchString(written) || deniesReplacement.MatchString(written) {
			gaps = append(gaps, path+": replaces only sometimes, and its prose says always or never")
		}
	case replacesNever:
		if claimsReplacement.MatchString(written) {
			gaps = append(gaps, path+": changes in place, and its prose says it replaces")
		}
	}
	for _, limit := range validatorDescriptions(ctx, v) {
		if !strings.Contains(strings.ToLower(doc), strings.ToLower(strings.TrimSuffix(limit, "."))) {
			gaps = append(gaps, fmt.Sprintf("%s: its description doesn't state the validator's limit %q", path, limit))
		}
	}
	if def, ok := defaultValue(ctx, v); ok && !strings.Contains(doc, defaultSentence(def)) {
		gaps = append(gaps, fmt.Sprintf("%s: its description doesn't say it defaults to %s", path, def))
	}
	if strings.Contains(path, ".timeouts.") && !strings.Contains(doc, "`"+shortDuration(DefaultOperationTimeout)+"`") {
		gaps = append(gaps, path+": a timeout whose description doesn't give its default")
	}
	if boolField(v, "Sensitive") && !strings.Contains(strings.ToLower(doc), "sensitive") {
		gaps = append(gaps, path+": sensitive, and its description doesn't say so")
	}
	if boolField(v, "WriteOnly") && !strings.Contains(strings.ToLower(doc), "write-only") {
		gaps = append(gaps, path+": write-only, and its description doesn't say so")
	}
	return gaps
}

// description is the attribute's markdown description, or its plain one when
// only that is set.
func description(v reflect.Value) string {
	if f := v.FieldByName("MarkdownDescription"); f.IsValid() && f.String() != "" {
		return f.String()
	}
	if f := v.FieldByName("Description"); f.IsValid() {
		return f.String()
	}
	return ""
}

func boolField(v reflect.Value, name string) bool {
	f := v.FieldByName(name)
	return f.IsValid() && f.Kind() == reflect.Bool && f.Bool()
}

type describer interface {
	MarkdownDescription(context.Context) string
}

// listField returns the elements of a slice field, such as Validators or
// PlanModifiers, whatever their element type.
func listField(v reflect.Value, name string) []any {
	f := v.FieldByName(name)
	if !f.IsValid() || f.Kind() != reflect.Slice {
		return nil
	}
	out := make([]any, 0, f.Len())
	for i := range f.Len() {
		out = append(out, f.Index(i).Interface())
	}
	return out
}

// validatorDescriptions is what each of an attribute's validators says it
// accepts.
func validatorDescriptions(ctx context.Context, v reflect.Value) []string {
	var out []string
	for _, val := range listField(v, "Validators") {
		if d, ok := val.(describer); ok {
			if text := strings.TrimSpace(d.MarkdownDescription(ctx)); text != "" {
				out = append(out, text)
			}
		}
	}
	return out
}

// replacementDescriptions is what each of an attribute's RequiresReplace
// modifiers says about when it replaces.
func replacementDescriptions(ctx context.Context, v reflect.Value) []string {
	var out []string
	for _, m := range listField(v, "PlanModifiers") {
		if !IsReplacementModifier(m) {
			continue
		}
		text := ""
		if d, ok := m.(describer); ok {
			text = strings.TrimSpace(d.MarkdownDescription(ctx))
		}
		out = append(out, text)
	}
	return out
}

// defaultValue renders an attribute's schema default. The framework's static
// defaults describe themselves only as "value defaults to ...", so it is
// read by asking the default for its value, the way the framework does when
// it plans.
func defaultValue(ctx context.Context, v reflect.Value) (string, bool) {
	f := v.FieldByName("Default")
	if !f.IsValid() || f.Kind() != reflect.Interface || f.IsNil() {
		return "", false
	}
	return renderDefault(ctx, f.Interface())
}

func defaultSentence(rendered string) string {
	return "Defaults to " + rendered + "."
}

// renderDefault writes a default's value as a reader would type it. A kind of
// default with no case here falls back to the default's own description, so
// it is still stated, only less tidily.
func renderDefault(ctx context.Context, d any) (string, bool) {
	switch d := d.(type) {
	case defaults.String:
		var r defaults.StringResponse
		d.DefaultString(ctx, defaults.StringRequest{}, &r)
		return "`" + r.PlanValue.ValueString() + "`", true
	case defaults.Bool:
		var r defaults.BoolResponse
		d.DefaultBool(ctx, defaults.BoolRequest{}, &r)
		return "`" + strconv.FormatBool(r.PlanValue.ValueBool()) + "`", true
	case defaults.Int64:
		var r defaults.Int64Response
		d.DefaultInt64(ctx, defaults.Int64Request{}, &r)
		return "`" + strconv.FormatInt(r.PlanValue.ValueInt64(), 10) + "`", true
	case defaults.Float64:
		var r defaults.Float64Response
		d.DefaultFloat64(ctx, defaults.Float64Request{}, &r)
		return "`" + strconv.FormatFloat(r.PlanValue.ValueFloat64(), 'g', -1, 64) + "`", true
	case describer:
		return strings.TrimSuffix(strings.TrimSpace(d.MarkdownDescription(ctx)), "."), true
	}
	return "", false
}

// requiresReplaceDescription is how the framework's unconditional
// RequiresReplace describes itself. A RequiresReplaceIf carries the
// description it was built with, which says when it replaces.
const requiresReplaceDescription = "If the value of this attribute changes, Terraform will destroy and recreate the resource."

// factWriter appends the schema's facts to each description on one surface.
type factWriter struct {
	// kind is the surface's kind, as on docSurface.
	kind string
	// guarded is true on a resource carrying allow_replace, where a plan
	// that replaces it fails unless the opt-in is set (replace_guard.go).
	guarded bool
}

// documentResourceSchema appends each attribute's facts to its description,
// and describes the timeouts block. It runs last in every resource's Schema,
// from declareReplacementGuard or describeReplacement, so the opt-in is
// already in place when it looks for it.
func documentResourceSchema(s *schema.Schema) {
	_, guarded := s.Attributes[allowReplaceAttribute]
	describeTimeouts(reflect.ValueOf(s.Blocks))
	w := factWriter{kind: "resource", guarded: guarded}
	w.document(reflect.ValueOf(s.Attributes), true)
	w.document(reflect.ValueOf(s.Blocks), false)
}

// documentDataSourceSchema is documentResourceSchema for a data source. It
// runs last in every data source's Schema.
func documentDataSourceSchema(s *dschema.Schema) {
	w := factWriter{kind: "data source"}
	w.document(reflect.ValueOf(s.Attributes), false)
	w.document(reflect.ValueOf(s.Blocks), false)
}

// documentListSchema is documentResourceSchema for a list resource's config.
func documentListSchema(s *lschema.Schema) {
	w := factWriter{kind: "list resource"}
	w.document(reflect.ValueOf(s.Attributes), false)
	w.document(reflect.ValueOf(s.Blocks), false)
}

// documentEphemeralSchema is documentResourceSchema for an ephemeral resource.
func documentEphemeralSchema(s *eschema.Schema) {
	describeTimeouts(reflect.ValueOf(s.Blocks))
	w := factWriter{kind: "ephemeral resource"}
	w.document(reflect.ValueOf(s.Attributes), false)
	w.document(reflect.ValueOf(s.Blocks), false)
}

// documentActionSchema is documentResourceSchema for an action.
func documentActionSchema(s *aschema.Schema) {
	describeTimeouts(reflect.ValueOf(s.Blocks))
	w := factWriter{kind: "action"}
	w.document(reflect.ValueOf(s.Attributes), false)
	w.document(reflect.ValueOf(s.Blocks), false)
}

// document appends facts to every description in one schema map and in
// everything nested inside it. The entries are struct values held in
// interfaces, so each is copied out, changed and stored back. The maps nested
// inside an entry are shared with the copy, so the recursion changes them in
// place.
func (w factWriter) document(m reflect.Value, scopeTop bool) {
	if !m.IsValid() || m.Kind() != reflect.Map || m.IsNil() {
		return
	}
	ctx := context.Background()
	for _, k := range m.MapKeys() {
		found := concrete(m.MapIndex(k))
		if !found.IsValid() {
			continue
		}
		v := reflect.New(found.Type()).Elem()
		v.Set(found)
		name := k.String()
		appendFacts(v, w.facts(ctx, v, scopeTop && (name == "region" || name == "project")))
		for _, nested := range nestedMaps(v) {
			w.document(nested, false)
		}
		m.SetMapIndex(k, v)
	}
}

// facts are the sentences one attribute's schema declares, in the order a
// reader wants them: what replaces it, what it defaults to, what it accepts,
// and how Terraform treats its value.
func (w factWriter) facts(ctx context.Context, v reflect.Value, scope bool) []string {
	var out []string
	if s := w.replacementSentence(ctx, v, scope); s != "" {
		out = append(out, s)
	}
	if def, ok := defaultValue(ctx, v); ok {
		out = append(out, defaultSentence(def))
	}
	if limits := validatorDescriptions(ctx, v); len(limits) > 0 {
		sentences := make([]string, len(limits))
		for i, l := range limits {
			sentences[i] = asSentence(l)
		}
		out = append(out, "**Constraints:** "+strings.Join(sentences, " "))
	}
	if boolField(v, "Sensitive") {
		if w.kind == "ephemeral resource" {
			out = append(out, "**Sensitive:** Terraform hides it in output, and an ephemeral "+
				"resource's values are never written to plan or state.")
		} else {
			out = append(out, "**Sensitive:** Terraform hides it in plan and apply output, but "+
				"it's still written to state.")
		}
	}
	if boolField(v, "WriteOnly") {
		out = append(out, "**Write-only:** Terraform sends it to the provider and never writes "+
			"it to plan or state.")
	}
	return out
}

// replacementSentence says whether changing the attribute replaces the
// resource, and when, if only sometimes. Region and project replace from
// ModifyPlan, so scope says so for them.
func (w factWriter) replacementSentence(ctx context.Context, v reflect.Value, scope bool) string {
	descs := replacementDescriptions(ctx, v)
	if !scope && len(descs) == 0 {
		return ""
	}
	guard := ""
	if w.guarded {
		guard = " (needs `" + allowReplaceAttribute + " = true`)"
	}
	var when []string
	for _, d := range descs {
		if d == "" || d == requiresReplaceDescription {
			return "**Forces replacement**" + guard + "."
		}
		when = append(when, strings.TrimSuffix(lowerFirst(d), "."))
	}
	if scope {
		return "**Forces replacement**" + guard + "."
	}
	return "**Can force replacement**" + guard + ": " + strings.Join(when, ", and ") + "."
}

// appendFacts adds the sentences to the end of the description, once. A
// schema map shared between calls would otherwise collect them again on
// every call.
func appendFacts(v reflect.Value, facts []string) {
	if len(facts) == 0 {
		return
	}
	field := v.FieldByName("MarkdownDescription")
	if !field.IsValid() || field.String() == "" {
		field = v.FieldByName("Description")
	}
	if !field.IsValid() || !field.CanSet() || strings.TrimSpace(field.String()) == "" {
		// Left empty, so DocumentationGaps reports the missing description
		// rather than a description that is only facts.
		return
	}
	suffix := " " + strings.Join(facts, " ")
	if strings.HasSuffix(field.String(), suffix) {
		return
	}
	field.SetString(strings.TrimRight(field.String(), " ") + suffix)
}

// asSentence capitalises a validator's description and ends it with a full
// stop, so several read as sentences one after another.
func asSentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	if r := []rune(s); unicode.IsLower(r[0]) {
		r[0] = unicode.ToUpper(r[0])
		s = string(r)
	}
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

func lowerFirst(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) > 1 && unicode.IsUpper(r[0]) && !unicode.IsUpper(r[1]) {
		r[0] = unicode.ToLower(r[0])
	}
	return string(r)
}

// timeoutOperations says what each timeout bounds. Every one of them runs the
// whole operation under one deadline (withTimeout), the platform's wait
// included.
var timeoutOperations = map[string]string{
	"create": "Bounds the whole create, including the wait until the platform reports it ready.",
	"read":   "Bounds the whole read.",
	"update": "Bounds the whole update, including the wait until the platform settles.",
	"delete": "Bounds the whole destroy, including the wait until the platform reports it gone.",
	"open":   "Bounds opening it, including the wait while the cluster is creating or updating.",
	"invoke": "Bounds the whole invocation, including the wait until the platform settles.",
}

// describeTimeouts writes the timeouts block's descriptions. The library
// leaves the block undescribed and gives each operation only the duration
// syntax, which says nothing about what happens when it is left out. Here
// that is the provider's operation_timeout, which every withTimeout call
// falls back to.
func describeTimeouts(blocks reflect.Value) {
	if !blocks.IsValid() || blocks.Kind() != reflect.Map || blocks.IsNil() {
		return
	}
	key := reflect.ValueOf("timeouts")
	found := concrete(blocks.MapIndex(key))
	if !found.IsValid() {
		return
	}
	b := reflect.New(found.Type()).Elem()
	b.Set(found)
	attrs := b.FieldByName("Attributes")
	long := false
	if attrs.IsValid() && attrs.Kind() == reflect.Map {
		for _, k := range attrs.MapKeys() {
			if op := concrete(attrs.MapIndex(k)); op.IsValid() {
				d := op.FieldByName("MarkdownDescription")
				long = long || (d.IsValid() && d.String() == longTimeoutMarker)
			}
		}
	}
	if f := b.FieldByName("MarkdownDescription"); f.IsValid() && f.CanSet() {
		which := "Each operation left out defaults"
		if long {
			which = "Left out, `create` and `delete` default to `" + shortDuration(DefaultLongOperationTimeout) +
				"` and every other operation"
		}
		f.SetString("How long each operation may take before it fails. " + which + " to the provider's " +
			"`operation_timeout`, which is `" + shortDuration(DefaultOperationTimeout) +
			"` unless the provider block or `" + EnvVar(envOperationTimeout) + "` sets it.")
	}
	if attrs.IsValid() && attrs.Kind() == reflect.Map {
		for _, k := range attrs.MapKeys() {
			op := concrete(attrs.MapIndex(k))
			if !op.IsValid() {
				continue
			}
			a := reflect.New(op.Type()).Elem()
			a.Set(op)
			if f := a.FieldByName("MarkdownDescription"); f.IsValid() && f.CanSet() {
				sentence := timeoutDefaultSentence()
				if f.String() == longTimeoutMarker {
					sentence = longTimeoutDefaultSentence()
				}
				f.SetString(strings.TrimSpace(timeoutOperations[k.String()] + " " + sentence))
			}
			// The library's own check accepts a negative duration, which
			// withTimeout would quietly replace with the default.
			if f := a.FieldByName("Validators"); f.IsValid() && f.CanSet() &&
				reflect.TypeOf(positiveDuration{}).AssignableTo(f.Type().Elem()) {
				one := reflect.MakeSlice(f.Type(), 0, 1)
				f.Set(reflect.Append(one, reflect.ValueOf(positiveDuration{})))
			}
			attrs.SetMapIndex(k, a)
		}
	}
	blocks.SetMapIndex(key, b)
}

// timeoutDefaultSentence is the default every timeout takes, from the code
// that applies it.
func timeoutDefaultSentence() string {
	return "Left out, it defaults to the provider's `operation_timeout`, which is `" +
		shortDuration(DefaultOperationTimeout) + "` unless the provider block or `" +
		EnvVar(envOperationTimeout) + "` sets it."
}

// longTimeoutMarker is the description hardwareTimeouts gives an operation
// whose default is DefaultLongOperationTimeout. describeTimeouts replaces it.
const longTimeoutMarker = "default: long"

// longTimeoutDefaultSentence is timeoutDefaultSentence for such an operation.
func longTimeoutDefaultSentence() string {
	return "Left out, it defaults to `" + shortDuration(DefaultLongOperationTimeout) +
		"`, or to the provider's `operation_timeout` once the provider block or `" +
		EnvVar(envOperationTimeout) + "` sets it."
}

// hardwareTimeouts is the timeouts block of a resource whose create and
// delete build and free hardware: those two default to
// DefaultLongOperationTimeout, and update to `operation_timeout`.
func hardwareTimeouts(ctx context.Context) schema.Block {
	block := timeouts.Block(ctx, timeouts.Opts{Create: true, Update: true, Delete: true})
	nested, ok := block.(schema.SingleNestedBlock)
	if !ok {
		return block
	}
	for _, op := range []string{"create", "delete"} {
		if a, ok := nested.Attributes[op].(schema.StringAttribute); ok {
			a.MarkdownDescription = longTimeoutMarker
			nested.Attributes[op] = a
		}
	}
	return nested
}

// shortDuration writes 30m rather than Go's 30m0s.
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
