package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// A cluster the platform reports `failed`.
//
// The contract says `failed` "indicates the last lifecycle transition failed"
// and "is not a stop condition for a poller — a watchdog-force-failed cluster
// may still return to `active` with no user action". So a plan on a cluster
// the refresh read `failed` warns with the platform's reason and message, and
// is an update that waits for the cluster, the way a resumed create does
// (resume.go). Without the update, `terraform apply` would exit 0 with "No
// changes" on a cluster that doesn't work. The apply waits through the
// provisional window. A cluster back to `active` passes, and one still
// `failed` is an error that says how to replace it. Nothing is deleted or
// replaced on its own, since the platform can still bring the cluster back.
//
// The plan only warns, never errors: `terraform destroy` runs a normal plan
// first, and an error there would stop the destroy of a cluster that can't
// be used.
//
// Two kinds of `failed` warn and plan nothing. Reason `CANCELLED` is what the
// cluster_cancel_operation action leaves, a stop the tenant asked for that
// keeps the cluster on purpose. And `on_failed = "keep"` is a tenant who has
// decided to keep a cluster the platform won't scale or upgrade any more, such
// as one whose upgrade failed, without every apply failing until it's rebuilt.
//
// A cluster whose delete failed isn't told apart: the platform doesn't say
// which operation failed, and at v1.3.0-rc.4 it keeps a cluster's node pools
// until the delete settles `deleted`, so a failed teardown reads like any
// other failure. It plans the wait like one, and `terraform destroy` deletes
// it as before.

// onFailedAttribute is the attribute's name.
const onFailedAttribute = "on_failed"

// The two values `on_failed` takes.
const (
	failedWaits = "wait"
	failedKept  = "keep"
)

// onFailedSchemaAttribute declares `on_failed`. Optional, not Computed, and
// null reads as `wait`, as `on_destroy_busy` does, so a configuration that
// never mentions it plans no change after an upgrade to a provider that has
// it.
func onFailedSchemaAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Optional:   true,
		Validators: []validator.String{stringvalidator.OneOf(failedWaits, failedKept)},
		MarkdownDescription: "What a plan does with a cluster the platform reports `failed`. `wait`, the " +
			"default when unset, plans an update and warns with the platform's reason: the apply waits " +
			"up to " + shortDuration(DefaultProvisionalFailureWindow) + " for the platform to bring the " +
			"cluster back to `active`, and fails, deleting nothing, if it doesn't. `keep` warns and plans " +
			"nothing, for a cluster you keep as it is, such as one whose upgrade failed: the platform " +
			"doesn't scale or upgrade a `failed` cluster, though its `tags`, `display_name` and " +
			"`description` can still change. A cluster left `failed` by the `" +
			ResourceName(clusterCancelOperationActionSuffix) + "` action plans nothing either way.\n\n" +
			"Never sent to the platform.",
	}
}

// failedState is the state a cluster reports when its last lifecycle
// transition failed.
const failedState = string(k0rdent.ClusterStateFailed)

// failedReadKey is the private state key a read sets to what the platform
// said about a `failed` cluster, and clears otherwise. A plan reads it, since
// neither the reason nor the message is an attribute: both move without a
// write.
const failedReadKey = "failed_read"

// failedRead is what the platform said about a cluster read `failed`.
type failedRead struct {
	// Reason is `stateReason`, and Detail it with `stateMessage` as
	// stateDetail words them.
	Reason string `json:"reason"`
	Detail string `json:"detail"`
}

// waits reports whether a cluster `failed` this way is waited on. Every reason
// is, the empty one included, except the one a cancel leaves.
func (f failedRead) waits() bool {
	return f.Reason != string(k0rdent.StateReasonCANCELLED)
}

