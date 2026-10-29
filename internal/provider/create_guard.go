package provider

import (
	"context"
	"fmt"
	"strings"

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
			"the plan that would replace it warns so, with the way to wait for it instead.",
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
	tagged bool
	// busyNote follows what applying does to one still being built, when
	// the kind can be told to wait for it instead.
	busyNote string
}

// refuse fails the plan for an object the platform already holds under the
// configured id that carries no create token from this provider, and says how
// to keep it, replace it, or leave it alone. No token doesn't mean no
// Terraform apply made it: an imported cluster has none, and neither has one
// whose tags an older provider updated, so the advice covers this state
// holding it too.
func (e existing) refuse(diags *diag.Diagnostics) {
	summary := fmt.Sprintf("%s named %q already exists", aKind(e.kind), e.id)
	if verb, busy := inFlight[e.state]; busy {
		summary = fmt.Sprintf("The %s %q is still being %s", e.kind, e.id, verb)
	}
	diags.AddAttributeError(path.Root("id"), summary, redactDiagnosticText(fmt.Sprintf(
		"The platform reports %s %q as `%s`, and it carries no create token from this provider. This plan "+
			"would create it, so the provider can't tell this state's tainted copy of it from a second %s "+
			"under an id that's taken.\n\n"+
			"If this state holds it, as an import or a failed apply leaves it, keep it, replacing <address> "+
			"with this resource's address, such as %s.example:\n\n"+
			"    terraform untaint <address>\n"+
			"    terraform apply\n\n"+
			"If this state should manage it and doesn't yet, import it with `terraform import <address> %s`. "+
			"Otherwise choose another `id`. To replace it on purpose, set `allow_replace = true` on this "+
			"resource. Nothing was sent: the plan was refused.\n\n"+
			"`terraform destroy` runs this plan first to refresh state, so it's refused too while this state "+
			"holds the %s tainted. To destroy it, untaint it first:\n\n"+
			"    terraform untaint <address>\n"+
			"    terraform destroy",
		e.kind, e.id, e.state, e.kind, e.resourceName, scopedImportID(e.target)(e.id), e.kind)))
}

// warnReplacing passes a plan to create over an object the provider can
// take for this configuration's, and says what applying it does. A tainted
// object plans this way, and so does the plan a destroy refreshes with, which
// the provider can't tell apart: refusing it would block the destroy. So the
// plan passes in every state. Nor can the provider tell whether this state
// holds the object at all: a second configuration that names the same id
// plans the same create. So the warning says what each case does, and how to
// keep the object instead. It's in front of the approval, as the cancel it
// names would be.
func (e existing) warnReplacing(diags *diag.Diagnostics) {
	notHeld := "If this state doesn't hold it, such as when another configuration or workspace manages it, " +
		"applying replaces nothing: the create fails with \"already exists\" and nothing is deleted. Choose " +
		"another `id` for this one."
	destroyToo := "\n\n`terraform destroy` shows this warning too, from the plan it runs first to refresh state. " +
		"It replaces nothing."
	origin := aKind(e.kind) + " has no tags to carry a create token, so it passes on its state alone."
	if e.tagged {
		origin = "It carries this provider's create token, so a Terraform apply made it."
	}
	if e.state == "failed" {
		diags.AddAttributeWarning(path.Root("id"),
			fmt.Sprintf("The %s %q failed: applying this plan replaces it if this state holds it", e.kind, e.id),
			redactDiagnosticText(fmt.Sprintf("The platform reports %s %q as `failed`. %s If this state holds it, "+
				"a failed create leaves it tainted, so applying this plan deletes it and creates it again. Its "+
				"delete can fail the way its create did. To only remove it, run `terraform destroy` instead.\n\n%s",
				e.kind, e.id, origin, notHeld)+destroyToo))
		return
	}
	keep := fmt.Sprintf("To keep it and wait for it instead, replacing <address> with this resource's address, "+
		"such as %s.example:\n\n"+
		"    terraform untaint <address>\n"+
		"    terraform apply", e.resourceName)
	summary := fmt.Sprintf("The %s %q exists: applying this plan replaces it if this state holds it", e.kind, e.id)
	does := "applying this plan deletes it and creates it again. An earlier apply may have stopped waiting " +
		"for it before it finished."
	if verb, busy := inFlight[e.state]; busy {
		summary = fmt.Sprintf("The %s %q is still being %s: applying this plan cancels it if this state holds it",
			e.kind, e.id, verb)
		does = fmt.Sprintf("applying this plan stops what the platform is still doing, deletes the %s, and "+
			"creates it again.%s", e.kind, e.busyNote)
	}
	diags.AddAttributeWarning(path.Root("id"), summary, redactDiagnosticText(fmt.Sprintf(
		"The platform reports %s %q as `%s`. %s If this state holds it tainted, as a create that stopped "+
			"waiting leaves it, %s\n\n%s\n\n%s", e.kind, e.id, e.state, origin, does, keep, notHeld)+destroyToo))
}

// aKind is kind with its indefinite article, capitalized to start a sentence:
// "A cluster", "An instance group".
func aKind(kind string) string {
	if kind != "" && strings.ContainsRune("aeiou", rune(kind[0])) {
		return "An " + kind
	}
	return "A " + kind
}

// inFlight are the states an object is still being built or changed in, by
// the word a sentence uses for each.
var inFlight = map[string]string{creatingState: "created", "updating": "updated"}

// refuseExisting is createGuard for a cluster. One that carries this
// provider's create token passes with a warning, in any state: a Terraform
// apply made it, and when it's tainted, the plan to replace it is also the
// plan a destroy refreshes with. One without a token was made some other way,
// so the plan is refused: it isn't this configuration's to replace.
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
		tagged: true,
		busyNote: " With `on_destroy_busy = \"wait\"`, it waits for the operation to finish instead of " +
			"cancelling it.",
	}
	if token {
		found.warnReplacing(diags)
		return
	}
	found.refuse(diags)
}

// refuseExisting is createGuard for an instance group. An instance group has
// no tags to carry a create token, so it passes on its state alone, with a
// warning: refusing one would block the destroy of a tainted group. A group
// this state doesn't hold fails at apply instead, by the create's 409, and
// nothing is deleted first.
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
	state := stateString(got.JSON200.State)
	if state == "deleting" || state == "deleted" {
		return
	}
	existing{
		kind: "instance group", resourceName: ResourceName(instanceGroupResourceSuffix), target: target, id: id,
		state: state,
	}.warnReplacing(diags)
}
