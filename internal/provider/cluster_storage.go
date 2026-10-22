package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// A cluster's storage ceiling, which v1.3.0-dev.4 adds as `storage.ceilingGiB`
// on the cluster, its create and its update. It's the storage the cluster
// declares, charged against the storage_gib quota and not enforced on the
// storage array. It can be raised and never lowered: a lower value is a 422.

// maxStorageRaises is how many times one apply sends a raise the platform
// refuses because the cluster isn't settled. The contract says that refusal
// "can be retried shortly", and each resend follows a wait for the cluster to
// settle, so a refusal that outlasts this many is something else moving the
// cluster, which is reported rather than chased.
const maxStorageRaises = 3

func storageCeilingAttribute() schema.Int64Attribute {
	return schema.Int64Attribute{
		Optional: true,
		Computed: true,
		MarkdownDescription: fmt.Sprintf("Storage the cluster declares, in GiB. It's charged against the "+
			"organization's and the project's `storage_gib` quota, and isn't enforced on the storage array. "+
			"From %d to %d.\n\n"+
			"It can be raised in place and never lowered: a lower value is refused at plan time, since the "+
			"platform refuses it too. Left out, the cluster keeps whatever ceiling it has, and removing it "+
			"from configuration doesn't clear it. A raise is sent once the cluster is `active`, and one "+
			"refused while the cluster or its quota accounting is settling is sent again after the cluster "+
			"settles. A raise past a quota is refused with the amount that would be admitted.",
			k0rdent.ClusterStorageCeilingGiBMinimum, k0rdent.ClusterStorageCeilingGiBMaximum),
		Validators: []validator.Int64{int64validator.Between(
			k0rdent.ClusterStorageCeilingGiBMinimum, k0rdent.ClusterStorageCeilingGiBMaximum)},
		PlanModifiers: []planmodifier.Int64{
			int64planmodifier.UseStateForUnknown(),
			neverLowered{},
		},
	}
}

// neverLowered refuses a plan that lowers the ceiling. Replacing the cluster
// instead would destroy its workloads over a quota number, so the plan stops
// and says how to get there on purpose.
type neverLowered struct{}

func (neverLowered) Description(context.Context) string {
	return "The storage ceiling can be raised and never lowered."
}

func (n neverLowered) MarkdownDescription(ctx context.Context) string { return n.Description(ctx) }

func (neverLowered) PlanModifyInt64(_ context.Context, req planmodifier.Int64Request, resp *planmodifier.Int64Response) {
	from, to := req.StateValue, resp.PlanValue
	if from.IsNull() || from.IsUnknown() || to.IsNull() || to.IsUnknown() || to.ValueInt64() >= from.ValueInt64() {
		return
	}
	addLoweredCeilingError(&resp.Diagnostics, from.ValueInt64(), to.ValueInt64())
}

// addLoweredCeilingError is the refusal of a lower ceiling, at plan or, for a
// value the plan didn't know, at apply. Local: decided from state and the plan.
func addLoweredCeilingError(diags *diag.Diagnostics, from, to int64) {
	diags.AddAttributeError(path.Root("storage_ceiling_gib"), "The storage ceiling can't be lowered",
		fmt.Sprintf("The cluster's storage ceiling is %d GiB and the configuration asks for %d. The platform "+
			"raises a ceiling and never lowers one, and refuses a lower value. Keep %d or more, or replace "+
			"the cluster, with `terraform apply -replace`, to start it at %d.", from, to, from, to))
}

// storageLowered reports whether the plan, as the apply resolved it, lowers
// the ceiling state has. neverLowered catches a known value at plan time, and
// this catches one that was unknown there.
func storageLowered(plan, state clusterModel) (from, to int64, lowered bool) {
	p, s := plan.StorageCeilingGiB, state.StorageCeilingGiB
	if p.IsNull() || p.IsUnknown() || s.IsNull() || s.IsUnknown() {
		return 0, 0, false
	}
	return s.ValueInt64(), p.ValueInt64(), p.ValueInt64() < s.ValueInt64()
}

