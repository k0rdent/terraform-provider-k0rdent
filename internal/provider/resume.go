package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// A create or delete that outlives the run waiting for it.
//
// Hardware creates and destroys can outlast a run, so a wait that times out,
// or a run someone interrupts, is normal use. Nothing the provider does stops the operation on the platform,
// so what matters is what the next run does with it.
//
// # A create
//
// A create whose wait stops while the platform is still building fails, so
// that no pipeline reads the object as ready. Terraform taints any create that
// fails (its maybeTainted, which no provider can avoid), and plans to replace
// a tainted object: destroy it, cancelling the create that was nearly done,
// and create it again. The error says to run `terraform untaint` and then
// `terraform apply` instead. After the untaint, the next plan reads the object
// `creating`, plans an update, and the update waits for the same object. No
// second create is sent.
//
// A cluster and an instance group take an `id` from configuration, so a plan
// to create one can read it first (createGuard). The plan to replace a
// tainted one can't be refused, since `terraform destroy` runs it first to
// refresh state, so it warns instead, before the approval, that applying it
// cancels what the platform is building, with the same advice. A cluster
// that no Terraform apply made is refused.
//
// # A delete
//
// A destroy's error leaves the object in state, so the next destroy deletes it
// again. Every Delete reads the object first, and one already `deleting` is
// waited out rather than sent a second DELETE, which most kinds answer 409.

// creatingState is the state every kind that resumes a create reports while
// the platform builds it.
const creatingState = "creating"

// keptThroughResume are the computed attributes a resumed create never marks
// unknown: they name the object, and a dependent that forces replacement on
// one would plan to replace itself over a value that can't change.
var keptThroughResume = map[string]bool{"id": true, "region": true, "project": true}

// stillCreating reports whether prior state's `state` says the platform is
// still building the object.
func stillCreating(ctx context.Context, state tfsdk.State) bool {
	var prior types.String
	if state.Raw.IsNull() || state.GetAttribute(ctx, path.Root("state"), &prior).HasError() {
		return false
	}
	return prior.ValueString() == creatingState
}

// planResume plans an update for an object the last read found still
// `creating`, so that the apply waits for it, though nothing in the
// configuration moved. That's an object whose create stopped waiting and was
// untainted, or one imported mid-create. It runs from ModifyPlan before the
// check for a plan with no change, since one is what it changes.
//
// `state` goes unknown, and so does every computed attribute the
// configuration leaves out that is still null, since the platform fills those
// in as the object settles. The rest keep what the last read found, and the
// update keeps them: the next refresh brings any that moved up to date.
func planResume(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || resp.Plan.Raw.IsNull() || !stillCreating(ctx, req.State) {
		return
	}
	var plan, config map[string]tftypes.Value
	if resp.Plan.Raw.As(&plan) != nil || req.Config.Raw.As(&config) != nil {
		return
	}
	// A copy: the map As fills shares the value's own.
	planned := make(map[string]tftypes.Value, len(plan))
	for name, v := range plan {
		planned[name] = v
	}
	for name, a := range resp.Plan.Schema.GetAttributes() {
		v, ok := planned[name]
		if !ok || !a.IsComputed() || keptThroughResume[name] || !v.IsKnown() {
			continue
		}
		if c, set := config[name]; set && !c.IsNull() {
			continue
		}
		if name == "state" || v.IsNull() {
			planned[name] = tftypes.NewValue(v.Type(), tftypes.UnknownValue)
		}
	}
	resp.Plan.Raw = tftypes.NewValue(resp.Plan.Raw.Type(), planned)
}

// providerOnly are attributes the platform never sees, and that a resumed
// create takes a change to with it: raising `timeouts.create` is what the
// advice to a create that stopped waiting suggests. `on_destroy_busy` isn't
// one: an apply that changes it alone mustn't wait, since it's how a destroy
// is told to stop waiting (cluster_destroy_busy.go).
var providerOnly = map[string]bool{"timeouts": true, allowReplaceAttribute: true}

