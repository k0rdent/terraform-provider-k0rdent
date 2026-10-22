package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// What a refresh makes of a resource the platform is tearing down. See
// teardown_read.go.

var sshUserTF = tftypes.Object{AttributeTypes: map[string]tftypes.Type{
	"username":        tftypes.String,
	"ssh_public_keys": tftypes.List{ElementType: tftypes.String},
	"sudo":            tftypes.String,
	"display_name":    tftypes.String,
}}

// opsAccess is one user with one key, as a configuration writes it.
func opsAccess() tftypes.Value {
	return tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, []tftypes.Value{
		tftypes.NewValue(sshUserTF, map[string]tftypes.Value{
			"username":        str("ops"),
			"ssh_public_keys": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{str("ssh-ed25519 AAAA ops")}),
			"sudo":            tftypes.NewValue(tftypes.String, nil),
			"display_name":    tftypes.NewValue(tftypes.String, nil),
		}),
	})
}

// opsGroup is an applied instance group with that access, in the given state.
func opsGroup(state string) map[string]tftypes.Value {
	pool := tftypes.Object{AttributeTypes: map[string]tftypes.Type{"size": tftypes.Number}}
	return map[string]tftypes.Value{
		"id":                    str("pig"),
		"region":                str("test"),
		"project":               str("test"),
		"cluster_type":          str("/v1/regions/test/compute/cluster-types/r22-igt"),
		"cluster_type_resolved": str("/v1/regions/test/compute/cluster-types/r22-igt/versions/1"),
		"self_link":             str("/v1/regions/test/projects/test/compute/instance-groups/pig"),
		"state":                 str(state),
		"node_pool": tftypes.NewValue(tftypes.Map{ElementType: pool}, map[string]tftypes.Value{
			"hosts": tftypes.NewValue(pool, map[string]tftypes.Value{"size": tftypes.NewValue(tftypes.Number, 1)}),
		}),
		"ssh_access": opsAccess(),
	}
}

// deletingGroupBody is the body dev.4 answered for an instance group straight
// after its DELETE (live-shapes-dev4.txt, section E), less the uid, so the
// read doesn't go on to list the group's VPC.
func deletingGroupBody(state string) map[string]any {
	return map[string]any{
		"id":          "pig",
		"selfLink":    "/v1/regions/test/projects/test/compute/instance-groups/pig",
		"clusterType": "/v1/regions/test/compute/cluster-types/r22-igt/versions/1",
		"nodePools": []any{map[string]any{
			"nodePool": "hosts", "desiredCount": 1, "readyCount": 1, "notReadyCount": 0,
		}},
		"securityGroups": []any{},
		"sshAccess":      []any{},
		"state":          state,
		"createdAt":      "2026-10-11T22:06:38Z",
		"updatedAt":      "2026-10-11T22:08:10Z",
	}
}

// refreshThenPlan reads the group the way a refresh does, against a platform
// that answers body, then plans configured against what the read wrote, with
// the private state the read left. That's the pair `terraform plan` and the
// refresh before `terraform destroy` both run.
func refreshThenPlan(t *testing.T, body map[string]any, applied, configured map[string]tftypes.Value) (resource.ReadResponse, resource.ModifyPlanResponse) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()
	r := guardedModifier(t, &instanceGroupResource{meta: testClusterMeta(t, server.URL, time.Minute)},
		instanceGroupTenantState)

	raw, s := testValue(t, r, applied)
	prior := tfsdk.State{Raw: raw, Schema: s.Schema}
	req := resource.ReadRequest{State: prior}
	read := resource.ReadResponse{State: prior}
	freshPrivate(t, &req.Private, nil)
	freshPrivate(t, &read.Private, nil)
	r.Read(context.Background(), req, &read)
	if read.Diagnostics.HasError() {
		t.Fatalf("read: %v", read.Diagnostics)
	}

	configRaw, _ := testValue(t, r, configured)
	config := tfsdk.Config{Raw: configRaw, Schema: s.Schema}
	planned := tfsdk.Plan{Raw: configRaw, Schema: s.Schema}
	resp := resource.ModifyPlanResponse{Plan: planned}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Config: config, Plan: planned, State: read.State, Private: read.Private,
	}, &resp)
	return read, resp
}

func warningSummariesOf(read resource.ReadResponse) []string {
	var out []string
	for _, d := range read.Diagnostics.Warnings() {
		out = append(out, d.Summary())
	}
	return out
}

// Live on dev.4, a group read in `deleting` answered `"sshAccess":[]`, the
// provider wrote that to state, and the next `terraform plan` and
// `terraform destroy` both stopped on "Changing `ssh_access` would replace this
// instance group". A failed teardown settles the group `failed` with the
// users still gone, so that state reads the same way, and the users are kept
// there too so a second destroy gets past its refresh. A group that stays
// `failed` without its users is something to say, though, so that read warns.
func TestARefreshOfAGroupBeingTornDownKeepsItsSSHAccess(t *testing.T) {
	for state, warns := range map[string]bool{"deleting": false, "failed": true} {
		t.Run(state, func(t *testing.T) {
			read, plan := refreshThenPlan(t, deletingGroupBody(state), opsGroup("active"), opsGroup("active"))

			var access types.Set
			read.State.GetAttribute(context.Background(), path.Root("ssh_access"), &access)
			if access.IsNull() || len(access.Elements()) != 1 {
				t.Errorf("ssh_access after the read = %v, want the configured user kept", access)
			}
			var got types.String
			read.State.GetAttribute(context.Background(), path.Root("state"), &got)
			if got.ValueString() != state {
				t.Errorf("state = %v, want the platform's %q: only what it blanks is held", got, state)
			}
			if plan.Diagnostics.HasError() {
				t.Errorf("the plan after the read failed: %q", errorSummaries(plan.Diagnostics))
			}
			warnings := warningSummariesOf(read)
			if said := len(warnings) == 1 && strings.Contains(warnings[0], "no SSH users"); said != warns {
				t.Errorf("read warnings = %q, want a warning about the missing users: %v", warnings, warns)
			}
		})
	}
}

