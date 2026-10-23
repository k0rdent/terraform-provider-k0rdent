package provider

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/list"
	listschema "github.com/hashicorp/terraform-plugin-framework/list/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// List resources, Terraform 1.14 and later.
//
// `terraform query` runs the `list` blocks in a `.tfquery.hcl` file and shows
// what each one finds. With `-generate-config-out` it also writes an `import`
// block and a `resource` block for every object found. For a tenant adopting a
// project built in the console, that's the whole estate as reviewable
// configuration in one command, where the import guide used to ask for a
// hand-written block per object.
//
// Each list resource finds objects with the same paged list calls the plural
// data sources use, and names each one by its managed resource's identity, so
// the import block it leads to is the identity form. Terraform before 1.14
// never asks for one.
//
// The full object, which `include_resource` asks for and which
// `-generate-config-out` always asks for, is the state an import of that
// identity would land: the managed resource's own ImportState, then its own
// Read. That costs a read per object on top of the list, and it's what keeps
// the generated configuration honest. The import acceptance tests already
// hold every resource's generated configuration to validating and planning
// with no change, and this goes through exactly the code they test, rather
// than a second mapping from a list row that could drift from Read.

// listedObject is one object a list found.
type listedObject struct {
	// identity is the managed resource's identity, every attribute set.
	identity map[string]string
	// display is the name `terraform query` shows for it.
	display string
}

// listSpec is everything that differs between one list resource and another.
type listSpec struct {
	// suffix is the managed resource's, since a list resource shares its name.
	suffix string
	// newResource makes the managed resource, for include_resource.
	newResource func() resource.Resource
	// description is the page's opening, after the generic part.
	description string
	// attributes are the filters a `config` block takes.
	attributes map[string]listschema.Attribute
	// find reads every object the filters match. Diagnostics with an error
	// mean nothing is returned. A warning names something left out.
	find func(ctx context.Context, meta *Meta, config listConfig, diags *diag.Diagnostics) []listedObject
	// validate checks a configuration before anything is sent. Optional.
	validate func(config listConfig, diags *diag.Diagnostics)
}

// listResources is every list resource, in the order Resources registers
// their managed resources.
func listResources() []func() list.ListResource {
	specs := listSpecs()
	out := make([]func() list.ListResource, 0, len(specs))
	for _, spec := range specs {
		out = append(out, func() list.ListResource { return &listResource{spec: spec} })
	}
	return out
}

// listResource is one list resource. Every one of them is this type with a
// different spec, so the rules every list has to follow (the limit, the
// order, what a vanished object does) have one implementation.
type listResource struct {
	spec listSpec
	meta *Meta
}

var (
	_ list.ListResourceWithConfigure      = (*listResource)(nil)
	_ list.ListResourceWithValidateConfig = (*listResource)(nil)
)

func (l *listResource) Metadata(_ context.Context, _ resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = ResourceName(l.spec.suffix)
}

func (l *listResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	l.meta = configureMeta(req.ProviderData, &resp.Diagnostics)
}

func (l *listResource) ListResourceConfigSchema(_ context.Context, _ list.ListResourceSchemaRequest, resp *list.ListResourceSchemaResponse) {
	attrs := make(map[string]listschema.Attribute, len(l.spec.attributes))
	for name, a := range l.spec.attributes {
		attrs[name] = a
	}
	resp.Schema = listschema.Schema{
		MarkdownDescription: fmt.Sprintf("Finds every `%s` a `terraform query` can adopt, by its "+
			"identity. With `include_resource = true`, or under `-generate-config-out`, each result also "+
			"carries the object as an import of that identity would read it, so the generated "+
			"configuration is what an import plans against. Terraform 1.14 and later.\n\n",
			ResourceName(l.spec.suffix)) + l.spec.description,
		Attributes: attrs,
	}
	documentListSchema(&resp.Schema)
}

func (l *listResource) ValidateListResourceConfig(_ context.Context, req list.ValidateConfigRequest, resp *list.ValidateConfigResponse) {
	if l.spec.validate != nil {
		l.spec.validate(listConfig{config: req.Config}, &resp.Diagnostics)
	}
}

func (l *listResource) List(ctx context.Context, req list.ListRequest, stream *list.ListResultsStream) {
	var diags diag.Diagnostics
	if !providerConfigured(l.meta, &diags) {
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}
	config := listConfig{config: req.Config}
	if l.spec.validate != nil {
		l.spec.validate(config, &diags)
	}
	var found []listedObject
	if !diags.HasError() {
		found = l.spec.find(ctx, l.meta, config, &diags)
	}
	if diags.HasError() {
		stream.Results = list.ListResultsStreamDiagnostics(diags)
		return
	}
	sortListed(found)

	stream.Results = func(push func(list.ListResult) bool) {
		// A warning from the list, such as an object of a kind this build
		// can't read, goes first and on its own: the framework passes a
		// result with no identity through when it carries only diagnostics.
		if len(diags) > 0 && !push(list.ListResult{Diagnostics: diags}) {
			return
		}
		// The limit counts results, not objects found: one dropped below
		// doesn't take a place. Counting here rather than cutting the slice
		// first is also what stops the reads at the limit.
		sent := int64(0)
		for i, obj := range found {
			if req.Limit > 0 && sent == req.Limit {
				push(list.ListResult{Diagnostics: limitReached(req.Limit, len(found)-i)})
				return
			}
			result, outcome := l.result(ctx, req, obj)
			if outcome == resultGone {
				continue
			}
			if !push(result) || result.Diagnostics.HasError() {
				return
			}
			if outcome == resultFound {
				sent++
			}
		}
	}
}