// recordFailed keeps what the platform said about a cluster read `failed`, and
// clears it for any other state.
func recordFailed(ctx context.Context, private privateWriter, state string, f failedRead) diag.Diagnostics {
	if v := reflect.ValueOf(private); !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return nil
	}
	var raw []byte
	if state == failedState {
		var err error
		if raw, err = json.Marshal(f); err != nil {
			var diags diag.Diagnostics
			// Local: the provider could not encode its own record.
			diags.AddError("Could not record the failed cluster's reason", err.Error())
			return diags
		}
	}
	return private.SetKey(ctx, failedReadKey, raw)
}

// lastFailed is what the last read recorded. A read that recorded nothing
// reads as a `failed` with no reason, which is waited on.
func lastFailed(ctx context.Context, private privateState) failedRead {
	var f failedRead
	if v := reflect.ValueOf(private); !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return f
	}
	raw, diags := private.GetKey(ctx, failedReadKey)
	if diags.HasError() || len(raw) == 0 || json.Unmarshal(raw, &f) != nil {
		return failedRead{}
	}
	return f
}

// waitsOnFailed reports whether an update waits for a cluster prior state
// holds `failed`: on_failed isn't `keep`, and no cancel left it so.
func waitsOnFailed(onFailed types.String, f failedRead) bool {
	return onFailed.ValueString() != failedKept && f.waits()
}

// planFailed warns about a cluster the refresh read `failed`, and plans the
// update that waits for it unless it's kept or cancelled. It runs from
// ModifyPlan next to planResume, before the check for a plan with no change.
func planFailed(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse, window time.Duration) {
	if req.State.Raw.IsNull() || resp.Plan.Raw.IsNull() || priorState(ctx, req.State) != failedState {
		return
	}
	var id, onFailed types.String
	resp.Diagnostics.Append(req.State.GetAttribute(ctx, path.Root("id"), &id)...)
	resp.Diagnostics.Append(resp.Plan.GetAttribute(ctx, path.Root(onFailedAttribute), &onFailed)...)
	if resp.Diagnostics.HasError() {
		return
	}
	f := lastFailed(ctx, req.Private)
	waits := waitsOnFailed(onFailed, f)
	warnFailed(&resp.Diagnostics, id.ValueString(), f, onFailed.ValueString() == failedKept, window)
	if waits {
		planWaitOn(ctx, req, resp, failedState)
	}
}

// warnFailed is the plan's warning for a cluster read `failed`.
func warnFailed(diags *diag.Diagnostics, id string, f failedRead, kept bool, window time.Duration) {
	summary := fmt.Sprintf("Cluster %q is failed", id)
	limits := "The platform doesn't scale or upgrade a `failed` cluster, though its `tags`, " +
		"`display_name` and `description` can still change."
	var what string
	switch {
	case !f.waits():
		summary += " after a cancel"
		what = "That's what cancelling its operation leaves, so the plan doesn't change it. " + limits
	case kept:
		what = "`on_failed` is `keep`, so the plan doesn't change it. " + limits
	default:
		what = fmt.Sprintf("Its last change didn't finish, and the platform can still bring it back to "+
			"`active` on its own, so the plan updates it: `terraform apply` waits up to %s and passes if "+
			"it comes back. If it doesn't, the apply fails and nothing is deleted.", shortDuration(window))
	}
	detail := fmt.Sprintf("The platform reports the cluster `failed`%s. %s %s", f.Detail, what,
		replaceAdvice("cluster", ResourceName(clusterResourceSuffix)))
	if f.waits() && !kept {
		detail += " To keep the cluster as it is, and plan no change for it, set `on_failed = \"keep\"`."
	}
	diags.AddWarning(summary, redactDiagnosticText(detail))
}

// replaceAdvice is the way out of a failed object: create it again, or
// delete it.
func replaceAdvice(kind, resourceName string) string {
	return fmt.Sprintf("To create the %s again, run `terraform apply -replace=<address>`, with this "+
		"resource's address, such as %s.example. To delete it, run `terraform destroy`.", kind, resourceName)
}
