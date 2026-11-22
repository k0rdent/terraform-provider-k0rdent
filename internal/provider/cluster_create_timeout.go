package provider

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// onCreateTimeoutAttribute is the attribute's name.
const onCreateTimeoutAttribute = "on_create_timeout"

// The two values `on_create_timeout` takes.
const (
	createTimeoutTaints = "taint"
	createTimeoutKeeps  = "keep"
)

// onCreateTimeoutSchemaAttribute declares `on_create_timeout`. Optional and
// not Computed, with no default, so leaving it out keeps the taint behavior.
func onCreateTimeoutSchemaAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Optional:   true,
		Validators: []validator.String{stringvalidator.OneOf(createTimeoutTaints, createTimeoutKeeps)},
		MarkdownDescription: "What a create does when its wait reaches `timeouts.create`. `taint`, the " +
			"default when unset, returns an error. Terraform keeps the cluster in state and taints it, so " +
			"the next apply plans a replacement unless you run `terraform untaint` first. `keep` reads no " +
			"state as ready: Create records the last lifecycle state the wait read, normally `creating`, " +
			"without an error or taint. The next plan is an in-place update with `state` unknown, and its " +
			"apply waits for the same cluster. If it settled between runs, the refresh reads `active` and " +
			"the plan is empty.\n\n" +
			"With `keep`, put a postcondition such as `condition = self.state == \"active\"` on the " +
			"resource when a pipeline requires a ready cluster. The postcondition fails the timed-out run. " +
			"On a retry while the cluster is still creating, `state` is unknown at plan, so the saved plan " +
			"reaches its apply and waits.\n\n" +
			"`keep` also covers a run that's interrupted while the create waits, such as by a CI cancel or a " +
			"job timeout: Create records the cluster the same way. Terraform still ends that run with " +
			"`execution halted` and a non-zero exit code. A `create_token` does the same for an interrupt, " +
			"but not for a timeout. " +
			"Never sent to the platform.",
	}
}

// keepStoppedCreate records what the wait last read and returns success, so
// Terraform leaves the row untainted. A create's scope is already in state,
// and its UID in private state, before its wait starts. uid also goes to the
// `uid` attribute when the create's answer named one. The lifecycle state is
// the last observed one: the stopped context can't make another API call, and
// a create accepted by the platform is creating when no read has happened yet.
func keepStoppedCreate(ctx context.Context, kind, resourceName, uid string, state *tfsdk.State, err error, diags *diag.Diagnostics) {
	last := creatingState
	var stopped stoppedWaiting
	if errors.As(err, &stopped) && stopped.last != "" {
		last = stopped.last
	}
	diags.Append(state.SetAttribute(ctx, path.Root("state"), types.StringValue(last))...)
	if uid != "" {
		diags.Append(state.SetAttribute(ctx, path.Root("uid"), types.StringValue(uid))...)
	}
	next := fmt.Sprintf("State records it untainted, and nothing was cancelled or deleted. The next "+
		"`terraform plan` proposes an in-place update whose apply waits for this same %s, or a refresh "+
		"reads it settled and plans nothing.", kind)
	if waitInterrupted(err) {
		diags.AddWarning(
			fmt.Sprintf("The %s is still being created", kind),
			fmt.Sprintf("The run was interrupted while the %s was `%s`. %s", kind, last, next),
		)
		return
	}
	diags.AddWarning(
		fmt.Sprintf("The %s is still being created", kind),
		fmt.Sprintf("The create timeout expired with the %s `%s`. %s If this run must produce a ready %s, "+
			"fail it with a `state == \"active\"` postcondition on `%s`.", kind, last, next, kind, resourceName),
	)
}