// limitReached is the warning for a list cut short by its block's `limit`.
func limitReached(limit int64, left int) diag.Diagnostics {
	var diags diag.Diagnostics
	diags.AddWarning(fmt.Sprintf("Stopped at the limit of %d", limit),
		fmt.Sprintf("%d more found objects weren't returned. Raise `limit` in the list block, or narrow "+
			"its `config`, to see them.", left))
	return diags
}

// resultOutcome is what became of one object found.
type resultOutcome int

const (
	// resultFound is a result to show.
	resultFound resultOutcome = iota
	// resultGone is an object deleted between the list and its read. An
	// import of it would fail, so it's left out without a word.
	resultGone
	// resultUnread is an object whose read failed. The result carries only
	// a warning naming it, and the rest of the list goes on.
	resultUnread
)

// result builds one object's result.
func (l *listResource) result(ctx context.Context, req list.ListRequest, obj listedObject) (list.ListResult, resultOutcome) {
	result := req.NewListResult(ctx)
	result.DisplayName = obj.display
	for _, name := range sortedKeys(obj.identity) {
		result.Diagnostics.Append(result.Identity.SetAttribute(ctx, path.Root(name), obj.identity[name])...)
	}
	if !req.IncludeResource || result.Diagnostics.HasError() {
		return result, resultFound
	}
	state, identity, diags := l.importAndRead(ctx, req, result.Identity)
	if diags.HasError() {
		// One object the credential can't read, or the platform fails on,
		// shouldn't hide the rest of a project. It's named, with why.
		var unread diag.Diagnostics
		var why []string
		for _, d := range diags.Errors() {
			why = append(why, d.Summary()+": "+d.Detail())
		}
		unread.AddWarning(fmt.Sprintf("%s was left out", obj.display),
			fmt.Sprintf("Reading %s %s failed, so it's not in the results:\n\n%s",
				ResourceName(l.spec.suffix), obj.display, strings.Join(why, "\n\n")))
		return list.ListResult{Diagnostics: unread}, resultUnread
	}
	result.Diagnostics.Append(diags...)
	if state.Raw.IsNull() {
		return result, resultGone
	}
	result.Resource = &tfsdk.Resource{Raw: state.Raw, Schema: state.Schema}
	if identity != nil && !identity.Raw.IsNull() {
		result.Identity = identity
	}
	return result, resultFound
}

// importAndRead is what Terraform does for an `import` block naming this
// identity: the resource's ImportState, then its Read on what that wrote.
//
// Private state is left nil on both. The framework hands an import an empty
// store, no ImportState here writes to it, and every Read that touches it
// checks for nil first. A result carries no private state anyway: Terraform's
// own import and read after it build that.
func (l *listResource) importAndRead(ctx context.Context, req list.ListRequest, identity *tfsdk.ResourceIdentity) (tfsdk.State, *tfsdk.ResourceIdentity, diag.Diagnostics) {
	var diags diag.Diagnostics
	r := l.spec.newResource()
	if c, ok := r.(resource.ResourceWithConfigure); ok {
		var resp resource.ConfigureResponse
		c.Configure(ctx, resource.ConfigureRequest{ProviderData: l.meta}, &resp)
		diags.Append(resp.Diagnostics...)
	}
	importer, ok := r.(resource.ResourceWithImportState)
	if !ok {
		// Local: a list resource registered for a resource with no import.
		// listSpecs is held to that by a test, so this is a broken build.
		diags.AddError("This resource has no import",
			fmt.Sprintf("%s can't be read for a list result. This is always a bug in the provider.", ResourceName(l.spec.suffix)))
	}
	if diags.HasError() {
		return tfsdk.State{}, nil, diags
	}

	empty := tfsdk.State{Raw: tftypes.NewValue(req.ResourceSchema.Type().TerraformType(ctx), nil), Schema: req.ResourceSchema}
	imported := resource.ImportStateResponse{State: empty, Identity: copyIdentity(identity)}
	importer.ImportState(ctx, resource.ImportStateRequest{Identity: copyIdentity(identity)}, &imported)
	// Only the errors. The one warning an import gives is to repeat a scope
	// the provider block doesn't name in the resource block, and a result
	// carries its scope in its identity and in the object, which is where
	// the generated configuration writes it from.
	diags.Append(imported.Diagnostics.Errors()...)
	if diags.HasError() {
		return tfsdk.State{}, nil, diags
	}

	read := resource.ReadResponse{
		State:    tfsdk.State{Raw: imported.State.Raw.Copy(), Schema: imported.State.Schema},
		Identity: copyIdentity(imported.Identity),
	}
	r.Read(ctx, resource.ReadRequest{
		State:    imported.State,
		Identity: copyIdentity(imported.Identity),
	}, &read)
	diags.Append(read.Diagnostics...)
	return read.State, read.Identity, diags
}

