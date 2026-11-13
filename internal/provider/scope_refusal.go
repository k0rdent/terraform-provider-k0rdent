package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
)

// The refusal of a scope move the platform can't make.
//
// A resource's region is its project's: a project is locked to one region,
// and the platform refuses every request for it in another. A project itself
// can only be in a region that holds projects. So a move of region or project
// that lands the resource where its project can't be would be a replacement
// whose create the platform refuses. allow_replace can't permit that.
//
// Refusing it at plan stops `terraform destroy` too, which runs that plan
// first. So the plan doesn't replace: it plans an update and warns, and the
// resource's Update refuses before it sends any request. The scope's planner
// (modifyScopePlan, or a project's planRegion) records the facts in planned
// private state, the resource's ModifyPlan warns with recordedScopeMove, and
// its Update refuses with refuseRecordedScopeMove. A guarded resource gets
// both from the guard. The security group, the peering and the binding call
// both functions in their own ModifyPlan and Update.
//
// One case can't be held at apply: another change the plan still replaces
// the resource for, a guarded one's with allow_replace = true or any on an
// unguarded one. Terraform deletes before the provider sees an update, so
// that plan is refused. The plan of the replacement's create would refuse it
// too (checkRegionMatchesProject), and this says so in the resource's terms.

// scopeRefusedKey carries the refusal from the plan to the Update.
const scopeRefusedKey = "scope_move_refused"

// scopeRefusal is a move of region or project the platform can't make.
type scopeRefusal struct {
	Moves []scopeMove `json:"moves"`
	// Where is the scope the resource can't be in, as a phrase: `region
	// "x" with project "y"`.
	Where string `json:"where"`
	// Reason is why, in sentences that don't name the resource.
	Reason string `json:"reason"`
}

// scopeMove is one scope attribute's planned change.
type scopeMove struct {
	Attr string `json:"attr"`
	From string `json:"from"`
	To   string `json:"to"`
	// Configured is whether the resource sets the attribute. Without it the
	// move comes from the provider block.
	Configured bool `json:"configured"`
}

// clearScopeRefusal drops a refusal an earlier plan left. A refused Update
// writes the plan's private data back to state, so without this it would
// outlive the plan that made it.
func clearScopeRefusal(ctx context.Context, resp *resource.ModifyPlanResponse) {
	if resp.Private != nil {
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, scopeRefusedKey, nil)...)
	}
}

// recordScopeRefusal writes the refusal into the plan's private data. With
// nowhere to write it the Update couldn't refuse, so the plan fails instead.
// The framework always hands ModifyPlan private data, so that's unreachable.
func recordScopeRefusal(ctx context.Context, resp *resource.ModifyPlanResponse, r scopeRefusal) {
	raw, err := json.Marshal(r)
	if err != nil || resp.Private == nil {
		// Local: fails closed.
		resp.Diagnostics.AddAttributeError(r.at(), "The provider could not record a refused move",
			fmt.Sprintf("The plan was refused because the apply couldn't refuse it: this resource can't be in %s.", r.Where))
		return
	}
	resp.Diagnostics.Append(resp.Private.SetKey(ctx, scopeRefusedKey, raw)...)
}

// readScopeRefusal is the refusal private data holds, if any.
func readScopeRefusal(ctx context.Context, private privateState, diags *diag.Diagnostics) *scopeRefusal {
	raw, d := private.GetKey(ctx, scopeRefusedKey)
	diags.Append(d...)
	if len(raw) == 0 {
		return nil
	}
	var r scopeRefusal
	if err := json.Unmarshal(raw, &r); err != nil || r.Where == "" {
		// Local: fails closed. The plan recorded a refusal this can't read.
		return &scopeRefusal{Where: "the region or project this plan names",
			Reason: "The plan recorded a move of its region or project that the platform can't make."}
	}
	return &r
}

