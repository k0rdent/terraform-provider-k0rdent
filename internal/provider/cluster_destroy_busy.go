package provider

import (
	"context"
	"fmt"
	"reflect"

	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// What a destroy does with a cluster that is running an operation.
//
// The platform deletes a cluster only from `active` or `failed`, and its 409
// names cancelCluster as the way through. Two answers are reasonable, and
// which one should be the default is a product decision still open. A
// destroy that cancels always ends, even on a cluster wedged mid-operation,
// but whatever the operation was doing doesn't finish, and a cancelled
// upgrade leaves the cluster `failed`. A destroy that waits lets the
// operation land, the way GKE's delete waits for the cluster to come to rest
// and EKS's retries while an update is in progress, but on a wedged cluster
// it runs until the delete timeout and deletes nothing.
//
// So both are built, and `on_destroy_busy` picks. Unset is `cancel`, which is
// what every destroy did before the attribute existed, so a state written
// then reads the same. The value is the provider's alone and is never sent.
// Like `timeouts`, a destroy reads it from state, so it takes effect once an
// apply has recorded it.

// onDestroyBusyAttribute is the attribute's name.
const onDestroyBusyAttribute = "on_destroy_busy"

// The two values `on_destroy_busy` takes.
const (
	destroyCancels = "cancel"
	destroyWaits   = "wait"
)

// onDestroyBusySchemaAttribute declares `on_destroy_busy`.
//
// Optional and not Computed, with no default, so that a configuration that
// never mentions it plans no change after an upgrade to a provider that has
// it. Null reads as `cancel`.
func onDestroyBusySchemaAttribute() schema.StringAttribute {
	return schema.StringAttribute{
		Optional:   true,
		Validators: []validator.String{stringvalidator.OneOf(destroyCancels, destroyWaits)},
		MarkdownDescription: "What a destroy does when the cluster is `creating` or `updating`, which the " +
			"platform refuses to delete. `cancel`, the default when unset, cancels the running operation " +
			"and then deletes the cluster. Whatever that operation was doing doesn't finish, and a cancelled " +
			"upgrade leaves the cluster `failed`. `wait` waits for the operation to finish and then deletes " +
			"the cluster, within the delete timeout. If another operation starts in the meantime, it waits " +
			"for that one too. An operation that outlasts the timeout fails the destroy with nothing " +
			"deleted, so a cluster wedged mid-operation needs `cancel`.\n\n" +
			"Never sent to the platform. A destroy reads it from state, like `timeouts`, so change it in " +
			"an apply before the destroy that should use it. That apply sends nothing and doesn't wait, " +
			"even while the cluster is mid-operation. A create that stops waiting or fails records it, so " +
			"the destroy of that cluster follows it too. To change it on a tainted cluster, run " +
			"`terraform untaint` on the cluster first: otherwise the apply plans a new cluster in its " +
			"place, and the destroy of this one reads the value already in state.",
	}
}

// destroyBusyOf is what a destroy does with a busy cluster, given the
// attribute's value. Anything but `wait` cancels.
func destroyBusyOf(v types.String) string {
	if v.ValueString() == destroyWaits {
		return destroyWaits
	}
	return destroyCancels
}

// keepDestroySettings records the attributes a destroy reads from state when
// a create got as far as an id and no further, so that a tainted cluster is
// destroyed the way its configuration asked. Without this they'd be null,
// which means `cancel` and the provider's default timeout.
func keepDestroySettings(ctx context.Context, plan clusterModel, st *tfsdk.State, diags *diag.Diagnostics) {
	if st.Raw.IsNull() {
		return
	}
	diags.Append(st.SetAttribute(ctx, path.Root(onDestroyBusyAttribute), plan.OnDestroyBusy)...)
	diags.Append(st.SetAttribute(ctx, path.Root("timeouts"), plan.Timeouts)...)
}

// onlyProviderSettingsChanged says that an update changes nothing the
// platform holds: the node pools, the security groups, the storage ceiling
// and the ClusterType binding all read as state does. The metadata fields are
// the caller's to compare, since it already has.
func onlyProviderSettingsChanged(plan, state clusterModel) bool {
	change, _ := classifyRefChange(plan.ClusterType.ValueString(), boundRef(state),
		k0rdent.ClusterState(state.State.ValueString()))
	return change == refUnchanged && plan.NodePool.Equal(state.NodePool) &&
		!securityGroupsChanged(plan, state) && !storageRaised(plan, state)
}

// planOverPrior is the state an update that sends nothing writes: the plan,
// with prior state for each attribute the plan left unknown, which is the
// computed ones ModifyPlan marks when anything changes. Terraform requires
// the result to match every value the plan knew, so prior state can't stand
// in for those, even where the platform reads the two as the same.
func planOverPrior(plan, prior clusterModel) clusterModel {
	out := plan
	o, p := reflect.ValueOf(&out).Elem(), reflect.ValueOf(prior)
	for i := range o.NumField() {
		// An unexported field is bookkeeping, not an attribute.
		if !o.Type().Field(i).IsExported() {
			continue
		}
		if v, ok := o.Field(i).Interface().(interface{ IsUnknown() bool }); ok && v.IsUnknown() {
			o.Field(i).Set(p.Field(i))
		}
	}
	return out
}

// awaitTimedOutAdvice is the advice when a destroy told to wait ran out of time.
func awaitTimedOutAdvice(state string) string {
	return fmt.Sprintf("The cluster is `%s`, and `on_destroy_busy` is `wait`, so the destroy waited for "+
		"that operation to finish rather than cancel it, and the delete timeout ran out first. Nothing "+
		"was deleted and the cluster stays in state. Run the destroy again to keep waiting, raise "+
		"`timeouts.delete`, or set `on_destroy_busy = \"cancel\"` and apply before destroying to cancel "+
		"the operation instead. That apply sends nothing and doesn't wait. If the cluster is tainted, run "+
		"`terraform untaint` on it first, or the apply replaces it rather than recording the change. To "+
		"cancel the operation and keep the cluster, invoke the `%s` action.", state,
		ResourceName(clusterCancelOperationActionSuffix))
}
