package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

func TestTheAdoptPlanKeepsRefreshedValues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const bound = "/v1/regions/test/compute/cluster-types/bound"
	prior := testClusterState(t, "c1", bound)
	input := testClusterState(t, "c1", "/v1/regions/test/compute/cluster-types/configured")
	put := func(row *tfsdk.State, name string, value any) {
		t.Helper()
		if d := row.SetAttribute(ctx, path.Root(name), value); d.HasError() {
			t.Fatal(d)
		}
	}
	put(&prior, "display_name", types.StringValue("from the platform"))
	put(&prior, "state", types.StringValue("creating"))
	put(&prior, "storage_ceiling_gib", types.Int64Value(100))
	put(&input, "storage_ceiling_gib", types.Int64Value(50))
	put(&input, adoptOnlyAttribute, types.BoolValue(true))
	resp := resource.ModifyPlanResponse{Plan: tfsdk.Plan(input), RequiresReplace: []path.Path{path.Root("cluster_type"), path.Root("region")}}
	planAdoptedCluster(ctx, resource.ModifyPlanRequest{State: prior, Config: tfsdk.Config(input)}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics.Errors())
	}
	containsAll(t, "adopt warnings", diagText(resp.Diagnostics), "keeps the platform value for `cluster_type`", "keeps the platform value for `storage_ceiling_gib`")
	if strings.Contains(diagText(resp.Diagnostics), "`display_name`") {
		t.Error("the plan warned about a field absent from configuration")
	}
	var got clusterModel
	if d := resp.Plan.Get(ctx, &got); d.HasError() {
		t.Fatal(d)
	}
	if got.ClusterType.ValueString() != bound || got.DisplayName.ValueString() != "from the platform" ||
		got.StorageCeilingGiB.ValueInt64() != 100 || got.State.ValueString() != "creating" || !got.AdoptOnly.ValueBool() {
		t.Errorf("planned cluster didn't keep the observed values: %+v", got)
	}
	if len(resp.RequiresReplace) != 1 || !resp.RequiresReplace[0].Equal(path.Root("region")) {
		t.Errorf("replacement paths = %v, want only region", resp.RequiresReplace)
	}
}

func TestTheAdoptPlanRefusesAnUnrepresentableNull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	prior := testClusterState(t, "c1", "/v1/regions/test/compute/cluster-types/test")
	input := prior
	if d := input.SetAttribute(ctx, path.Root("description"), types.StringValue("present only in configuration")); d.HasError() {
		t.Fatal(d)
	}
	resp := resource.ModifyPlanResponse{Plan: tfsdk.Plan(input)}
	planAdoptedCluster(ctx, resource.ModifyPlanRequest{State: prior, Config: tfsdk.Config(input)}, &resp)
	if !resp.Diagnostics.HasError() {
		t.Fatal("the plan tried to return null for a non-null configured value without a non-null prior value")
	}
	containsAll(t, "null refusal", diagText(resp.Diagnostics), "The platform doesn't report `description`", "Omit `description` for the adopt apply")
}

func TestOnlyNormalPlansReplaceForADifferentClusterType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const oldRef = "/v1/regions/test/compute/cluster-types/original"
	const newRef = "/v1/regions/test/compute/cluster-types/different"
	for _, adopt := range []bool{false, true} {
		prior := testClusterState(t, "c1", oldRef)
		input := testClusterState(t, "c1", newRef)
		if d := input.SetAttribute(ctx, path.Root(adoptOnlyAttribute), types.BoolValue(adopt)); d.HasError() {
			t.Fatal(d)
		}
		req := planmodifier.StringRequest{Config: tfsdk.Config(input), State: prior, Plan: tfsdk.Plan(input),
			ConfigValue: types.StringValue(newRef), StateValue: types.StringValue(oldRef), PlanValue: types.StringValue(newRef)}
		resp := planmodifier.StringResponse{PlanValue: req.PlanValue}
		clusterTypeVersionGuard().PlanModifyString(ctx, req, &resp)
		if resp.Diagnostics.HasError() || resp.RequiresReplace == adopt {
			t.Errorf("adopt_only = %v: requires replacement %v, diagnostics %v", adopt, resp.RequiresReplace, resp.Diagnostics)
		}
	}
}

func TestOnlyNormalPlansRefuseALowerStorageCeiling(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, adopt := range []bool{false, true} {
		cfg := testClusterState(t, "c1", "/v1/regions/test/compute/cluster-types/test")
		if d := cfg.SetAttribute(ctx, path.Root(adoptOnlyAttribute), types.BoolValue(adopt)); d.HasError() {
			t.Fatal(d)
		}
		req := planmodifier.Int64Request{Config: tfsdk.Config(cfg), StateValue: types.Int64Value(100), PlanValue: types.Int64Value(50)}
		resp := planmodifier.Int64Response{PlanValue: req.PlanValue}
		neverLowered{}.PlanModifyInt64(ctx, req, &resp)
		if resp.Diagnostics.HasError() == adopt {
			t.Errorf("adopt_only = %v: got %v", adopt, resp.Diagnostics)
		}
	}
}
