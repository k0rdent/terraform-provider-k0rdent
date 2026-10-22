package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"
)

// The plan-time read behind createGuard. See replace_guard_resource.go.

// hardwareIDAttribute is a cluster's or an instance group's `id`. It's
// required, unlike the generated id other kinds offer, so that a plan to
// create one can read it first: Terraform plans the replacement of a tainted
// object with no prior state, and an id made inside Create would be unknown
// there. The platform can't rename one, so a change replaces it.
func hardwareIDAttribute(name string) schema.StringAttribute {
	return schema.StringAttribute{
		Required: true,
		MarkdownDescription: name + " name, the one the platform shows, and the id this resource is imported " +
			"by. Lower-case letters, digits and hyphens, starting with a letter. Required, so that a plan " +
			"can read it before creating it: a create that stopped waiting leaves it still being built, and " +
			"the plan that would replace it is refused with the way to wait for it instead.",
		PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
		Validators:    idValidators(),
	}
}

// plannedTarget is the id and scope a plan to create names, or false when
// one of them isn't known yet and there is nothing to read.
func plannedTarget(ctx context.Context, meta *Meta, plan tfsdk.Plan) (clusterTarget, string, bool) {
	var id, region, project types.String
	if plan.GetAttribute(ctx, path.Root("id"), &id).HasError() ||
		plan.GetAttribute(ctx, path.Root("region"), &region).HasError() ||
		plan.GetAttribute(ctx, path.Root("project"), &project).HasError() {
		return clusterTarget{}, "", false
	}
	if id.IsNull() || id.IsUnknown() || id.ValueString() == "" || region.IsUnknown() || project.IsUnknown() {
		return clusterTarget{}, "", false
	}
	var ignored diag.Diagnostics
	target, ok := resolveScope(meta, region, project, &ignored)
	return target, id.ValueString(), ok
}

// existing is what the plan-time read found under the configured id.
type existing struct {
	kind, resourceName string
	target             clusterTarget
	id, state          string
	// tagged is false for a kind with no tags to carry a create token.
	tagged, token bool
}

// refuse fails the plan for an object the platform already holds under the
// configured id, and says how to keep it, replace it, or leave it alone.
func (e existing) refuse(diags *diag.Diagnostics) {
	summary := fmt.Sprintf("A %s named %q already exists", e.kind, e.id)
	if e.state == creatingState {
		summary = fmt.Sprintf("The %s %q is still being created", e.kind, e.id)
	}
	origin := fmt.Sprintf("A %s has no tags, so the provider can't tell which configuration made it.", e.kind)
	switch {
	case e.tagged && e.token:
		origin = "It carries this provider's create token, so a Terraform apply made it, most likely this " +
			"configuration's."
	case e.tagged:
		origin = "It carries no create token from this provider, so it wasn't made by a Terraform apply."
	}
	diags.AddAttributeError(path.Root("id"), summary, redactDiagnosticText(fmt.Sprintf(
		"The platform reports %s %q as `%s`. This plan would create it, which means either a second "+
			"%s under an id that's taken, or, when this resource is tainted in state, destroying this "+
			"one first and creating it again. A destroy cancels whatever the platform is still building. "+
			"%s\n\n"+
			"If an earlier apply stopped waiting for it, wait for it instead, replacing <address> with "+
			"this resource's address, such as %s.example:\n\n"+
			"    terraform untaint <address>\n"+
			"    terraform apply\n\n"+
			"To replace it on purpose, set `allow_replace = true` on this resource. If this state doesn't "+
			"manage it, import it with `terraform import <address> %s`, or choose another `id`. "+
			"Nothing was sent: the plan was refused.",
		e.kind, e.id, e.state, e.kind, origin, e.resourceName, scopedImportID(e.target)(e.id))))
}

// warnReplacingFailed says a plan to create passed over a `failed` object,
// because applying it deletes that object first when it's tainted, and that
// delete can fail too. The plan a destroy refreshes with shows it as well,
// which the warning says, since the provider can't tell the two apart.
func (e existing) warnReplacingFailed(diags *diag.Diagnostics) {
	diags.AddAttributeWarning(path.Root("id"),
		fmt.Sprintf("The %s %q failed, and applying this plan replaces it", e.kind, e.id),
		fmt.Sprintf("The platform reports %s %q as `failed`. A failed create leaves it tainted in state, so "+
			"applying this plan deletes it and creates it again. Its delete can fail the way its create did. "+
			"To only remove it, run `terraform destroy` instead.\n\n"+
			"`terraform destroy` shows this warning too, from the plan it runs first to refresh state. "+
			"It replaces nothing.", e.kind, e.id))
}

// refuseExisting is createGuard for a cluster. A `failed` one this provider
// made passes with a warning: nothing is being built, so its replacement only
// destroys what the platform gave up on, and a destroy's refresh can run past
// it.
func (r *clusterResource) refuseExisting(ctx context.Context, plan tfsdk.Plan, diags *diag.Diagnostics) {
	target, id, ok := plannedTarget(ctx, r.meta, plan)
	if !ok || r.meta == nil || r.meta.Client == nil {
		return
	}
	got, err := r.meta.Client.GetClusterWithResponse(ctx, target.region, target.project, id, nil)
	if err != nil || got.JSON200 == nil {
		// A 404 is a create like any other. A read that failed leaves it to
		// the create, which reads the id back on a 409.
		if err != nil {
			tflog.Debug(ctx, "the plan-time read of the cluster failed", map[string]any{"error": redactDiagnosticText(err.Error())})
		}
		return
	}
	state := stateString(got.JSON200.State)
	token := createTokenOf(presentMap(got.JSON200.Tags)) != ""
	if state == "deleting" || state == "deleted" {
		return
	}
	found := existing{
		kind: "cluster", resourceName: ResourceName(clusterResourceSuffix), target: target, id: id, state: state,
		tagged: true, token: token,
	}
	if state == "failed" && token {
		found.warnReplacingFailed(diags)
		return
	}
	found.refuse(diags)
}

// refuseExisting is createGuard for an instance group.
func (r *instanceGroupResource) refuseExisting(ctx context.Context, plan tfsdk.Plan, diags *diag.Diagnostics) {
	target, id, ok := plannedTarget(ctx, r.meta, plan)
	if !ok || r.meta == nil || r.meta.Client == nil {
		return
	}
	got, err := r.meta.Client.GetInstanceGroupWithResponse(ctx, target.region, target.project, id, nil)
	if err != nil || got.JSON200 == nil {
		if err != nil {
			tflog.Debug(ctx, "the plan-time read of the instance group failed", map[string]any{"error": redactDiagnosticText(err.Error())})
		}
		return
	}
	// An instance group has no tags to carry a create token, so a `failed`
	// one passes on its state alone. Replacing it only happens when it's
	// tainted in this state, and a plan for a new one is answered 409.
	state := stateString(got.JSON200.State)
	if state == "deleting" || state == "deleted" {
		return
	}
	found := existing{
		kind: "instance group", resourceName: ResourceName(instanceGroupResourceSuffix), target: target, id: id,
		state: state,
	}
	if state == "failed" {
		found.warnReplacingFailed(diags)
		return
	}
	found.refuse(diags)
}
