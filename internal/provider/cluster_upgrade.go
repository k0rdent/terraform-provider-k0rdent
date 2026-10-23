package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-log/tflog"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// versionRefusal is the summary of every plan-time refusal of a version change.
const versionRefusal = "Cannot move the cluster to a different version of its ClusterType"

// destroyAdvice ends every plan-time refusal. `terraform destroy` refreshes
// with a normal plan first, and the provider cannot tell that plan from any
// other, so the refusal stops a destroy too, `-target` or not. Naming the
// bound version matters: after a failed step it is not the one the
// configuration had before the edit.
func destroyAdvice(bound string) string {
	return fmt.Sprintf(" To destroy the cluster, set `cluster_type` to %q, what it is bound to now, "+
		"then run `terraform destroy`. Or run `terraform destroy -refresh=false`.", bound)
}

// upgradePath returns the versions an upgrade from boundID to targetID passes
// through, in order and ending at the target, or a refusal saying why there is
// no such path. Only `available` versions are steps: the platform skips a draft
// or deprecated version rather than stopping at it.
func upgradePath(versions []k0rdent.ClusterTypeVersion, boundID, targetID string) ([]k0rdent.ClusterTypeVersion, string) {
	byID := map[string]k0rdent.ClusterTypeVersion{}
	for _, v := range versions {
		if v.VersionId != nil && v.Version != nil {
			byID[v.VersionId.String()] = v
		}
	}
	bound, ok := byID[boundID]
	if !ok {
		return nil, fmt.Sprintf("The platform reports the cluster bound to version id %q, which its "+
			"ClusterType does not list, so there is no telling whether %q is later.", boundID, targetID)
	}
	target, ok := byID[targetID]
	if !ok {
		return nil, fmt.Sprintf("`cluster_type` names version id %q, which is not a version of this "+
			"cluster's ClusterType. A cluster moves only between versions of the ClusterType it was "+
			"created from; naming another ClusterType replaces the cluster.", targetID)
	}
	from, to := *bound.Version, *target.Version
	switch {
	case to == from:
		return nil, ""
	case to < from:
		return nil, fmt.Sprintf("`cluster_type` names version %d and the platform reports version %d. "+
			"Upgrades are forward-only, so the cluster cannot be moved back, and replacing it would "+
			"destroy it to change a version. If the cluster was upgraded outside Terraform, set "+
			"`cluster_type` to the reported version. To rebuild it at an earlier version on purpose, "+
			"destroy it first, then apply.", to, from)
	case target.Status != k0rdent.ClusterTypeVersionStatusAvailable:
		return nil, fmt.Sprintf("Version %d is `%s`. Only an `available` version can be upgraded to.",
			to, target.Status)
	}

	var steps []k0rdent.ClusterTypeVersion
	for _, v := range byID {
		if n := *v.Version; n > from && n <= to && v.Status == k0rdent.ClusterTypeVersionStatusAvailable {
			steps = append(steps, v)
		}
	}
	sort.Slice(steps, func(i, j int) bool { return *steps[i].Version < *steps[j].Version })
	return steps, ""
}

func (m *Meta) listVersions(ctx context.Context, region, typeID string, diags *diag.Diagnostics) ([]k0rdent.ClusterTypeVersion, bool) {
	got, err := m.Client.ListClusterTypeVersionsWithResponse(ctx, region, typeID, nil)
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(diags, "list the cluster type's versions", httpResp, body, err)
		return nil, false
	}
	return got.JSON200.Versions, true
}

// versionBounds is one version on an upgrade route and the node pool bounds
// it declares.
type versionBounds struct {
	number int32
	pools  map[string]k0rdent.ClusterTypeNodePool
}

func (v versionBounds) fits(pool string, count int) bool {
	p, ok := v.pools[pool]
	if !ok {
		return true
	}
	return (p.NodeCountMin == nil || count >= int(*p.NodeCountMin)) &&
		(p.NodeCountMax == nil || count <= int(*p.NodeCountMax))
}

func (v versionBounds) allows(pool string) string {
	p := v.pools[pool]
	lo := int32(0)
	if p.NodeCountMin != nil {
		lo = *p.NodeCountMin
	}
	if p.NodeCountMax == nil {
		return fmt.Sprintf("version %d allows at least %d", v.number, lo)
	}
	return fmt.Sprintf("version %d allows %d to %d", v.number, lo, *p.NodeCountMax)
}

