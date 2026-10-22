package provider

import (
	"context"
	"testing"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

func filledInstanceGroup(t *testing.T, body string) instanceGroupDataSourceModel {
	t.Helper()
	var data instanceGroupDataSourceModel
	if diags := data.fill(context.Background(), decodeInto[k0rdent.InstanceGroup](t, body)); diags.HasError() {
		t.Fatal(diags.Errors())
	}
	return data
}

func TestInstanceGroupDataKeepsSecurityGroupOrder(t *testing.T) {
	data := filledInstanceGroup(t, `{"id":"g","clusterType":"/ct","securityGroups":["sg-b","sg-a"]}`)

	if len(data.SecGroups) != 2 || data.SecGroups[0].ValueString() != "sg-b" || data.SecGroups[1].ValueString() != "sg-a" {
		t.Errorf("security_groups = %v, want [sg-b sg-a]: the list reaches the fabric in this order", data.SecGroups)
	}
}

func TestInstanceGroupDataReadsTheStableReason(t *testing.T) {
	data := filledInstanceGroup(t, `{"id":"g","clusterType":"/ct","securityGroups":[],
		"state":"active","stateReason":"INSUFFICIENT_CAPACITY","statusReason":"free text",
		"progressUpdatedAt":"2026-09-21T10:15:00+02:00"}`)

	if got := data.StateReason.ValueString(); got != "INSUFFICIENT_CAPACITY" {
		t.Errorf("state_reason = %q, want the stable code rather than the free text", got)
	}
	if got := data.ProgressUpdatedAt.ValueString(); got != "2026-09-21T08:15:00Z" {
		t.Errorf("progress_updated_at = %q", got)
	}
}

func TestInstanceGroupDataLeavesWhatIsAbsentNull(t *testing.T) {
	data := filledInstanceGroup(t, `{"id":"g","clusterType":"/ct","securityGroups":[]}`)

	if !data.StateReason.IsNull() || !data.ProgressUpdatedAt.IsNull() || !data.ExecutionID.IsNull() {
		t.Errorf("state_reason %s, progress_updated_at %s, execution_id %s: want all null",
			data.StateReason, data.ProgressUpdatedAt, data.ExecutionID)
	}
}

// The data source reads what the platform echoes: the users and, inside each,
// the keys in the order they were sent. A group with none reads null.
func TestInstanceGroupDataReadsTheSSHAccess(t *testing.T) {
	data := filledInstanceGroup(t, `{"id":"g","clusterType":"/ct","securityGroups":[],
		"sshAccess":[{"username":"adam","sshPublicKeys":["abe"],"displayName":"Adam"},
		{"username":"zoe","sshPublicKeys":["zed","abe"],"sudo":"ALL=(ALL) NOPASSWD:ALL"}]}`)

	var users []sshUserModel
	if diags := data.SSHAccess.ElementsAs(context.Background(), &users, false); diags.HasError() {
		t.Fatal(diags.Errors())
	}
	byName := map[string]sshUserModel{}
	for _, u := range users {
		byName[u.Username.ValueString()] = u
	}
	zoe := byName["zoe"]
	var keys []string
	zoe.SSHPublicKeys.ElementsAs(context.Background(), &keys, false)
	if len(keys) != 2 || keys[0] != "zed" || keys[1] != "abe" {
		t.Errorf("zoe's keys = %v, want [zed abe] in the order sent", keys)
	}
	if zoe.Sudo.ValueString() != "ALL=(ALL) NOPASSWD:ALL" || !zoe.DisplayName.IsNull() {
		t.Errorf("zoe = %+v, want the sudo fragment and no display name", zoe)
	}
	if byName["adam"].DisplayName.ValueString() != "Adam" || !byName["adam"].Sudo.IsNull() {
		t.Errorf("adam = %+v, want the display name and no sudo", byName["adam"])
	}

	none := filledInstanceGroup(t, `{"id":"g","clusterType":"/ct","securityGroups":[]}`)
	if !none.SSHAccess.IsNull() {
		t.Errorf("ssh_access = %s, want null for a group the platform echoes none for", none.SSHAccess)
	}
}
