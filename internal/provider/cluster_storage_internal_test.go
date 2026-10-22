package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// v1.3.0-dev.4 lets a cluster declare a storage ceiling, charged against the
// storage_gib quota. It can be raised and never lowered: a lower value is a
// 422. These pin how the cluster resource plans, sends and reads it.

func withCeiling(v map[string]tftypes.Value, gib int) map[string]tftypes.Value {
	v["storage_ceiling_gib"] = tftypes.NewValue(tftypes.Number, gib)
	return v
}

// runRaise applies a plan that changes only the storage ceiling.
func runRaise(t *testing.T, api *upgradeAPI, to int) resource.UpdateResponse {
	t.Helper()
	server := api.serve(t)
	r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	stateRaw, s := testValue(t, r, withCeiling(clusterValues(verRef(api.bound), verRef(api.bound), api.workers, api.state), api.ceiling))
	planRaw, _ := testValue(t, r, withCeiling(clusterValues(verRef(api.bound), "", api.workers, ""), to))
	resp := resource.UpdateResponse{State: tfsdk.State{Raw: planRaw, Schema: s.Schema}}
	r.Update(context.Background(), resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}, &resp)
	return resp
}

func TestARaisedStorageCeilingIsSentAndReadBack(t *testing.T) {
	api := &upgradeAPI{bound: 1, workers: 2, state: "active", ceiling: 100}
	resp := runRaise(t, api, 250)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if got := strings.Join(api.calls, " "); got != "storage 250" {
		t.Errorf("calls = %q, want one raise to 250", got)
	}
	var got types.Int64
	resp.State.GetAttribute(context.Background(), path.Root("storage_ceiling_gib"), &got)
	if got.ValueInt64() != 250 {
		t.Errorf("storage_ceiling_gib = %s", got)
	}
}

// A raise on a cluster that isn't active, or whose quota accounting is still
// settling, is a 409 with no reason that "can be retried shortly". The raise
// waits for the cluster to settle and is sent again.
func TestARaiseRefusedWhileTheClusterIsBusyIsSentAgain(t *testing.T) {
	api := &upgradeAPI{bound: 1, workers: 2, state: "active", ceiling: 100, busyRaises: 1}
	resp := runRaise(t, api, 250)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if got := strings.Join(api.calls, " "); got != "storage 250 storage 250" {
		t.Errorf("calls = %q, want the raise sent twice", got)
	}
	if api.ceiling != 250 {
		t.Errorf("ceiling = %d", api.ceiling)
	}
}

// A raise that keeps being refused isn't chased past a few tries: it stops
// with the platform's answer.
func TestARaiseRefusedOverAndOverStops(t *testing.T) {
	api := &upgradeAPI{bound: 1, workers: 2, state: "active", ceiling: 100, busyRaises: 10}
	resp := runRaise(t, api, 250)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a raise refused every time reported success")
	}
	if n := strings.Count(strings.Join(api.calls, " "), "storage 250"); n != maxStorageRaises {
		t.Errorf("sent %d raises, want %d", n, maxStorageRaises)
	}
	text := resp.Diagnostics.Errors()[0].Detail()
	if !strings.Contains(text, "409") || !strings.Contains(text, "storage ceiling") {
		t.Errorf("diagnostic:\n%s", text)
	}
}

// planCeiling runs the attribute's plan modifiers for one change.
func planCeiling(t *testing.T, state, config types.Int64) planmodifier.Int64Response {
	t.Helper()
	var s resource.SchemaResponse
	(&clusterResource{}).Schema(context.Background(), resource.SchemaRequest{}, &s)
	attr, ok := s.Schema.Attributes["storage_ceiling_gib"].(schema.Int64Attribute)
	if !ok {
		t.Fatal("storage_ceiling_gib is not an Int64Attribute")
	}
	plan := config
	if config.IsNull() {
		plan = types.Int64Unknown()
	}
	// An update, so the resource has a prior state.
	stateRaw, _ := testValue(t, &clusterResource{}, clusterValues(verRef(1), verRef(1), 2, "active"))
	resp := planmodifier.Int64Response{PlanValue: plan}
	for _, m := range attr.PlanModifiers {
		m.PlanModifyInt64(context.Background(), planmodifier.Int64Request{
			Path: path.Root("storage_ceiling_gib"), StateValue: state, ConfigValue: config, PlanValue: resp.PlanValue,
			State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		}, &resp)
	}
	return resp
}

func TestLoweringTheStorageCeilingIsRefusedAtPlan(t *testing.T) {
	resp := planCeiling(t, types.Int64Value(500), types.Int64Value(100))
	if !resp.Diagnostics.HasError() {
		t.Fatal("lowering the ceiling planned")
	}
	text := resp.Diagnostics.Errors()[0].Summary() + "\n" + resp.Diagnostics.Errors()[0].Detail()
	for _, want := range []string{"lowered", "500", "100"} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostic does not say %q:\n%s", want, text)
		}
	}

	if raised := planCeiling(t, types.Int64Value(500), types.Int64Value(800)); raised.Diagnostics.HasError() {
		t.Errorf("raising refused: %v", raised.Diagnostics)
	}
	if first := planCeiling(t, types.Int64Null(), types.Int64Value(800)); first.Diagnostics.HasError() {
		t.Errorf("a first ceiling refused: %v", first.Diagnostics)
	}
	// Left out of configuration, the ceiling the cluster has stays: nothing
	// can lower or clear it.
	resp = planCeiling(t, types.Int64Value(500), types.Int64Null())
	if resp.Diagnostics.HasError() || resp.PlanValue.ValueInt64() != 500 {
		t.Errorf("removed from configuration: plan %s, %v", resp.PlanValue, resp.Diagnostics)
	}
}

func TestTheStorageCeilingIsReadFromTheCluster(t *testing.T) {
	prior := clusterModel{Tags: types.MapNull(types.StringType), NodePool: types.MapNull(clusterNodePoolObjectType()),
		StorageCeilingGiB: types.Int64Null()}
	got, d := clusterStateFrom(context.Background(), k0rdent.Cluster{Id: "c",
		Storage: &k0rdent.ClusterStorage{CeilingGiB: 300}}, prior, defaultTags{})
	if d.HasError() || got.StorageCeilingGiB.ValueInt64() != 300 {
		t.Errorf("storage_ceiling_gib = %s (%v)", got.StorageCeilingGiB, d)
	}
	got, d = clusterStateFrom(context.Background(), k0rdent.Cluster{Id: "c"}, prior, defaultTags{})
	if d.HasError() || !got.StorageCeilingGiB.IsNull() {
		t.Errorf("a cluster that declares no storage: %s (%v)", got.StorageCeilingGiB, d)
	}
}

// A ceiling that was unknown at plan, say from another resource's output,
// gets past the plan-time refusal. Lower than the cluster's at apply, it's
// refused there with the same words, before anything is sent, rather than
// sending nothing and failing on an inconsistent result.
func TestALowerCeilingKnownOnlyAtApplyIsRefusedBeforeAnythingIsSent(t *testing.T) {
	api := &upgradeAPI{bound: 1, workers: 2, state: "active", ceiling: 500}
	resp := runRaise(t, api, 100)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a lower ceiling applied")
	}
	text := resp.Diagnostics.Errors()[0].Summary() + "\n" + resp.Diagnostics.Errors()[0].Detail()
	for _, want := range []string{"can't be lowered", "500", "100"} {
		if !strings.Contains(text, want) {
			t.Errorf("diagnostic does not say %q:\n%s", want, text)
		}
	}
	if len(api.calls) != 0 {
		t.Errorf("sent %q, want nothing", api.calls)
	}
}
