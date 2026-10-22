package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/identityschema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// Resource identity, Terraform 1.12 and later.
//
// An identity is the handful of attributes that name a resource on the
// platform, stored beside state as a typed object. Its point for a tenant is
// the import block: `identity = { id = "...", project = "..." }` says which
// part is which, where the string form packs them into `region/project/id`
// and leaves the reader to count slashes. Terraform before 1.12 ignores it.
//
// Every identity attribute here is also a state attribute of the same name and
// type, and every one of them forces a replacement when it changes. That's
// what makes copying them from state safe: the framework refuses an identity
// that changes under a live resource, and none of these can.

// Each resource's identity, as the import block takes it. The scope is
// optional, because an import that leaves it out uses the provider block's,
// the same as the bare string form.
func scopedIdentitySchema(key, parent string) identityschema.Schema {
	attrs := map[string]identityschema.Attribute{
		"region": identityschema.StringAttribute{
			OptionalForImport: true,
			Description:       "Region the resource lives in. Defaults to the provider's `region` on import.",
		},
		"project": identityschema.StringAttribute{
			OptionalForImport: true,
			Description:       "Project the resource lives in. Defaults to the provider's `project` on import.",
		},
		key: identityschema.StringAttribute{
			RequiredForImport: true,
			Description:       identityKeyDescription(key),
		},
	}
	if parent != "" {
		attrs[parent] = identityschema.StringAttribute{
			RequiredForImport: true,
			Description:       fmt.Sprintf("Id of the %s this resource lives under.", identityNoun(parent)),
		}
	}
	return identityschema.Schema{Attributes: attrs}
}

func identityKeyDescription(key string) string {
	if key == "id" {
		return "The resource's id."
	}
	return fmt.Sprintf("Id of the %s, which is what names this resource.", identityNoun(key))
}

func identityNoun(attr string) string {
	if attr == "vpc" {
		return "VPC"
	}
	return attr
}

// syncIdentity writes the identity from the first of states that holds a
// resource. It's deferred at the top of Create, Read and Update, so every
// path out of them, the early ones included, leaves the two in step.
//
// Read passes its prior state second. A read that finds the resource gone
// removes it from state, and the framework still requires an identity, which
// for a row written before identity existed would otherwise be empty.
//
// A null region or project is the provider's, the same scope resolveScope
// gives every request for that row. A row written before the scope was
// recorded beside the id has both null, and the framework refuses a read that
// returns no identity, so without this every refresh of such a row fails.
//
// A row with any other identity attribute unknown or null is left alone:
// that's a create that failed before the platform named the resource, and an
// identity half filled in would be refused as a change once the rest arrived.
func syncIdentity(ctx context.Context, meta *Meta, identity *tfsdk.ResourceIdentity, diags *diag.Diagnostics, states ...*tfsdk.State) {
	if identity == nil {
		return
	}
	for _, state := range states {
		if state == nil || state.Raw.IsNull() {
			continue
		}
		values := map[string]types.String{}
		for name := range identity.Schema.GetAttributes() {
			var v types.String
			if d := state.GetAttribute(ctx, path.Root(name), &v); d.HasError() || v.IsUnknown() {
				return
			}
			if v.IsNull() {
				v = providerScope(meta, name)
			}
			if v.IsNull() {
				return
			}
			values[name] = v
		}
		for name, v := range values {
			diags.Append(identity.SetAttribute(ctx, path.Root(name), v)...)
		}
		return
	}
}

// providerScope is the provider's region or project, null for any other
// attribute or when the provider has none.
func providerScope(meta *Meta, name string) types.String {
	if meta == nil {
		return types.StringNull()
	}
	switch {
	case name == "region" && meta.Region != "":
		return types.StringValue(meta.Region)
	case name == "project" && meta.Project != "":
		return types.StringValue(meta.Project)
	}
	return types.StringNull()
}

// importForm is how a resource is addressed on import, in either form.
type importForm struct {
	resource string // the full type name, for messages
	key      string // the attribute the last segment names: id, or vpc for a binding
	parent   string // the attribute the segment before it names, for a nested resource
	example  string // a nested import id, for the error that explains the form
}

// importScoped imports a resource addressed by region and project, from a
// string id or from an identity, and writes the scope the way the string
// form always has, so both forms land in the same state.
func importScoped(ctx context.Context, meta *Meta, form importForm, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts, region, project, diags := form.parse(ctx, req)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	for _, name := range []string{form.parent, form.key} {
		if name != "" {
			resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(name), parts[name])...)
		}
	}
	setImportedScope(ctx, meta, region, project, &resp.State, &resp.Diagnostics)
}

func (f importForm) parse(ctx context.Context, req resource.ImportStateRequest) (parts map[string]string, region, project string, diags diag.Diagnostics) {
	if req.ID == "" && req.Identity != nil && !req.Identity.Raw.IsNull() {
		return f.parseIdentity(ctx, req.Identity)
	}
	parts = map[string]string{}
	if f.parent == "" {
		var id string
		id, region, project, diags = importID(req.ID, f.resource)
		parts[f.key] = id
		return parts, region, project, diags
	}
	var parent, id string
	parent, id, region, project, diags = nestedImportID(req.ID, f.resource, f.parent, f.example)
	parts[f.key], parts[f.parent] = id, parent
	return parts, region, project, diags
}

// parseIdentity holds an identity to the rules the string form is held to.
// Its values become segments of a request path, so a slash or a blank is
// refused here rather than sent.
func (f importForm) parseIdentity(ctx context.Context, identity *tfsdk.ResourceIdentity) (parts map[string]string, region, project string, diags diag.Diagnostics) {
	parts = map[string]string{}
	read := func(name string) string {
		var v types.String
		diags.Append(identity.GetAttribute(ctx, path.Root(name), &v)...)
		return v.ValueString()
	}
	region, project = read("region"), read("project")
	for _, name := range []string{f.parent, f.key} {
		if name == "" {
			continue
		}
		v := read(name)
		if strings.TrimSpace(v) == "" || strings.Contains(v, "/") {
			diags.AddAttributeError(path.Root(name), "Incomplete import identity",
				fmt.Sprintf("Importing %s by identity needs `%s`, and %q isn't one: it's a single "+
					"id, with no slash.", f.resource, name, v))
		}
		parts[name] = v
	}
	if region != "" {
		diags.Append(validateRegion(region)...)
	}
	if project != "" {
		diags.Append(validateProject(project)...)
	}
	return parts, region, project, diags
}