// onlyResume reports whether an update's plan changes nothing the platform
// holds beyond what planResume marked unknown.
func onlyResume(plan, state tftypes.Value) bool {
	var planned, prior map[string]tftypes.Value
	if plan.As(&planned) != nil || state.As(&prior) != nil {
		return false
	}
	for name, v := range planned {
		if !v.IsKnown() || providerOnly[name] {
			continue
		}
		if !v.IsFullyKnown() || !v.Equal(prior[name]) {
			return false
		}
	}
	return true
}

// fillUnknown is the plan with each unknown top-level value taken from fresh.
// Every value the plan knew stays as it was, which is what Terraform holds an
// apply's result to.
func fillUnknown(plan, fresh tftypes.Value) (tftypes.Value, error) {
	var planned, read map[string]tftypes.Value
	if err := plan.As(&planned); err != nil {
		return tftypes.Value{}, err
	}
	if err := fresh.As(&read); err != nil {
		return tftypes.Value{}, err
	}
	out := make(map[string]tftypes.Value, len(planned))
	for name, v := range planned {
		out[name] = v
		if !v.IsKnown() {
			out[name] = read[name]
		}
	}
	return tftypes.NewValue(plan.Type(), out), nil
}

// resumable is how one kind finishes a create an earlier apply stopped
// waiting for.
type resumable[M any] struct {
	// kind is the resource as a sentence names it, e.g. "cluster".
	kind string
	// resourceName is the Terraform type, for the commands it gives.
	resourceName string
	// timeout is the create's own timeout, and fallback its default.
	timeout  func(context.Context, time.Duration) (time.Duration, diag.Diagnostics)
	fallback time.Duration
	// wait waits for the object to settle.
	wait func(ctx context.Context) error
	// read reads the settled object, reconciled against the plan.
	read func(ctx context.Context) (M, bool, diag.Diagnostics)
}

// resume runs first in an Update. When the last read found the object still
// `creating` and the plan changes nothing else, it waits for the create under
// the create's timeout, records what it settled to, and reports true: the
// update is done. Any other update is the Update's own, and reports false.
//
// An update's error doesn't taint, so a wait that stops again here is an
// error, and the object stays in state, still `creating`, for the next apply.
func (p resumable[M]) resume(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) bool {
	if !stillCreating(ctx, req.State) || !onlyResume(req.Plan.Raw, req.State.Raw) {
		return false
	}
	ctx, cancel := withTimeout(ctx, p.timeout, p.fallback, &resp.Diagnostics)
	defer cancel()
	if resp.Diagnostics.HasError() {
		return true
	}
	if err := p.wait(ctx); err != nil {
		if stillRunning(err) {
			addCreateStillRunning(&resp.Diagnostics, p.kind, p.resourceName, err, false)
		} else {
			addAdvisedError(&resp.Diagnostics, fmt.Sprintf("The %s's create failed", p.kind),
				fmt.Sprintf("An earlier apply stopped waiting for this %s's create, and the platform has since "+
					"reported it failed. Nothing was deleted. To create it again, run "+
					"`terraform apply -replace=<address>`, with this resource's address, such as %s.example.",
					p.kind, p.resourceName), err)
		}
		return true
	}

	got, found, diags := p.read(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return true
	}
	if !found {
		// Local: the provider's conclusion from two calls.
		resp.Diagnostics.AddError(
			fmt.Sprintf("The %s disappeared while its create finished", p.kind),
			fmt.Sprintf("The %s settled, and reading it back answered 404.", p.kind),
		)
		return true
	}
	fresh := tfsdk.State{Schema: req.Plan.Schema, Raw: tftypes.NewValue(req.Plan.Raw.Type(), nil)}
	resp.Diagnostics.Append(fresh.Set(ctx, &got)...)
	if resp.Diagnostics.HasError() {
		return true
	}
	out, err := fillUnknown(req.Plan.Raw, fresh.Raw)
	if err != nil {
		// Local: the provider could not merge its own plan.
		resp.Diagnostics.AddError("Could not record the finished create", err.Error())
		return true
	}
	var finished M
	resp.Diagnostics.Append(tfsdk.State{Schema: req.Plan.Schema, Raw: out}.Get(ctx, &finished)...)
	if resp.Diagnostics.HasError() {
		return true
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &finished)...)
	return true
}

