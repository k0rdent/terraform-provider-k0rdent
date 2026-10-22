package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// Region and project address the resource, so they belong on it. Read only from
// the provider block, a changed block silently retargets everything in state.
//
// Three things this gets wrong if done the obvious way:
//   - Static RequiresReplace fires when prior state has no value, e.g. under
//     -refresh=false. So replacement is decided here and a null is ignored.
//   - Optional+Computed without writing the resolved value back produces no plan
//     at all, as google_storage_bucket does on project.
//   - Scope in the id string leaks into HCL. Scaleway shipped two functions to
//     undo that. Keep it in its own attributes.
//
// No organization attribute: the gateway takes it from the token.

// Optional+Computed because it falls back to the provider block and must be
// known after apply. The modifier lives in ModifyPlan, not here: the schema is
// built before Configure runs, so an attribute modifier cannot reach Meta.
func regionAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: "Region this resource lives in. Defaults to the provider's `region`. " +
			"It's part of the resource's API path, so a resource can't move between regions.",
		Validators: []validator.String{urlSafeIdentifier{}},
	}
}

// See regionAttribute.
func projectAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: "Project this resource belongs to. Defaults to the provider's " +
			"`project`. It's part of the resource's API path, so a resource can't move between " +
			"projects.",
		Validators: []validator.String{urlSafeIdentifier{}},
	}
}

type scopeOutcome struct {
	Value types.String
	// Write is false when the attribute was left alone: a destroy plan, or
	// nothing to default from.
	Write   bool
	Replace bool
}

// resolveScopeValue decides one attribute, with no framework plumbing so the
// cases stay a table.
func resolveScopeValue(config, state types.String, deflt string, planIsNull, stateIsNull bool) scopeOutcome {
	if planIsNull {
		return scopeOutcome{}
	}

	// An unknown is a value the configuration has and has not resolved yet, not
	// a value it lacks. Defaulting it plans against a guess: on a create the
	// real value arrives at apply and Terraform reports an inconsistent final
	// plan, and on an update the comparison below would replace a live resource
	// over a value nobody wrote.
	if config.IsUnknown() {
		return scopeOutcome{}
	}

	resolved := config
	if resolved.IsNull() {
		if deflt == "" {
			// Let RequireRegion/RequireProject report it. They can name the
			// resource and both places the value can be set.
			return scopeOutcome{}
		}
		resolved = types.StringValue(deflt)
	}

	// Writing this back is what makes a changed provider default show up.
	out := scopeOutcome{Value: resolved, Write: true}

	if stateIsNull {
		return out
	}

	// No prior value: older state, or -refresh=false. Replacing here would
	// destroy live infrastructure over a bookkeeping gap.
	if state.IsNull() || state.IsUnknown() {
		return out
	}

	out.Replace = !resolved.Equal(state)
	return out
}

// setImportedScope writes the scope an imported resource belongs to, falling
// back to the provider's configuration when the bare-id import form was used.
//
// Every resource resolves its API path from state first, then from the provider
// block, so a resource with no scope in state follows whatever the provider is
// configured for at the time. Ids are caller-chosen slugs, so a same-named
// resource in another scope is a real collision. Recording the scope at import
// is also what lets modifyScopePlan see a later move: it declines to replace on
// a null prior value.
func setImportedScope(ctx context.Context, meta *Meta, region, project string, state *tfsdk.State, diags *diag.Diagnostics) {
	if meta != nil {
		warnScopeNeedsRepeating(diags, region, project, meta)
		if region == "" {
			region = meta.Region
		}
		if project == "" {
			project = meta.Project
		}
	}
	if region != "" {
		diags.Append(state.SetAttribute(ctx, path.Root("region"), region)...)
	}
	if project != "" {
		diags.Append(state.SetAttribute(ctx, path.Root("project"), project)...)
	}
}

// setCreatedScope writes the scope a create used beside its id, before the
// wait. A failed wait leaves a tainted row, and without the scope the next
// refresh reads the provider's project and drops a resource that still exists.
func setCreatedScope(ctx context.Context, target clusterTarget, state *tfsdk.State, diags *diag.Diagnostics) {
	diags.Append(state.SetAttribute(ctx, path.Root("region"), target.region)...)
	diags.Append(state.SetAttribute(ctx, path.Root("project"), target.project)...)
}

// warnScopeNeedsRepeating fires when an import names a scope the provider block
// is not configured for.
//
// State alone does not hold the resource there. The next plan resolves the
// attribute from configuration, finds nothing, falls back to the provider block
// and proposes a replacement. Repeating the scope in the resource block is what
// makes the two agree. An import sees no configuration, so it can't tell a
// block that already sets the scope, as `project = local.project`, from one
// that doesn't. The warning says so, and warnScopeMoves fires at plan when it
// turns out the block doesn't.
func warnScopeNeedsRepeating(diags *diag.Diagnostics, region, project string, meta *Meta) {
	var repeat []string
	if region != "" && region != meta.Region {
		repeat = append(repeat, fmt.Sprintf("region  = %q", region))
	}
	if project != "" && project != meta.Project {
		repeat = append(repeat, fmt.Sprintf("project = %q", project))
	}
	if len(repeat) == 0 {
		return
	}
	diags.AddWarning(
		"Repeat the imported scope in the resource block",
		fmt.Sprintf("This resource was imported into a scope the provider is not configured for. "+
			"Unless its configuration already sets it, add:\n\n    %s\n\n"+
			"Without that, the next plan resolves the scope from the provider block and proposes "+
			"destroying and recreating the resource.", strings.Join(repeat, "\n    ")),
	)
}

