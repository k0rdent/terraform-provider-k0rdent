package provider

import (
	"context"
	"fmt"
	"net/http"
	"reflect"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// A cluster's `sshAccess`: the Linux users written onto every node when it is
// provisioned, each with the public keys allowed to log in as that user. It's
// the same shape as an instance group's. `Cluster` doesn't carry it: the
// platform reports it at its own path, `listClusterSshAccess`, which a read
// asks after the cluster itself.
//
// Not every landscape serves that path. Where it's missing, state keeps what
// it held, which is what a create sent. The contract makes the users
// immutable after create ("cannot be changed or removed from a running
// cluster"), so a change is a replacement. That holds only while state's value
// is known to be what the nodes carry: sent by a create, or read from the
// platform. A cluster imported where the path is missing, or created by an
// older version, has no such value. There a changed value is recorded in
// place, sends nothing, and the plan says the nodes keep the users they have.

// sshAccessRecordedKey is the private state key set once the cluster's
// `ssh_access` in state is what its nodes carry: on a create, and on every
// read that got the users from the platform.
const sshAccessRecordedKey = "ssh_access_recorded"

// recordSSHAccess marks the cluster's `ssh_access` in state as what its nodes
// carry. Terraform always hands a create and a read their private state. A
// test that calls one directly may not, and there's nothing to record into.
func recordSSHAccess(ctx context.Context, private privateWriter) diag.Diagnostics {
	if v := reflect.ValueOf(private); !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return nil
	}
	return private.SetKey(ctx, sshAccessRecordedKey, []byte("true"))
}

// sshAccessRecorded reports whether state's `ssh_access` is what the nodes
// carry.
func sshAccessRecorded(ctx context.Context, private privateState) bool {
	if v := reflect.ValueOf(private); !v.IsValid() || (v.Kind() == reflect.Pointer && v.IsNil()) {
		return false
	}
	raw, diags := private.GetKey(ctx, sshAccessRecordedKey)
	return !diags.HasError() && string(raw) == "true"
}

// noSSHUsers is a set with no users in it: null or empty.
func noSSHUsers(v types.Set) bool {
	return v.IsNull() || (!v.IsUnknown() && len(v.Elements()) == 0)
}

// clusterSSHAccessRequiresReplace replaces a cluster whose recorded
// `ssh_access` has changed, since the platform can't change it on a running
// cluster. Null and `[]` both ask for no users, so moving between them
// replaces nothing. Without a record there's nothing to compare against, so
// the change is recorded in place: see warnUnrecordedSSHAccess. Nor does a
// configuration with no users, against a cluster whose last read held users
// the platform had released (teardown_read.go): both say there are none.
func clusterSSHAccessRequiresReplace(ctx context.Context, req planmodifier.SetRequest, resp *setplanmodifier.RequiresReplaceIfFuncResponse) {
	if noSSHUsers(req.PlanValue) && wasReleased(ctx, req.Private) {
		return
	}
	resp.RequiresReplace = sshAccessRecorded(ctx, req.Private) &&
		(!noSSHUsers(req.PlanValue) || !noSSHUsers(req.StateValue))
}

// warnUnrecordedSSHAccess says what an in-place `ssh_access` change on an
// unrecorded cluster does, which is nothing to its nodes.
func warnUnrecordedSSHAccess(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.State.Raw.IsNull() || req.Plan.Raw.IsNull() || sshAccessRecorded(ctx, req.Private) {
		return
	}
	unrecordedSSHAccessWarning(ctx, req.State, req.Plan, &resp.Diagnostics)
}

// unrecordedSSHAccessWarning is warnUnrecordedSSHAccess once it's known
// that state has no record: a warning when the plan changes `ssh_access`.
func unrecordedSSHAccessWarning(ctx context.Context, state tfsdk.State, plan tfsdk.Plan, diags *diag.Diagnostics) {
	var planned, prior types.Set
	if plan.GetAttribute(ctx, path.Root("ssh_access"), &planned).HasError() ||
		state.GetAttribute(ctx, path.Root("ssh_access"), &prior).HasError() || planned.Equal(prior) ||
		(noSSHUsers(planned) && noSSHUsers(prior)) {
		return
	}
	var id types.String
	_ = state.GetAttribute(ctx, path.Root("id"), &id)
	diags.AddWarning(
		redactDiagnosticText(fmt.Sprintf("`ssh_access` on %s is recorded, not sent", id.ValueString())),
		"The platform takes `ssh_access` only when it creates a cluster. This landscape doesn't report "+
			"a cluster's SSH users, and this cluster was imported, or created by an earlier version of the "+
			"provider, so state has no record of the users its nodes were made with. The apply records the "+
			"configured value and sends nothing, and the nodes keep whatever users they have.\n\nTo put "+
			"these users on the nodes, replace the cluster: `terraform apply -replace` with its address. "+
			"That deletes it and creates it again.")
}

// clusterSSHAccessState is prior state's `ssh_access`, kept when the platform
// says nothing about it. A prior with no type, from a model built by hand, is
// null.
func clusterSSHAccessState(prior types.Set) types.Set {
	if prior.ElementType(context.Background()) == nil {
		return types.SetNull(sshUserObjectType())
	}
	return prior
}

// sshAccessRouteMissing reports whether a status says the landscape doesn't
// serve `listClusterSshAccess`. A 404 is that whoever answered it: without the
// service's envelope a router or region guard had no route for the path, and
// with it, moments after the cluster read 200, the read raced a delete that
// the next refresh will see. Neither says anything about the users. A 405 or a
// 501 is a server that knows the path and not the read.
func sshAccessRouteMissing(status int) bool {
	return status == http.StatusNotFound || status == http.StatusMethodNotAllowed ||
		status == http.StatusNotImplemented
}

// readSSHAccess is the cluster's `ssh_access` after a read, and whether it
// came from the platform. Where the landscape doesn't serve the read, prior
// state is kept without a word. Any other failure keeps it too, with a
// warning, since a refresh shouldn't stop over a field the platform never
// changes.
func (r *clusterResource) readSSHAccess(ctx context.Context, target clusterTarget, id string, prior types.Set) (types.Set, bool, diag.Diagnostics) {
	got, err := r.meta.Client.ListClusterSshAccessWithResponse(ctx, target.region, target.project, id)
	if err == nil && got.JSON200 != nil {
		v, diags := sshAccessState(ctx, got.JSON200, clusterSSHAccessState(prior))
		return v, !diags.HasError(), diags
	}
	// Checked whatever err says: the client reports a body it can't decode
	// as an error, and a router's 404 needn't be the contract's shape.
	if got != nil && sshAccessRouteMissing(got.StatusCode()) {
		return clusterSSHAccessState(prior), false, nil
	}
	var failed, diags diag.Diagnostics
	var httpResp *http.Response
	var body []byte
	if got != nil {
		httpResp, body = got.HTTPResponse, got.Body
	}
	addAPIError(&failed, "read the cluster's SSH access", httpResp, body, err)
	for _, d := range failed.Errors() {
		diags.AddWarning(d.Summary(), d.Detail()+"\n\nState keeps the `ssh_access` it held. A read that "+
			"succeeds replaces it with what the platform reports.")
	}
	return clusterSSHAccessState(prior), false, diags
}