// upgradeRoute is the way from the bound version to the target: the steps in
// order, and the bounds of the bound version followed by those of each step.
type upgradeRoute struct {
	steps  []k0rdent.ClusterTypeVersion
	bounds []versionBounds
	// kubernetes is the Kubernetes version the route ends on, "" when that
	// version carries none.
	kubernetes string
}

// route reads what an upgrade from boundID to wantID passes through. A
// refusal says why there is no way there; ok is false when a read failed,
// which has already reported itself.
func (r *clusterResource) route(ctx context.Context, region, typeID, boundID, wantID string, diags *diag.Diagnostics) (upgradeRoute, string, bool) {
	versions, ok := r.meta.listVersions(ctx, region, typeID, diags)
	if !ok {
		return upgradeRoute{}, "", false
	}
	steps, refusal := upgradePath(versions, boundID, wantID)
	if refusal != "" || len(steps) == 0 {
		return upgradeRoute{}, refusal, true
	}
	boundUUID, err := uuid.Parse(boundID)
	if err != nil {
		return upgradeRoute{}, fmt.Sprintf("The bound version id %q is not a UUID.", boundID), true
	}
	ids := []openapi_types.UUID{boundUUID}
	for _, s := range steps {
		ids = append(ids, *s.VersionId)
	}
	out := upgradeRoute{steps: steps}
	for i, id := range ids {
		v, found := r.meta.readVersion(ctx, region, typeID, id, diags)
		if !found {
			return upgradeRoute{}, "", false
		}
		number := versionNumber(versions, id.String())
		if i > 0 && v.K8s == nil {
			// The platform resolves "next available" without regard to k8s and
			// then refuses a BMaaS-only target, so nothing past it is reachable.
			return upgradeRoute{}, fmt.Sprintf("Version %d is the next available version on the way and has "+
				"no Kubernetes configuration, so the platform cannot upgrade a Kubernetes cluster into it. "+
				"It does not skip such a version either, so no later version is reachable by upgrade until "+
				"version %d is deprecated on the platform.", number, number), true
		}
		if i == len(ids)-1 && v.K8s != nil {
			out.kubernetes = v.K8s.K0sVersion
		}
		vb := versionBounds{number: number, pools: map[string]k0rdent.ClusterTypeNodePool{}}
		for _, p := range v.NodePools {
			vb.pools[p.Id] = p
		}
		out.bounds = append(out.bounds, vb)
	}
	return out, "", true
}

// schedulePools decides when each node pool change happens along a route: at
// k, it is sent while the cluster is on bounds[k], so before step k+1.
//
// The platform checks a scale against the version the cluster is on and an
// upgrade against the version it moves to, both on the counts already
// settled. So a pool scaled at k needs its new count to fit bounds[k] and
// every later step, and its old count to fit every step before. The earliest
// k that works is taken. Pools are independent, so this is per pool. A pool
// that does not change has to fit every step as it is.
func schedulePools(have, want map[string]int, route []versionBounds) (map[string]int, string) {
	at := map[string]int{}
	for _, pool := range sortedKeys(have) {
		c := have[pool]
		n, managed := want[pool]
		if !managed {
			n = c
		}
		k, ok := earliestStep(pool, c, n, route)
		if !ok {
			var all []string
			for _, v := range route {
				all = append(all, v.allows(pool))
			}
			if c == n {
				return nil, fmt.Sprintf("Node pool %q has %d nodes, which the route does not allow throughout: "+
					"%s. The platform refuses an upgrade into a version whose bounds the current count "+
					"breaks. Set `node_pool[%q].node_count` to a count every version on the way allows.",
					pool, c, strings.Join(all, ", "), pool)
			}
			return nil, fmt.Sprintf("Node pool %q cannot go from %d to %d across this upgrade: %s. A scale "+
				"is checked against the version the cluster is on and an upgrade against the version it "+
				"moves to, and no order of the two keeps the pool within bounds throughout. Choose a count "+
				"every version on the way allows, or upgrade in smaller steps.",
				pool, c, n, strings.Join(all, ", "))
		}
		if c != n {
			at[pool] = k
		}
	}
	return at, ""
}