// A group applied with `ssh_access = []` never had users, so a failed read
// that answers none holds nothing and has nothing to warn about. Before, it
// warned about users it never had on every refresh.
func TestAFailedGroupWithNoSSHAccessDoesNotWarn(t *testing.T) {
	none := with(opsGroup("active"), map[string]tftypes.Value{
		"ssh_access": tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, []tftypes.Value{}),
	})
	read, plan := refreshThenPlan(t, deletingGroupBody("failed"), none, none)
	if warnings := warningSummariesOf(read); len(warnings) != 0 {
		t.Errorf("read warnings = %q, want none", warnings)
	}
	var access types.Set
	read.State.GetAttribute(context.Background(), path.Root("ssh_access"), &access)
	if access.IsNull() || len(access.Elements()) != 0 {
		t.Errorf("ssh_access after the read = %v, want the configured empty set", access)
	}
	if plan.Diagnostics.HasError() {
		t.Errorf("the plan after the read failed: %q", errorSummaries(plan.Diagnostics))
	}
}

// The platform and a configuration that drops `ssh_access` agree that a
// failed group with its users released has none. That isn't a replacement:
// the plan records it in place, and nothing is refused.
func TestDroppingSSHAccessFromAReleasedGroupIsNotAReplacement(t *testing.T) {
	for _, dropped := range []tftypes.Value{
		tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, nil),
		tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, []tftypes.Value{}),
	} {
		_, plan := refreshThenPlan(t, deletingGroupBody("failed"), opsGroup("active"),
			with(opsGroup("active"), map[string]tftypes.Value{"ssh_access": dropped}))
		if plan.Diagnostics.HasError() {
			t.Errorf("ssh_access = %v: refused: %q", dropped, errorSummaries(plan.Diagnostics))
		}
	}
}

// The hold is only for a group being torn down. An active group that reads
// back with no users has lost them, and that's still a difference the plan
// shows.
func TestAnActiveGroupThatLostItsSSHAccessStillPlansTheChange(t *testing.T) {
	_, plan := refreshThenPlan(t, deletingGroupBody("active"), opsGroup("active"), opsGroup("active"))
	if got := errorSummaries(plan.Diagnostics); len(got) != 1 || !strings.Contains(got[0], "Changing `ssh_access` would replace") {
		t.Errorf("errors = %q, want the replacement refused", got)
	}
}

// The hold lets an unchanged configuration through. A change that would
// replace the group is still refused while it's being deleted, since the
// guard doesn't stand down for a state it may be reading stale.
func TestTheGuardStillRefusesAReplacementWhileDeleting(t *testing.T) {
	other := tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, []tftypes.Value{
		tftypes.NewValue(sshUserTF, map[string]tftypes.Value{
			"username":        str("dev"),
			"ssh_public_keys": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{str("ssh-ed25519 AAAA dev")}),
			"sudo":            tftypes.NewValue(tftypes.String, nil),
			"display_name":    tftypes.NewValue(tftypes.String, nil),
		}),
	})
	_, plan := refreshThenPlan(t, deletingGroupBody("deleting"), opsGroup("active"),
		with(opsGroup("active"), map[string]tftypes.Value{"ssh_access": other}))
	if got := errorSummaries(plan.Diagnostics); len(got) != 1 || !strings.Contains(got[0], "Changing `ssh_access` would replace") {
		t.Errorf("errors = %q, want the replacement refused", got)
	}
}

// A cluster's DELETE soft-deletes its node pools in the same transaction that
// moves it to `deleting` (the k0rdent API's DeleteCluster), and dev.4 answered
// `"nodePools":[]` and `"nodeCount":0` for a cluster straight after its DELETE.
// The configured pools are kept without a warning that they aren't on the
// cluster: they were, until the delete took them. A cluster left `failed`
// that way warns that the platform reports no pools on it.
func TestAClusterBeingTornDownKeepsItsNodePools(t *testing.T) {
	for state, warns := range map[k0rdent.ClusterState]bool{k0rdent.ClusterStateDeleting: false, k0rdent.ClusterStateFailed: true} {
		t.Run(string(state), func(t *testing.T) {
			prior := testClusterPriorState()
			got, diags := clusterStateFrom(context.Background(), k0rdent.Cluster{
				Id:          "tf-demo",
				ClusterType: ptr(testHeadRef),
				State:       ptr(state),
				NodePools:   []k0rdent.ClusterNodePool{},
				NodeCount:   ptr(0),
			}, prior, defaultTags{})
			said := diags.WarningsCount() == 1 && strings.Contains(diags.Warnings()[0].Summary(), "no node pools")
			if diags.HasError() || said != warns || (!warns && diags.WarningsCount() > 0) {
				t.Errorf("diagnostics = %v, want a warning about the missing pools: %v", diags, warns)
			}
			if !got.NodePool.Equal(prior.NodePool) {
				t.Errorf("node_pool = %v, want the prior %v", got.NodePool, prior.NodePool)
			}
			if got.NodeCount.ValueInt64() != 0 {
				t.Errorf("node_count = %v, want the platform's 0: it's read-only, and true", got.NodeCount)
			}
		})
	}
}
