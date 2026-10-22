package provider

import (
	"context"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// planNetworkUpdate runs modifyScopePlan for a network in project "imported",
// with project in its configuration set to configured, or left out when that's
// empty. The provider block's project is "p".
func planNetworkUpdate(t *testing.T, configured string) resource.ModifyPlanResponse {
	t.Helper()
	meta := mismatchMeta(t, &mismatchAPI{home: "eu", hidden: true})
	r := &networkResource{meta: meta}
	set := map[string]tftypes.Value{"id": str("n")}
	if configured != "" {
		set["project"] = str(configured)
	}
	configRaw, s := testValue(t, r, set)
	stateRaw, _ := testValue(t, r, map[string]tftypes.Value{"id": str("n"), "region": str("eu"), "project": str("imported")})
	plan := tfsdk.Plan{Raw: configRaw, Schema: s.Schema}
	resp := resource.ModifyPlanResponse{Plan: plan}
	modifyScopePlan(context.Background(), meta, resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: configRaw, Schema: s.Schema},
		Plan:   plan,
		State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
	}, &resp)
	return resp
}

// A configuration that leaves the project to the provider block moves an
// imported resource, and the plan says how to keep it.
func TestAPlanThatMovesAnImportSaysToRepeatTheScope(t *testing.T) {
	resp := planNetworkUpdate(t, "")
	if len(resp.RequiresReplace) != 1 {
		t.Fatalf("RequiresReplace = %v, want project", resp.RequiresReplace)
	}
	w := resp.Diagnostics.Warnings()
	if len(w) != 1 || !strings.Contains(w[0].Detail(), `project = "imported"`) {
		t.Errorf("warnings = %v, want one naming project = \"imported\"", w)
	}
}

// A configuration that sets the project, as local.project from a resource
// does, plans no move and no warning.
func TestAConfigurationThatSetsTheScopeIsNotWarned(t *testing.T) {
	if resp := planNetworkUpdate(t, "imported"); len(resp.Diagnostics) != 0 || len(resp.RequiresReplace) != 0 {
		t.Errorf("diagnostics = %v, replace = %v, want neither", resp.Diagnostics, resp.RequiresReplace)
	}
}