func earliestStep(pool string, c, n int, route []versionBounds) (int, bool) {
	for k := range route {
		if c != n && !route[k].fits(pool, n) {
			continue
		}
		ok := true
		for j := 1; j < len(route) && ok; j++ {
			count := n
			if j <= k {
				count = c
			}
			ok = route[j].fits(pool, count)
		}
		if ok {
			return k, true
		}
	}
	return 0, false
}

// planVersionChange decides what a `cluster_type` naming another version of
// the same ClusterType plans as: an in-place upgrade with its step count, or a
// refusal. With no configured client it cannot read the version list, so it
// plans the update and leaves the check to the apply, which makes it again.
func (r *clusterResource) planVersionChange(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	var configured types.String
	var prior clusterModel
	resp.Diagnostics.Append(resp.Plan.GetAttribute(ctx, path.Root("cluster_type"), &configured)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &prior)...)
	if resp.Diagnostics.HasError() || configured.IsNull() {
		return
	}
	if configured.IsUnknown() {
		// It may name another version, so the binding cannot be promised,
		// and nor can the Kubernetes version it carries.
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("cluster_type_resolved"), types.StringUnknown())...)
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("kubernetes_version"), types.StringUnknown())...)
		return
	}
	want, bound := configured.ValueString(), boundRef(prior)
	change, refusal := classifyRefChange(want, bound, k0rdent.ClusterState(prior.State.ValueString()))
	if change != refUpgrade && change != refRefused {
		return
	}
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("cluster_type_resolved"), types.StringUnknown())...)
	resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("kubernetes_version"), types.StringUnknown())...)

	attr := path.Root("cluster_type")
	if change == refRefused {
		// Local: a plan-time refusal, no request made.
		resp.Diagnostics.AddAttributeError(attr, versionRefusal, redactDiagnosticText(refusal+destroyAdvice(bound)))
		return
	}
	if r.meta == nil || r.meta.Client == nil {
		return
	}
	target, ok := resolveScope(r.meta, prior.Region, prior.Project, &resp.Diagnostics)
	if !ok {
		return
	}
	typeID, boundVersion := refTypeAndVersion(bound)
	_, wantVersion := refTypeAndVersion(want)
	rt, refusal, ok := r.route(ctx, target.region, typeID, boundVersion, wantVersion, &resp.Diagnostics)
	if !ok {
		return
	}
	if refusal == "" && len(rt.steps) > 0 {
		refusal, ok = r.planPools(ctx, target, prior, req, rt, &resp.Diagnostics)
		if !ok {
			return
		}
	}
	if refusal != "" {
		// Local: a plan-time refusal; the reads report their own failures.
		resp.Diagnostics.AddAttributeError(attr, versionRefusal, redactDiagnosticText(refusal+destroyAdvice(bound)))
		return
	}
	if len(rt.steps) == 0 {
		return
	}
	// The version the route ends on is the one the apply reads back, so the
	// plan can show what the upgrade does to Kubernetes rather than "known
	// after apply". A step that fails ends the apply in an error, which
	// Terraform doesn't hold to the plan.
	if rt.kubernetes != "" {
		resp.Diagnostics.Append(resp.Plan.SetAttribute(ctx, path.Root("kubernetes_version"), types.StringValue(rt.kubernetes))...)
	}
	numbers := make([]string, len(rt.steps))
	for i, s := range rt.steps {
		numbers[i] = fmt.Sprint(*s.Version)
	}
	unit := "steps"
	if len(rt.steps) == 1 {
		unit = "step"
	}
	resp.Diagnostics.AddAttributeWarning(attr,
		fmt.Sprintf("This plan upgrades the cluster in %d %s", len(rt.steps), unit),
		fmt.Sprintf("Through version %s, one upgrade per version, each waited out before the next. "+
			"If a step fails, the apply stops there and state records the version the cluster reached.",
			strings.Join(numbers, ", then ")))
}