// recordedScopeMove is the plan's half: a warning when the plan recorded a
// refused move, or an error when another change still replaces the
// resource, since Terraform would delete it before any Update ran. guarded
// says the resource holds tenant state behind the replacement guard, where
// only allow_replace = true lets a replacement through. Called from the
// resource's ModifyPlan after the scope's planner.
func recordedScopeMove(ctx context.Context, noun string, guarded bool, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if resp.Private == nil || req.State.Raw.IsNull() || req.Plan.Raw.IsNull() {
		return
	}
	r := readScopeRefusal(ctx, resp.Private, &resp.Diagnostics)
	if r == nil {
		return
	}
	var replacing []string
	if !guarded || replacementAllowed(ctx, req.Config) {
		planned, ok := modifierReplacements(ctx, req, resp)
		if !ok {
			// Local: fails closed, as the guard does. Unreachable while every
			// resource is built from resource/schema.
			resp.Diagnostics.AddError("The provider could not read this resource's schema",
				fmt.Sprintf("The plan was refused because the provider cannot tell whether it replaces this %s.", noun))
			return
		}
		for _, p := range planned {
			replacing = append(replacing, p.path.String())
		}
	}
	if len(replacing) > 0 {
		// Local: decided from the plan and the project or region list.
		resp.Diagnostics.AddAttributeError(r.at(),
			fmt.Sprintf("This %s can't be in %s, and this plan replaces it", noun, r.Where),
			redactDiagnosticText(fmt.Sprintf("%s The %s can't be created there. The plan also replaces the %s "+
				"for its change to %s, and the new one would be created there, so the plan is refused, and "+
				"nothing is changed. `terraform destroy` runs this plan first, so it's "+
				"refused too, and `terraform destroy -refresh=false` destroys from state as it is.\n\n"+
				"To keep the %s where it is, %s, and plan again.",
				r.moved(noun), noun, noun, orList(replacing), noun, r.keep())))
		return
	}
	resp.Diagnostics.AddAttributeWarning(r.at(),
		fmt.Sprintf("This %s can't be in %s, so the apply will refuse the change", noun, r.Where),
		redactDiagnosticText(fmt.Sprintf("%s\n\n"+
			"So this plan shows the %s updated in place, and the apply refuses that update before it sends "+
			"any request. None of this %s's changes in the plan are made. Other resources in the plan that "+
			"don't depend on it are still changed. `terraform destroy` isn't affected.\n\n"+
			"To keep the %s where it is, %s.",
			r.why(noun, guarded), noun, noun, noun, r.keep())))
	// The apply refuses this update, so no computed value can change. One
	// left unknown, such as a cluster's api_endpoint, would plan the
	// replacement of a resource that reads it, which Terraform makes before
	// the refusal. The moved attributes, and any the guard refused, keep
	// their planned values, so the plan still shows the change. Unlike the
	// guard's, this refusal holds at apply whatever else resolves then: a move
	// is only recorded from a region and project known at plan, so the final
	// plan records it again and keeps the same values.
	kept := make([]plannedReplacement, 0, len(r.Moves))
	for _, m := range r.Moves {
		kept = append(kept, plannedReplacement{path: path.Root(m.Attr)})
	}
	for _, refused := range readRefusal(ctx, resp.Private, &resp.Diagnostics) {
		kept = append(kept, plannedReplacement{path: pathOf(refused.Path)})
	}
	keepComputed(ctx, req, resp, kept)
}

// refuseRecordedScopeMove is the apply's half: it refuses an update the plan
// recorded a refused move for, before any request, and reports whether it
// did. The refusal is dropped from what's written back, so it can't outlive
// this apply. Called first in the resource's Update.
func refuseRecordedScopeMove(ctx context.Context, noun string, guarded bool, req resource.UpdateRequest, resp *resource.UpdateResponse) bool {
	if req.Private == nil {
		return false
	}
	r := readScopeRefusal(ctx, req.Private, &resp.Diagnostics)
	if r == nil {
		return false
	}
	// Local: decided from the plan, no request made. The framework keeps
	// prior state on an error.
	resp.Diagnostics.AddAttributeError(r.at(),
		fmt.Sprintf("This %s can't be in %s, so the apply refused the change", noun, r.Where),
		redactDiagnosticText(fmt.Sprintf("%s\n\n"+
			"The plan warned about this. The apply refused before it sent any request, so the %s is "+
			"unchanged and none of its changes in this plan were made. `terraform destroy` isn't affected.\n\n"+
			"To keep the %s where it is, %s, and apply again.",
			r.why(noun, guarded), noun, noun, r.keep())))
	if resp.Private != nil {
		resp.Diagnostics.Append(resp.Private.SetKey(ctx, scopeRefusedKey, nil)...)
	}
	return true
}

// at is the attribute a diagnostic points at: the first one that moved.
func (r scopeRefusal) at() path.Path {
	if len(r.Moves) == 0 {
		return path.Root("region")
	}
	return path.Root(r.Moves[0].Attr)
}

// moved says what the plan does to the resource, and why the platform can't.
func (r scopeRefusal) moved(noun string) string {
	if len(r.Moves) == 0 {
		return r.Reason
	}
	parts := make([]string, len(r.Moves))
	for i, m := range r.Moves {
		parts[i] = fmt.Sprintf("%s %q to %q", m.Attr, m.From, m.To)
	}
	return fmt.Sprintf("The plan moves the %s from %s. %s", noun, strings.Join(parts, " and "), r.Reason)
}

// why is moved, and what a replacement would do.
func (r scopeRefusal) why(noun string, guarded bool) string {
	out := fmt.Sprintf("%s The %s can't be created there, so a replacement can't move it either.",
		r.moved(noun), noun)
	if guarded {
		out += fmt.Sprintf(" `%s` doesn't change that.", allowReplaceAttribute)
	}
	return out
}

// keep says how to put the scope back.
func (r scopeRefusal) keep() string {
	if len(r.Moves) == 0 {
		return "undo the change to its region or project"
	}
	parts := make([]string, len(r.Moves))
	for i, m := range r.Moves {
		parts[i] = fmt.Sprintf("set `%s` back to %q", m.Attr, m.From)
		if !m.Configured {
			parts[i] = fmt.Sprintf("add `%[1]s = %[2]q` to this resource, or set the provider's `%[1]s` back to %[2]q",
				m.Attr, m.From)
		}
	}
	return strings.Join(parts, " and ")
}