// storageRequest is the ceiling as a request body carries it, nil for none.
func storageRequest(v types.Int64) *k0rdent.ClusterStorage {
	if v.IsNull() || v.IsUnknown() {
		return nil
	}
	return &k0rdent.ClusterStorage{CeilingGiB: v.ValueInt64()}
}

// storageRaised reports whether the plan raises the ceiling state has.
func storageRaised(plan, state clusterModel) bool {
	to := plan.StorageCeilingGiB
	if to.IsNull() || to.IsUnknown() {
		return false
	}
	from := state.StorageCeilingGiB
	return from.IsNull() || from.IsUnknown() || to.ValueInt64() > from.ValueInt64()
}

// raiseStorage sends the new ceiling as a PATCH of its own.
//
// It's on its own because the platform answers a raise on a cluster that
// isn't `active`, and one whose quota accounting is still settling, with a
// 409 that carries no reason, and the contract says it "can be retried
// shortly". Alone in the request, that 409 can only be about the raise, so it
// waits for the cluster to settle and is sent again, at most
// maxStorageRaises times. Every other refusal, a quota one included, is the
// answer. A 503 the quota check couldn't make is resent by the transport.
func (r *clusterResource) raiseStorage(ctx context.Context, target clusterTarget, id string, ceiling types.Int64) diag.Diagnostics {
	var diags diag.Diagnostics
	body := k0rdent.ClusterUpdateRequest{Storage: storageRequest(ceiling)}
	for attempt := 1; ; attempt++ {
		got, err := r.meta.Client.UpdateClusterWithResponse(ctx, target.region, target.project, id, body)
		if err != nil {
			addAPIError(&diags, "raise the cluster's storage ceiling", nil, nil, err)
			return diags
		}
		if got.JSON200 != nil || got.JSON202 != nil {
			return diags
		}
		if !unsettledRaise(got.HTTPResponse, got.Body) || attempt == maxStorageRaises {
			advice := ""
			if unsettledRaise(got.HTTPResponse, got.Body) {
				advice = fmt.Sprintf("The platform refused the storage ceiling raise %d times while the cluster "+
					"or its quota accounting was settling, waiting for the cluster to settle before each one. "+
					"Nothing was changed. Run the apply again once the cluster reads `active`.", attempt)
			}
			refusal := k0rdent.CheckResponse(got.HTTPResponse, got.Body)
			if refusal == nil {
				refusal = answeredWithout(got.HTTPResponse, "cluster")
			}
			addAdvisedError(&diags, "Could not raise the cluster's storage ceiling", advice, refusal)
			return diags
		}
		tflog.Info(ctx, "the platform refused the storage ceiling raise while the cluster was settling, so "+
			"it's sent again once the cluster settles", withRequestID(map[string]any{
			"cluster": id, "attempt": attempt,
		}, got.HTTPResponse, got.Body))
		if err := r.waitForSettled(ctx, target, id, "the cluster to settle before raising its storage ceiling"); err != nil {
			addClusterWaitError(&diags, "The cluster did not settle for its storage ceiling raise", changeKeepsRunning, err)
			return diags
		}
	}
}

// unsettledRaise is the refusal the contract says a raise "can be retried"
// after: a 409 with no reason at the top or in its details.
func unsettledRaise(resp *http.Response, body []byte) bool {
	if resp == nil || resp.StatusCode != http.StatusConflict {
		return false
	}
	var refusal *k0rdent.APIError
	if !errors.As(k0rdent.CheckResponse(resp, body), &refusal) {
		return false
	}
	if refusal.Reason != "" {
		return false
	}
	for _, d := range refusal.Details {
		if reason, ok := d["reason"].(string); ok && reason != "" {
			return false
		}
	}
	return true
}

// storageCeilingFrom is the ceiling a read reports, null when the cluster
// declares none.
func storageCeilingFrom(c k0rdent.Cluster) types.Int64 {
	if c.Storage == nil {
		return types.Int64Null()
	}
	return types.Int64Value(c.Storage.CeilingGiB)
}