// planPools checks every node pool against every step of the route, with the
// counts this plan sets, so a chain that would stop part way is refused before
// any of it runs. A plan whose counts are not known yet is left to the apply.
func (r *clusterResource) planPools(ctx context.Context, target clusterTarget, prior clusterModel, req resource.ModifyPlanRequest, rt upgradeRoute, diags *diag.Diagnostics) (string, bool) {
	var planned types.Map
	diags.Append(req.Plan.GetAttribute(ctx, path.Root("node_pool"), &planned)...)
	if diags.HasError() || planned.IsUnknown() || anyPoolCountUnknown(planned) {
		return "", !diags.HasError()
	}
	want, d := clusterPoolsFromModel(ctx, planned)
	diags.Append(d...)
	have, ok := r.settledCounts(ctx, target, prior.ID.ValueString(), diags)
	if !ok {
		return "", false
	}
	_, refusal := schedulePools(have, want, rt.bounds)
	return refusal, true
}

func anyPoolCountUnknown(pools types.Map) bool {
	for _, pool := range pools.Elements() {
		obj, ok := pool.(types.Object)
		if !ok || obj.IsUnknown() {
			return true
		}
		if count, ok := obj.Attributes()["node_count"].(types.Int64); ok && count.IsUnknown() {
			return true
		}
	}
	return false
}

// settledCounts is every node pool's settled count as the platform reports it.
func (r *clusterResource) settledCounts(ctx context.Context, target clusterTarget, id string, diags *diag.Diagnostics) (map[string]int, bool) {
	got, err := r.meta.Client.GetClusterWithResponse(ctx, target.region, target.project, id, nil)
	if err != nil || got.JSON200 == nil {
		var body []byte
		var httpResp *http.Response
		if got != nil {
			body, httpResp = got.Body, got.HTTPResponse
		}
		addAPIError(diags, "read the cluster", httpResp, body, err)
		return nil, false
	}
	out := map[string]int{}
	for _, p := range got.JSON200.NodePools {
		out[p.Id] = p.NodeCount
	}
	return out, true
}

// bindingMoves reports whether the planned `cluster_type` names another
// version than the one the cluster is bound to. A new spelling of the bound
// version, such as an import's unversioned reference written as the pin it
// resolved to, moves nothing. A value the plan doesn't know yet counts as a
// move.
func bindingMoves(ctx context.Context, plan tfsdk.Plan, state tfsdk.State, diags *diag.Diagnostics) bool {
	var configured types.String
	var prior clusterModel
	diags.Append(plan.GetAttribute(ctx, path.Root("cluster_type"), &configured)...)
	diags.Append(state.Get(ctx, &prior)...)
	if diags.HasError() || configured.IsUnknown() {
		return true
	}
	change, _ := classifyRefChange(configured.ValueString(), boundRef(prior), "")
	return change != refUnchanged
}

// boundRef is the version the platform last reported the cluster bound to.
func boundRef(m clusterModel) string {
	if v := m.ClusterTypeResolved.ValueString(); v != "" {
		return v
	}
	return m.ClusterType.ValueString()
}

// upgrade moves the cluster to the version plan pins, one upgradeCluster call
// per step, each naming its version so that a version published mid-chain
// cannot redirect it, and makes the plan's node count changes on the way, each
// at the point the bounds admit it. Every pass sets state from a fresh read,
// so a failure part way leaves state at the version the cluster reached.
func (r *clusterResource) upgrade(ctx context.Context, target clusterTarget, id string, plan clusterModel, st *tfsdk.State) diag.Diagnostics {
	var diags diag.Diagnostics
	want := plan.ClusterType.ValueString()
	wantPools, d := clusterPoolsFromModel(ctx, plan.NodePool)
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}
	if err := r.waitForSettled(ctx, target, id, "the cluster to settle before upgrading"); err != nil {
		addWaitError(&diags, "The cluster did not settle before the upgrade", err)
		return diags
	}

	attempted := ""
	busy := &upgradeBusy{}
	for {
		bound, have, ok := r.recordPass(ctx, target, id, plan, st, &diags)
		if !ok {
			return diags
		}
		if change, _ := classifyRefChange(want, bound, ""); change == refUnchanged {
			up, down := poolChanges(wantPools, have)
			diags.Append(r.scaleEach(ctx, target, id, up, down)...)
			return diags
		}

		typeID, boundVersion := refTypeAndVersion(bound)
		if boundVersion == attempted {
			// Local: the platform settled `active` without moving, and asking
			// again would loop.
			diags.AddError("The upgrade settled without changing the version",
				redactDiagnosticText(fmt.Sprintf("The platform reports the cluster `active` and still bound to %s after an "+
					"upgrade was accepted. Nothing was deleted.", bound)))
			return diags
		}
		rt, at, ok := r.replan(ctx, target, want, bound, have, wantPools, &diags)
		if !ok {
			return diags
		}
		took, again := r.takeStep(ctx, target, id, typeID, plan, st, rt, at, have, wantPools, busy, &diags)
		if !took {
			return diags
		}
		if again {
			// The step met the cluster busy with another run's operation and
			// waited it out. That operation may have moved the cluster, so the
			// next pass reads it and plans again, and the step isn't counted
			// as attempted.
			continue
		}
		attempted = boundVersion
	}
}

