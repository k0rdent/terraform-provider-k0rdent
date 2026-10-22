package provider

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

func decodeInto[T any](t *testing.T, body string) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return out
}

func filledCluster(t *testing.T, body string) clusterDataSourceModel {
	t.Helper()
	var data clusterDataSourceModel
	if diags := data.fill(context.Background(), decodeInto[k0rdent.Cluster](t, body)); diags.HasError() {
		t.Fatal(diags)
	}
	return data
}

func TestClusterDataKeepsSecurityGroupOrder(t *testing.T) {
	data := filledCluster(t, `{"id":"c","uid":"00000000-0000-4000-8000-000000000001","selfLink":"/c",
		"nodePools":[],"securityGroups":["sg-b","sg-a","sg-c"]}`)

	got := make([]string, 0, len(data.SecGroups))
	for _, g := range data.SecGroups {
		got = append(got, g.ValueString())
	}
	want := []string{"sg-b", "sg-a", "sg-c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v: the list reaches the fabric in this order", got, want)
		}
	}
}

func TestClusterDataReadsTheHostedControlPlaneJudgement(t *testing.T) {
	data := filledCluster(t, `{"id":"c","uid":"00000000-0000-4000-8000-000000000001","selfLink":"/c",
		"nodePools":[],"securityGroups":[],
		"controlPlane":{"hosted":{
			"state":"degraded","message":"2 of 3 control-plane replicas are ready",
			"version":"v1.35.1+k0s.0","desiredReplicas":3,"readyReplicas":2,
			"observedAt":"2026-09-21T00:00:00Z",
			"ready":true,"initialized":true,"replicas":3,"updatedReplicas":3,"unavailableReplicas":1}}}`)

	checks := map[string][2]string{
		"control_plane_state":            {data.ControlPlaneState.ValueString(), "degraded"},
		"control_plane_message":          {data.ControlPlaneMessage.ValueString(), "2 of 3 control-plane replicas are ready"},
		"control_plane_version":          {data.ControlPlaneVersion.ValueString(), "v1.35.1+k0s.0"},
		"control_plane_desired_replicas": {data.ControlPlaneDesired.String(), "3"},
		"control_plane_ready_replicas":   {data.ControlPlaneReadyCount.String(), "2"},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
}

func TestClusterDataReadsAnUpgradeInFlight(t *testing.T) {
	data := filledCluster(t, `{"id":"c","uid":"00000000-0000-4000-8000-000000000001","selfLink":"/c",
		"nodePools":[],"securityGroups":[],"state":"updating",
		"targetClusterTypeVersionId":"33333333-3333-3333-3333-333333333333",
		"operation":{"executionId":"exec-7","status":"running","message":"applying the new config",
			"startedAt":"2026-09-21T10:15:00+02:00",
			"progress":{"completed":1,"total":4,"percentComplete":25,"current":"applying-config",
				"counts":{"done":1,"inProgress":1,"pending":2,"failed":0,"skipped":0}}}}`)

	if got := data.UpgradeTargetVersionID.ValueString(); got != "33333333-3333-3333-3333-333333333333" {
		t.Errorf("upgrade_target_version_id = %q", got)
	}
	op := data.Operation
	if op == nil {
		t.Fatal("operation is null on a cluster that reports one")
	}
	checks := map[string][2]string{
		"status":           {op.Status.ValueString(), "running"},
		"execution_id":     {op.ExecutionID.ValueString(), "exec-7"},
		"message":          {op.Message.ValueString(), "applying the new config"},
		"started_at":       {op.StartedAt.ValueString(), "2026-09-21T08:15:00Z"},
		"current_stage":    {op.CurrentStage.ValueString(), "applying-config"},
		"percent_complete": {op.PercentComplete.String(), "25"},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("operation.%s = %q, want %q", name, c[0], c[1])
		}
	}
	if !op.CompletedAt.IsNull() {
		t.Errorf("operation.completed_at = %s, want null while running", op.CompletedAt)
	}
}

func TestClusterDataLeavesWhatIsAbsentNull(t *testing.T) {
	data := filledCluster(t, `{"id":"c","uid":"00000000-0000-4000-8000-000000000001","selfLink":"/c",
		"nodePools":[],"securityGroups":[],"state":"failed","stateReason":"TIMEOUT"}`)

	if got := data.StateReason.ValueString(); got != "TIMEOUT" {
		t.Errorf("state_reason = %q, want TIMEOUT", got)
	}
	if data.Operation != nil {
		t.Errorf("operation = %+v, want null on a cluster that never ran one", data.Operation)
	}
	for name, v := range map[string]interface{ IsNull() bool }{
		"upgrade_target_version_id":      data.UpgradeTargetVersionID,
		"control_plane_state":            data.ControlPlaneState,
		"control_plane_message":          data.ControlPlaneMessage,
		"control_plane_desired_replicas": data.ControlPlaneDesired,
		"control_plane_ready_replicas":   data.ControlPlaneReadyCount,
		"api_endpoint":                   data.APIEndpoint,
	} {
		if !v.IsNull() {
			t.Errorf("%s is %v, want null", name, v)
		}
	}
}

// A progress block only arrives with includeStages, and an operation without
// one must not read as 0% done.
func TestClusterDataOperationWithoutProgressIsNotZeroPercent(t *testing.T) {
	data := filledCluster(t, `{"id":"c","uid":"00000000-0000-4000-8000-000000000001","selfLink":"/c",
		"nodePools":[],"securityGroups":[],
		"operation":{"executionId":"exec-1","status":"succeeded",
			"startedAt":"2026-09-21T00:00:00Z","completedAt":"2026-09-21T00:20:00Z"}}`)

	if data.Operation == nil {
		t.Fatal("operation is null")
	}
	if !data.Operation.PercentComplete.IsNull() || !data.Operation.CurrentStage.IsNull() {
		t.Errorf("progress = %s / %s, want null without a progress block",
			data.Operation.PercentComplete, data.Operation.CurrentStage)
	}
	if got := data.Operation.CompletedAt; got != types.StringValue("2026-09-21T00:20:00Z") {
		t.Errorf("completed_at = %s", got)
	}
}