// untaintAdvice is the way back to an object whose create stopped waiting:
// keep it, and wait for it.
func untaintAdvice(kind, resourceName string) string {
	return fmt.Sprintf("Next, to wait for this same %s, replacing <address> with this resource's address, "+
		"such as %s.example:\n\n"+
		"    terraform untaint <address>\n"+
		"    terraform apply\n\n"+
		"Don't run `terraform apply` first: Terraform would plan to destroy the %s, cancelling what the "+
		"platform has built so far, and create it again.", kind, resourceName, kind)
}

// addCreateStillRunning reports a create whose wait stopped while the
// platform was still building. It's an error, so no pipeline reads the object
// as ready. On the create itself Terraform taints the object, so the advice
// starts with the untaint. A resumed create is an update, which never taints,
// and is advised to apply again.
func addCreateStillRunning(diags *diag.Diagnostics, kind, resourceName string, err error, tainted bool) {
	summary := fmt.Sprintf("The %s is still being created", kind)
	detail := redactDiagnosticText(err.Error()+requestIDLine(requestIDIn(err))) + "\n\n"
	if tainted {
		detail += fmt.Sprintf("Nothing was cancelled or deleted. The %s is in state, and Terraform marks it "+
			"tainted because its create returned this error.\n\n", kind) + untaintAdvice(kind, resourceName)
	} else {
		detail += fmt.Sprintf("Nothing was cancelled or deleted, and the %s stays in state. Next, run "+
			"`terraform apply` again to keep waiting for it.", kind)
	}
	if !waitInterrupted(err) {
		detail += " If a create routinely takes this long, raise `create` in the resource's `timeouts` " +
			"block before applying."
	}
	diags.AddError(summary, detail)
}

// removedBlockAdvice is the way to stop managing an object the platform can't
// delete, in configuration rather than by editing state.
func removedBlockAdvice(kind, resourceName string) string {
	return fmt.Sprintf("To stop managing the %s until then, without editing state by hand, delete its "+
		"block from the configuration and add this one, replacing <address> with this resource's "+
		"address, such as %s.example:\n\n"+
		"    removed {\n"+
		"      from = <address>\n"+
		"      lifecycle {\n"+
		"        destroy = false\n"+
		"      }\n"+
		"    }\n\n"+
		"Then `terraform apply`. Terraform forgets the %s and leaves it on the platform, and the plan "+
		"shows that before anything happens. This needs Terraform 1.7 or later.", kind, resourceName, kind)
}

// deleteRefusedAdvice opens the advice on a delete the platform ended without
// deleting the object.
const deleteRefusedAdvice = "The platform ended this delete without deleting it"

// addDeleteWaitError reports a delete whose wait failed. When it stopped with
// the delete still running, it says that the next destroy picks it up. When
// the platform failed the delete, it gives the way out in order: destroy
// again, then report it and stop managing it, never an edit to state.
func addDeleteWaitError(diags *diag.Diagnostics, kind, resourceName, summary string, err error) {
	if refusedByPlatform(err) && !stillRunning(err) {
		addAdvisedError(diags, summary, fmt.Sprintf(deleteRefusedAdvice+", and the %s stays "+
			"in state. Next: run `terraform destroy` again, which asks the platform to delete it again.\n\n"+
			"If that fails the same way, the platform can't finish deleting it. Pass the details above to "+
			"whoever runs the landscape. ", kind)+removedBlockAdvice(kind, resourceName), err)
		return
	}
	if !stillRunning(err) {
		addWaitError(diags, summary, err)
		return
	}
	advice := fmt.Sprintf("The %s stays in state while the platform carries on deleting it. "+
		"Next: run `terraform destroy` again. It reads the %s first and waits for this same delete "+
		"rather than sending another.", kind, kind)
	if !waitInterrupted(err) {
		advice += " If a delete routinely takes this long, raise `delete` in the resource's `timeouts` " +
			"block and apply before destroying, since a destroy reads its timeout from state."
	}
	addAdvisedError(diags, summary, advice, err)
}