// recordPass reads the cluster at the start of a pass and sets state from it,
// so that whatever stops the upgrade leaves state at the version reached. It
// returns the reference the cluster is bound to and its node counts, and ok
// false when the upgrade stops here, having said why.
func (r *clusterResource) recordPass(ctx context.Context, target clusterTarget, id string, plan clusterModel, st *tfsdk.State, diags *diag.Diagnostics) (string, map[string]int, bool) {
	reached, found, d := r.read(ctx, target, id, recordedOver(ctx, plan, st))
	diags.Append(d...)
	if diags.HasError() {
		return "", nil, false
	}
	if !found {
		// Local: the read that 404d reported nothing, and the cluster is gone.
		diags.AddError("The cluster disappeared during the upgrade",
			fmt.Sprintf("Reading %q answered 404.", id))
		return "", nil, false
	}
	diags.Append(st.Set(ctx, &reached)...)
	have, d := clusterPoolsFromModel(ctx, reached.NodePool)
	diags.Append(d...)
	if diags.HasError() {
		return "", nil, false
	}
	return reached.ClusterTypeResolved.ValueString(), have, true
}

// replan makes the plan-time check again against the platform as it is now:
// the route from bound to want, and where each node pool change goes on it.
// It returns ok false when there is no way there, having said why.
func (r *clusterResource) replan(ctx context.Context, target clusterTarget, want, bound string, have, wantPools map[string]int, diags *diag.Diagnostics) (upgradeRoute, map[string]int, bool) {
	typeID, boundVersion := refTypeAndVersion(bound)
	_, wantVersion := refTypeAndVersion(want)
	rt, refusal, ok := r.route(ctx, target.region, typeID, boundVersion, wantVersion, diags)
	if !ok {
		return upgradeRoute{}, nil, false
	}
	var at map[string]int
	if refusal == "" && len(rt.steps) == 0 {
		refusal = fmt.Sprintf("`cluster_type` names %s, the version the cluster is on, under a "+
			"different origin than the platform's %s.", want, bound)
	}
	if refusal == "" {
		at, refusal = schedulePools(have, wantPools, rt.bounds)
	}
	if refusal != "" {
		// Local: the plan-time check, made again against the platform as it is now.
		diags.AddAttributeError(path.Root("cluster_type"), versionRefusal, redactDiagnosticText(refusal))
		return upgradeRoute{}, nil, false
	}
	return rt, at, true
}

// takeStep makes the node pool changes due before the route's first step,
// then takes that step and waits it out. It returns false when the upgrade
// stops here, having set state to the version reached and said why.
func (r *clusterResource) takeStep(ctx context.Context, target clusterTarget, id, typeID string, plan clusterModel, st *tfsdk.State, rt upgradeRoute, at, have, wantPools map[string]int, busy *upgradeBusy, diags *diag.Diagnostics) (took, again bool) {
	now := map[string]int{}
	for pool, k := range at {
		if k == 0 {
			now[pool] = wantPools[pool]
		}
	}
	up, down := poolChanges(now, have)
	if diags.Append(r.scaleEach(ctx, target, id, up, down)...); diags.HasError() {
		r.recordReached(ctx, target, id, plan, st)
		return false, false
	}

	from, to := rt.bounds[0].number, *rt.steps[0].Version
	accepted, again, waitErr := r.upgradeStep(ctx, target, id, typeID, from, rt.steps[0], busy, diags)
	if again {
		return true, true
	}
	if !accepted || waitErr != nil {
		failed := r.recordReached(ctx, target, id, plan, st)
		if waitErr != nil {
			upgradeWaitError(diags, waitErr, from, to, failed)
		}
		return false, false
	}
	return true, false
}

