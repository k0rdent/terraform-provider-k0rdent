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
			"reaches its apply and waits. An interrupt still returns an error and taints the cluster. " +
			"Never sent to the platform.",
	}
}

// keepTimedOutCreate records what the wait last read and returns success, so
// Terraform leaves the row untainted. A create's UID and scope are already in
// state before its wait starts. The lifecycle state is the last observed one:
// the timed-out context can't make another API call, and a create accepted by
// the platform is creating when no read has happened yet.
func keepTimedOutCreate(ctx context.Context, kind, resourceName string, state *tfsdk.State, err error, diags *diag.Diagnostics) {
	last := creatingState
	var stopped stoppedWaiting
	if errors.As(err, &stopped) && stopped.last != "" {
		last = stopped.last
	}
	diags.Append(state.SetAttribute(ctx, path.Root("state"), types.StringValue(last))...)
	diags.AddWarning(
		fmt.Sprintf("The %s is still being created", kind),
		fmt.Sprintf("The create timeout expired with the %s `%s`. State records it untainted, and nothing "+
			"was cancelled or deleted. The next `terraform plan` proposes an in-place update whose apply "+
			"waits for this same %s, or a refresh reads it settled and plans nothing. If this run must "+
			"produce a ready %s, fail it with a `state == \"active\"` postcondition on `%s`.",
			kind, last, kind, kind, resourceName),
	)
}