// warnScopeMoves fires when a plan moves a resource only because its
// configuration leaves the scope to the provider block.
func warnScopeMoves(diags *diag.Diagnostics, attr, stored, resolved string) {
	diags.AddWarning(
		"Repeat the scope in the resource block",
		fmt.Sprintf("This resource is in %s %q, but its configuration doesn't set %s, so the "+
			"provider block's %q applies and the plan destroys and recreates it there. "+
			"To keep it where it is, add to its configuration:\n\n    %s = %q",
			attr, stored, attr, resolved, attr, stored),
	)
}

// modifyScopePlan resolves region and project in the plan and flags replacement
// when either moves. No-op on a destroy.
func modifyScopePlan(ctx context.Context, meta *Meta, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() || meta == nil {
		return
	}

	for _, attr := range []struct {
		name  string
		deflt string
	}{
		{"region", meta.Region},
		{"project", meta.Project},
	} {
		p := path.Root(attr.name)

		var config, state types.String
		if d := req.Config.GetAttribute(ctx, p, &config); d.HasError() {
			continue // not an attribute on this resource
		}
		if !req.State.Raw.IsNull() {
			resp.Diagnostics.Append(req.State.GetAttribute(ctx, p, &state)...)
			if resp.Diagnostics.HasError() {
				return
			}
		}

		out := resolveScopeValue(config, state, attr.deflt, false, req.State.Raw.IsNull())
		if !out.Write {
			continue
		}
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, p, out.Value)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if out.Replace {
			resp.RequiresReplace = append(resp.RequiresReplace, p)
			if config.IsNull() {
				warnScopeMoves(&resp.Diagnostics, attr.name, state.ValueString(), out.Value.ValueString())
			}
		}
	}

	// A create, or a move, in a region its project isn't in is refused here
	// when the project list says where the project is. See region_mismatch.go.
	if req.State.Raw.IsNull() || len(resp.RequiresReplace) > 0 {
		var region, project types.String
		if resp.Plan.GetAttribute(ctx, path.Root("region"), &region).HasError() ||
			resp.Plan.GetAttribute(ctx, path.Root("project"), &project).HasError() {
			return
		}
		if !region.IsUnknown() && !project.IsUnknown() {
			checkRegionMatchesProject(ctx, meta, region.ValueString(), project.ValueString(), &resp.Diagnostics)
		}
	}
}

// clusterTarget is the region and project pair every project-scoped request
// needs.
type clusterTarget struct {
	region  k0rdent.RegionPathParam
	project k0rdent.ProjectPathParam
}

// resolveScope picks the region and project a request goes to. Resources and
// data sources both come through here, so the two cannot drift apart.
//
// A value on the block itself wins over the provider's: from state or plan on
// a resource, from the optional override on a data source. Both results are
// validated because configuration, state and the import id all arrive here
// without passing the provider's validators, and both become segments of the
// request URL. Every missing or bad half is reported at once, so fixing one
// does not uncover the other on the next run.
func resolveScope(meta *Meta, region, project types.String, diags *diag.Diagnostics) (clusterTarget, bool) {
	if !providerConfigured(meta, diags) {
		return clusterTarget{}, false
	}
	t := clusterTarget{
		region:  ownOrProvider(region, meta.RequireRegion, diags),
		project: ownOrProvider(project, meta.RequireProject, diags),
	}
	diags.Append(validateRegion(t.region)...)
	diags.Append(validateProject(t.project)...)
	if diags.HasError() {
		return clusterTarget{}, false
	}
	return t, true
}

// resolveRegion is resolveScope for the region-wide catalogs, which are not
// project-scoped and must still read on a provider with no project.
func resolveRegion(meta *Meta, region types.String, diags *diag.Diagnostics) (string, bool) {
	if !providerConfigured(meta, diags) {
		return "", false
	}
	r := ownOrProvider(region, meta.RequireRegion, diags)
	diags.Append(validateRegion(r)...)
	if diags.HasError() {
		return "", false
	}
	return r, true
}

// ownOrProvider takes the block's own value, or the provider's when the block
// has none. Null, unknown and empty all count as none: a data source left the
// override out, a plan has not resolved it yet, or state predates the
// attribute.
func ownOrProvider(own types.String, provider func() (string, diag.Diagnostics), diags *diag.Diagnostics) string {
	if v := own.ValueString(); v != "" {
		return v
	}
	v, d := provider()
	diags.Append(d...)
	return v
}