// recordReached sets state from a fresh read, bounded on its own because the
// wait may have spent the operation's deadline, and reports whether the
// cluster is `failed`. A read that fails leaves state at the last good one.
func (r *clusterResource) recordReached(ctx context.Context, target clusterTarget, id string, plan clusterModel, st *tfsdk.State) bool {
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	reached, found, d := r.read(readCtx, target, id, recordedOver(ctx, plan, st))
	if !found || d.HasError() || st.Set(ctx, &reached).HasError() {
		return false
	}
	return reached.State.ValueString() == string(k0rdent.ClusterStateFailed)
}

// recordedOver is plan with the binding state last recorded, as the prior an
// upgrade's reads take. The plan's binding is unknown and its
// `kubernetes_version` is where the upgrade is going, so neither says what the
// cluster was last seen bound to. State does, and a read that finds the
// binding unchanged keeps the Kubernetes version recorded with it rather than
// asking again.
func recordedOver(ctx context.Context, plan clusterModel, st *tfsdk.State) clusterModel {
	var recorded clusterModel
	if st.Get(ctx, &recorded).HasError() {
		return plan
	}
	plan.ClusterTypeResolved, plan.KubernetesVersion = recorded.ClusterTypeResolved, recorded.KubernetesVersion
	return plan
}

func upgradeWaitError(diags *diag.Diagnostics, waitErr error, from, to int32, failed bool) {
	if failed {
		addAdvisedError(diags, fmt.Sprintf("The cluster is failed at version %d of its ClusterType", from),
			fmt.Sprintf("The upgrade to version %d did not complete, and the platform reports the cluster "+
				"`failed` and still bound to version %d, which state now records. The platform does not "+
				"retry an upgrade from `failed`, so applying again cannot continue until the cluster is "+
				"recovered to `active` on the platform. Nothing was deleted.", to, from), waitErr)
		return
	}
	advice := fmt.Sprintf("State records the version the platform reports, version %d unless the upgrade has "+
		"since finished. Nothing was deleted.", from)
	if errors.Is(waitErr, errWaitTimeout) {
		advice += " " + changeKeepsRunning
	}
	addAdvisedError(diags, fmt.Sprintf("The upgrade to version %d did not settle", to), advice, waitErr)
}

// upgradeStep makes one upgradeCluster call and waits it out. It reports
// whether the platform accepted the call, and the wait's error if it did.
// again says the platform refused the call because the cluster was busy, and
// that has been waited out, so the step is to be planned again.
func (r *clusterResource) upgradeStep(ctx context.Context, target clusterTarget, id, typeID string, from int32, next k0rdent.ClusterTypeVersion, busy *upgradeBusy, diags *diag.Diagnostics) (accepted, again bool, waitErr error) {
	up, err := r.meta.Client.UpgradeClusterWithResponse(ctx, target.region, target.project, id,
		k0rdent.UpgradeClusterRequest{ClusterTypeVersionId: next.VersionId})
	if err != nil {
		addAPIError(diags, "upgrade the cluster", nil, nil, err)
		return false, false, nil
	}
	if up.JSON200 == nil {
		again, state, busyErr := r.afterRefusedUpgrade(ctx, target, id, up, busy)
		switch {
		case again:
			return false, true, nil
		case busyErr != nil:
			// The platform's refusal, then what the wait made of it, as
			// busyRetry.report words a busy write whose wait ended.
			var refused diag.Diagnostics
			addAPIError(&refused, "upgrade the cluster", up.HTTPResponse, up.Body, nil)
			advice := fmt.Sprintf("The platform refused the upgrade to version %d because the cluster was "+
				"`%s`, busy with another operation, so this apply waited for it to finish, and the update "+
				"timeout ran out first. This step changed nothing: the cluster is at version %d, which state "+
				"records. Nothing was deleted. Apply again once the cluster reads `active`, or raise "+
				"`timeouts.update`.", *next.Version, state, from)
			for _, d := range refused.Errors() {
				diags.AddError(d.Summary(), d.Detail()+"\n\n"+advice+"\n\n"+
					redactDiagnosticText(busyErr.Error()+requestIDLine(requestIDIn(busyErr))))
			}
			return false, false, nil
		}
		r.upgradeRefused(ctx, target, id, typeID, from, next, up, diags)
		return false, false, nil
	}
	return true, false, r.waitForSettled(ctx, target, id, fmt.Sprintf("the cluster to finish upgrading to version %d", *next.Version))
}