func copyIdentity(identity *tfsdk.ResourceIdentity) *tfsdk.ResourceIdentity {
	if identity == nil {
		return nil
	}
	return &tfsdk.ResourceIdentity{Raw: identity.Raw.Copy(), Schema: identity.Schema}
}

// sortListed puts the results in a fixed order: by display name, then by
// identity. The list operations promise no order, and `terraform query`
// output and the generated configuration read better when a second run
// matches the first.
func sortListed(found []listedObject) {
	key := func(o listedObject) string {
		parts := make([]string, 0, len(o.identity))
		for _, name := range sortedKeys(o.identity) {
			parts = append(parts, o.identity[name])
		}
		return strings.Join(parts, "/")
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].display != found[j].display {
			return found[i].display < found[j].display
		}
		return key(found[i]) < key(found[j])
	})
}

// displayName is how a result is shown: the object's display name with its id
// beside it, or the id alone when there's no display name or it's the id.
func displayName(display *string, id string) string {
	// Both halves are platform text on their way to the terminal, and the
	// result is one line, so newline and tab are escaped too.
	d, id := oneLine(strings.TrimSpace(deref(display))), oneLine(id)
	if d == "" || d == id {
		return id
	}
	return fmt.Sprintf("%s (%s)", d, id)
}

// listConfig reads a list block's `config`.
type listConfig struct{ config tfsdk.Config }

// str is a string filter, null when it's absent or the block has no config.
func (c listConfig) str(name string, diags *diag.Diagnostics) types.String {
	var v types.String
	if c.config.Raw.IsNull() {
		return types.StringNull()
	}
	diags.Append(c.config.GetAttribute(context.Background(), path.Root(name), &v)...)
	return v
}

// flag is a boolean filter, false when it's absent.
func (c listConfig) flag(name string, diags *diag.Diagnostics) bool {
	var v types.Bool
	if c.config.Raw.IsNull() {
		return false
	}
	diags.Append(c.config.GetAttribute(context.Background(), path.Root(name), &v)...)
	return v.ValueBool()
}

// known is a string filter's value when it's set and known.
func known(v types.String) (string, bool) {
	if v.IsNull() || v.IsUnknown() {
		return "", false
	}
	return v.ValueString(), true
}

// Filter attributes. A list block's `config` is written by hand, so each one
// says what leaving it out means.

func listRegionAttribute() listschema.StringAttribute {
	return listschema.StringAttribute{
		Optional:            true,
		MarkdownDescription: "Region to search. Defaults to the provider's `region`.",
		Validators:          []validator.String{urlSafeIdentifier{}},
	}
}

func listProjectAttribute() listschema.StringAttribute {
	return listschema.StringAttribute{
		Optional:            true,
		MarkdownDescription: "Project to search. Defaults to the provider's `project`.",
		Validators:          []validator.String{urlSafeIdentifier{}},
	}
}

func listPlatformOwnedAttribute(what, marker string) listschema.BoolAttribute {
	return listschema.BoolAttribute{
		Optional: true,
		MarkdownDescription: fmt.Sprintf("Also find the %s the platform made for itself, the ones with "+
			"`%s` set. Left out or `false`, they're skipped: a managed resource refuses to change one, "+
			"so adopting it adds configuration that can never be applied.", what, marker),
	}
}

// scopedAttributes are the filters every project-scoped list takes.
func scopedAttributes(extra map[string]listschema.Attribute) map[string]listschema.Attribute {
	attrs := map[string]listschema.Attribute{
		"region":  listRegionAttribute(),
		"project": listProjectAttribute(),
	}
	for name, a := range extra {
		attrs[name] = a
	}
	return attrs
}

// listScope resolves a list's region and project the way a data source
// does: the block's own, then the provider's.
func listScope(meta *Meta, config listConfig, diags *diag.Diagnostics) (clusterTarget, bool) {
	region, project := config.str("region", diags), config.str("project", diags)
	if diags.HasError() {
		return clusterTarget{}, false
	}
	return resolveScope(meta, region, project, diags)
}

// scopedIdentity is the identity of an object named by region, project and id,
// plus any parent ids.
func scopedIdentity(target clusterTarget, key, id string, parents ...string) map[string]string {
	out := map[string]string{"region": target.region, "project": target.project, key: id}
	for i := 0; i+1 < len(parents); i += 2 {
		out[parents[i]] = parents[i+1]
	}
	return out
}

// goingAway is a state that says the object is being deleted or already is.
// It's left out of a list: adopting it plans against an object about to be
// gone, and the apply after that would create it again.
func goingAway(state string) bool {
	return state == "deleting" || state == "deleted"
}