// upgradeBusy is what one upgrade has done about refusals from a busy
// cluster, so it lives as long as one apply's upgrade.
type upgradeBusy struct {
	// waits is how many operations it has waited out.
	waits int
	// resentSettled says a refusal from a cluster that read `active` has
	// already been sent once more, which happens at most once.
	resentSettled bool
}

// afterRefusedUpgrade reads a refused upgrade the way busyRetry reads a
// write refused against a busy dependency, here the cluster itself.
//
// The platform admits an upgrade only from `active`, and refuses one from
// `creating`, `updating` or `deleting` with a 409 that carries no reason.
// The cluster resource waits for the cluster to settle before the first step
// and after each, so a refusal here means another run started an operation in
// the moment between. The cluster is read: while it's `creating` or
// `updating`, the upgrade waits for it to leave that state, within the update
// timeout, pausing longer before each wait after the first, and reports
// again so the caller plans the step from a fresh read. That operation may
// itself have moved the version. A cluster that already reads `active` may
// have settled between the refusal and the read, so the step is sent once
// more, once. `failed` and `deleting` don't clear by waiting, and the 409
// for a project in another region is about the address, so those are the
// answer. state is the state waited on, for the report when the wait ends.
func (r *clusterResource) afterRefusedUpgrade(ctx context.Context, target clusterTarget, id string, up *k0rdent.UpgradeClusterResponse, busy *upgradeBusy) (again bool, state k0rdent.ClusterState, err error) {
	if up.HTTPResponse == nil || up.HTTPResponse.StatusCode != http.StatusConflict {
		return false, "", nil
	}
	var refusal *k0rdent.APIError
	if errors.As(k0rdent.CheckResponse(up.HTTPResponse, up.Body), &refusal) && refusal.Reason == "PROJECT_REGION_MISMATCH" {
		return false, "", nil
	}
	now, readErr := r.clusterNow(ctx, target, id)
	if readErr != nil {
		tflog.Debug(ctx, "could not read the cluster after a refused upgrade step, so the refusal stands",
			map[string]any{"cluster": id, "error": redactDiagnosticText(readErr.Error())})
		return false, "", nil //nolint:nilerr // the refusal is what's reported, as busyDependency does
	}
	if !now.found {
		return false, "", nil
	}
	pause := func(n int) error {
		return sleepOrCancel(ctx, fullJitter(backoffCeiling(r.meta.Wait, n, time.Minute)))
	}
	switch {
	case now.state == k0rdent.ClusterStateActive:
		if busy.resentSettled {
			return false, "", nil
		}
		if err := pause(0); err != nil {
			return false, now.state, err
		}
		busy.resentSettled = true
		tflog.Info(ctx, "the platform refused the upgrade step, and the cluster already reads active, "+
			"so it's sent once more", withRequestID(map[string]any{"cluster": id}, up.HTTPResponse, up.Body))
		return true, "", nil
	case cancellable(now.state):
		if busy.waits > 0 {
			if err := pause(busy.waits - 1); err != nil {
				return false, now.state, err
			}
		}
		busy.waits++
		tflog.Info(ctx, "the platform refused the upgrade step while the cluster runs another operation, "+
			"so it's planned again once that finishes", withRequestID(map[string]any{
			"cluster": id, "state": string(now.state), "execution": now.execution, "waits": busy.waits,
		}, up.HTTPResponse, up.Body))
		err := waitFor(ctx, r.meta.Wait, "the cluster's other operation to finish", func(ctx context.Context) (bool, error) {
			after, readErr := r.clusterNow(ctx, target, id)
			if readErr != nil || !after.found {
				return !after.found, readErr
			}
			return probeVerdict(settledAfterCancel(after.state))
		})
		if err != nil {
			return false, now.state, err
		}
		return true, "", nil
	}
	return false, "", nil
}

// upgradeRefused turns a refused upgradeCluster call into a diagnostic that
// says what the refusal means for this cluster and what to do about it. Earlier
// steps of the same apply may have changed the cluster, so it says only that
// this step changed nothing, and names the version the cluster is at.
func (r *clusterResource) upgradeRefused(ctx context.Context, target clusterTarget, id, typeID string, from int32, next k0rdent.ClusterTypeVersion, up *k0rdent.UpgradeClusterResponse, diags *diag.Diagnostics) {
	checked := k0rdent.CheckResponse(up.HTTPResponse, up.Body)
	var apiErr *k0rdent.APIError
	if !errors.As(checked, &apiErr) {
		addAPIError(diags, "upgrade the cluster", up.HTTPResponse, up.Body, nil)
		return
	}
	to := *next.Version
	unchanged := fmt.Sprintf("This step changed nothing: the cluster is at version %d, which state records.", from)
	switch apiErr.StatusCode {
	case http.StatusNotFound:
		if !serviceSaysMissing(apiErr) {
			// It never reached the service, so it says nothing about the
			// version. apiErrorDetail names what answered.
			addAdvisedError(diags, "Could not upgrade the cluster", unchanged, checked)
			return
		}
		addAdvisedError(diags, fmt.Sprintf("The platform does not find version %d under this cluster's ClusterType", to),
			"The version may have been withdrawn since the plan, so plan again. "+unchanged, checked)
	case http.StatusConflict:
		addAdvisedError(diags, "The cluster cannot be upgraded in its current state",
			"The platform admits an upgrade only from `active`. A cluster that is `creating`, "+
				"`updating` or `deleting` is busy with another change, so apply again once it settles. "+
				"A `failed` cluster is not retried through an upgrade and has to be recovered on the "+
				"platform first. "+unchanged+" Nothing was deleted.", checked)
	case http.StatusUnprocessableEntity:
		if pool, count, allows, ok := r.outsideBounds(ctx, target, id, typeID, to, next); ok {
			addAdvisedError(diags, fmt.Sprintf("Upgrading to version %d would put a node pool outside its bounds", to),
				fmt.Sprintf("Node pool %q has %d nodes and %s. The platform refuses an upgrade the "+
					"cluster's current node counts do not fit. Set `node_pool[%q].node_count` to a count "+
					"every version on the way allows and apply again. %s", pool, count, allows, pool, unchanged), checked)
			return
		}
		addAdvisedError(diags, fmt.Sprintf("The platform refused the upgrade to version %d", to),
			"An upgrade must name the cluster's next available version of the same ClusterType. The "+
				"version list may have changed since the plan, so plan again. "+unchanged, checked)
	case http.StatusGatewayTimeout:
		addAdvisedError(diags, fmt.Sprintf("The upgrade to version %d was staged but did not start", to),
			fmt.Sprintf("The platform recorded the target and could not start the upgrade workflow. The "+
				"cluster stays `updating` until the platform's reconciler settles it, so this provider "+
				"stopped rather than wait on a workflow that may not exist. Nothing was deleted, and state "+
				"records version %d, the version the cluster is bound to. Apply again once the cluster "+
				"reads `active`.", from), checked)
	default:
		addAPIError(diags, "upgrade the cluster", up.HTTPResponse, up.Body, nil)
	}
}

// outsideBounds finds a node pool whose settled count the target version's
// bounds do not admit, which is the one 422 a user can fix from configuration.
func (r *clusterResource) outsideBounds(ctx context.Context, target clusterTarget, id, typeID string, to int32, next k0rdent.ClusterTypeVersion) (pool string, count int, allows string, ok bool) {
	var ignored diag.Diagnostics
	version, found := r.meta.readVersion(ctx, target.region, typeID, *next.VersionId, &ignored)
	if !found {
		return "", 0, "", false
	}
	have, found := r.settledCounts(ctx, target, id, &ignored)
	if !found {
		return "", 0, "", false
	}
	vb := versionBounds{number: to, pools: map[string]k0rdent.ClusterTypeNodePool{}}
	for _, p := range version.NodePools {
		vb.pools[p.Id] = p
	}
	for _, pool := range sortedKeys(have) {
		if !vb.fits(pool, have[pool]) {
			return pool, have[pool], vb.allows(pool), true
		}
	}
	return "", 0, "", false
}

func versionNumber(versions []k0rdent.ClusterTypeVersion, id string) int32 {
	for _, v := range versions {
		if v.VersionId != nil && v.VersionId.String() == id && v.Version != nil {
			return *v.Version
		}
	}
	return 0
}
