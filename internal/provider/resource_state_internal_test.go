package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

func ptr[T any](v T) *T { return &v }

const (
	testHeadRef   = "/v1/regions/us-texas-1/compute/cluster-types/gpu-training"
	testPinnedRef = testHeadRef + "/versions/3"
)

func testClusterPriorState() clusterModel {
	pool, _ := types.MapValueFrom(context.Background(), clusterNodePoolObjectType(),
		map[string]clusterNodePoolModel{"gpu-workers": {NodeCount: types.Int64Value(2)}})
	return clusterModel{
		ID:          types.StringValue("tf-demo"),
		ClusterType: types.StringValue(testHeadRef),
		NodePool:    pool,
		Tags:        types.MapNull(types.StringType),
	}
}

// Six of Cluster's nine required properties arrive as Go pointers because they
// are also `readOnly`, so the contract's presence guarantee is not expressible
// in the generated client and a nil is a contract violation the client accepts
// silently. The obligation that creates is one nil branch per
// field, and the failure mode if it is missed is a panic inside somebody's
// `terraform apply`.
//
// This maps the emptiest Cluster the generated type permits: every pointer nil,
// every slice nil, only the three required non-readOnly fields present.
func TestClusterStateFromSurvivesAnAllNilCluster(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("mapping a Cluster with every optional pointer nil panicked: %v\n"+
				"Every dereference needs a nil branch.", r)
		}
	}()

	got, diags := clusterStateFrom(context.Background(), k0rdent.Cluster{Id: "bare"}, clusterModel{}, defaultTags{})

	if diags.HasError() {
		t.Fatalf("mapping produced errors: %v", diags.Errors())
	}
	if got.ID.ValueString() != "bare" {
		t.Errorf("id = %q, want %q", got.ID.ValueString(), "bare")
	}
	// An absent value must be null, never the zero value: Terraform treats "" and
	// 0 as written values, so a zero here becomes a diff against an unset
	// configuration.
	if !got.State.IsNull() {
		t.Errorf("state = %v, want null when the platform reported none", got.State)
	}
	if !got.DisplayName.IsNull() || !got.Description.IsNull() || !got.SelfLink.IsNull() {
		t.Error("an absent optional string became a value rather than null")
	}
	if got.NodeCount.ValueInt64() != 0 {
		t.Errorf("node_count = %d, want 0 derived from no pools", got.NodeCount.ValueInt64())
	}
	if got.NodePool.IsNull() || got.NodePool.IsUnknown() {
		t.Error("node_pool must be a known empty map, not null: it is Computed, and a null " +
			"would plan as \"known after apply\" forever")
	}
}

// The round trip on the one required create field, at the level that matters:
// what lands in state.
func TestClusterStateFromKeepsTheConfiguredClusterTypeWhenThePlatformResolvesIt(t *testing.T) {
	t.Parallel()

	prior := testClusterPriorState()
	got, diags := clusterStateFrom(context.Background(), k0rdent.Cluster{
		Id:          "tf-demo",
		ClusterType: ptr(testPinnedRef),
		State:       ptr(k0rdent.ClusterStateActive),
	}, prior, defaultTags{})

	if diags.HasError() {
		t.Fatalf("mapping produced errors: %v", diags.Errors())
	}
	if got.ClusterType.ValueString() != testHeadRef {
		t.Errorf("cluster_type = %q, want the configured %q kept.\n"+
			"Writing the platform's resolved value here is what makes every plan propose a "+
			"change on an immutable field.", got.ClusterType.ValueString(), testHeadRef)
	}
	if got.ClusterTypeResolved.ValueString() != testPinnedRef {
		t.Errorf("cluster_type_resolved = %q, want the platform's %q.\n"+
			"Tolerating the resolution is only defensible if it stays visible.",
			got.ClusterTypeResolved.ValueString(), testPinnedRef)
	}
}

func TestClusterStateFromReportsARealClusterTypeChange(t *testing.T) {
	t.Parallel()

	const other = "/v1/regions/us-texas-1/compute/cluster-types/cpu-general"
	got, _ := clusterStateFrom(context.Background(), k0rdent.Cluster{
		Id:          "tf-demo",
		ClusterType: ptr(other),
	}, testClusterPriorState(), defaultTags{})

	if got.ClusterType.ValueString() != other {
		t.Errorf("cluster_type = %q, want the platform's %q written through so Terraform "+
			"proposes the replacement a re-bound cluster deserves",
			got.ClusterType.ValueString(), other)
	}
}

// An import has no prior configuration, so the platform's value is all there is.
func TestClusterStateFromAdoptsThePlatformValueOnImport(t *testing.T) {
	t.Parallel()

	got, _ := clusterStateFrom(context.Background(), k0rdent.Cluster{
		Id:          "tf-demo",
		ClusterType: ptr(testPinnedRef),
		NodePools: []k0rdent.ClusterNodePool{
			{Id: "control-plane", NodeCount: 1},
			{Id: "gpu-workers", NodeCount: 4},
		},
	}, clusterModel{}, defaultTags{})

	if got.ClusterType.ValueString() != testPinnedRef {
		t.Errorf("cluster_type = %q, want %q", got.ClusterType.ValueString(), testPinnedRef)
	}
	// With nothing known, every pool the cluster has is adopted: it is the only way
	// a user learns what the cluster actually has.
	if n := len(got.NodePool.Elements()); n != 2 {
		t.Errorf("node_pool has %d entries, want both of the cluster's pools on an import", n)
	}
}

// The platform creates one pool per pool on the bound ClusterType, whether
// or not the configuration mentioned it. A provider that stored all of them
// against a configuration naming one would propose removing the rest on every
// plan.
func TestClusterStateFromNarrowsNodePoolsToWhatIsManaged(t *testing.T) {
	t.Parallel()

	got, diags := clusterStateFrom(context.Background(), k0rdent.Cluster{
		Id: "tf-demo",
		NodePools: []k0rdent.ClusterNodePool{
			{Id: "control-plane", NodeCount: 1},
			{Id: "gpu-workers", NodeCount: 7},
		},
		NodeCount: ptr(8),
	}, testClusterPriorState(), defaultTags{})

	if diags.HasError() {
		t.Fatalf("mapping produced errors: %v", diags.Errors())
	}
	var pools map[string]clusterNodePoolModel
	got.NodePool.ElementsAs(context.Background(), &pools, false)

	if len(pools) != 1 {
		t.Fatalf("node_pool has %d entries, want only the managed one: %v", len(pools), pools)
	}
	if n := pools["gpu-workers"].NodeCount.ValueInt64(); n != 7 {
		t.Errorf("the managed pool reads %d, want the platform's settled 7 — drift detection "+
			"on a managed pool is the point of narrowing rather than ignoring", n)
	}
	// node_count is the platform's total across every pool, including unmanaged
	// ones, so it does not agree with the managed map and should not be made to.
	if got.NodeCount.ValueInt64() != 8 {
		t.Errorf("node_count = %d, want the platform's total 8", got.NodeCount.ValueInt64())
	}
}

// An empty tag map may read back as the field being absent. Nulling state on
// that basis diffs against a configuration that wrote `tags = {}`.
func TestClusterStateFromKeepsAnEmptyTagMapThatThePlatformOmitted(t *testing.T) {
	t.Parallel()

	empty, _ := types.MapValueFrom(context.Background(), types.StringType, map[string]string{})
	prior := testClusterPriorState()
	prior.Tags = empty

	got, _ := clusterStateFrom(context.Background(), k0rdent.Cluster{Id: "tf-demo"}, prior, defaultTags{})
	if got.Tags.IsNull() {
		t.Error("tags became null where the configuration wrote an empty map; that is a " +
			"permanent diff on every plan")
	}
	if n := len(got.Tags.Elements()); n != 0 {
		t.Errorf("tags has %d entries, want 0", n)
	}

	// The other direction: nothing configured, nothing returned, stays null.
	got, _ = clusterStateFrom(context.Background(), k0rdent.Cluster{Id: "tf-demo"}, testClusterPriorState(), defaultTags{})
	if !got.Tags.IsNull() {
		t.Errorf("tags = %v, want null when neither side has any", got.Tags)
	}
}

func TestClusterStateFromWarnsWhenARequiredClusterTypeIsMissing(t *testing.T) {
	t.Parallel()

	got, diags := clusterStateFrom(context.Background(), k0rdent.Cluster{Id: "tf-demo"}, testClusterPriorState(), defaultTags{})

	if got.ClusterType.ValueString() != testHeadRef {
		t.Errorf("cluster_type = %q, want the configured value kept rather than a required "+
			"argument nulled out of state", got.ClusterType.ValueString())
	}
	if len(diags.Warnings()) == 0 {
		t.Error("no warning for a missing required field; the contract marks clusterType " +
			"required, so its absence is worth saying out loud")
	}
}

// The pools array is optional in the response. Reading its absence as "no
// pools" would plan to destroy every one of them.
func TestInstanceGroupStateFromKeepsPoolsThePlatformOmitted(t *testing.T) {
	t.Parallel()

	prior, _ := types.MapValueFrom(context.Background(), instanceGroupPoolObjectType(),
		map[string]instanceGroupPoolModel{"gpu-workers": {Size: types.Int64Value(3)}})

	got, diags := instanceGroupStateFrom(context.Background(),
		k0rdent.InstanceGroup{Id: "hosts", ClusterType: testPinnedRef},
		instanceGroupModel{ClusterType: types.StringValue(testHeadRef), NodePool: prior})

	if !got.NodePool.Equal(prior) {
		t.Errorf("node_pool = %v, want the known pools kept", got.NodePool)
	}
	if len(diags.Warnings()) == 0 {
		t.Error("no warning when the platform omitted the pools")
	}
}

// `desiredCount` is the requested size under its response name. The live
// counts beside it must not reach the resource, or every refresh drifts.
func TestInstanceGroupStateFromReadsTheDesiredCount(t *testing.T) {
	t.Parallel()

	ready, notReady := int32(2), int32(1)
	pools := []k0rdent.NodePool{{NodePool: "gpu-workers", DesiredCount: 4, ReadyCount: &ready, NotReadyCount: &notReady}}
	got, diags := instanceGroupStateFrom(context.Background(),
		k0rdent.InstanceGroup{Id: "hosts", ClusterType: testPinnedRef, NodePools: &pools},
		instanceGroupModel{ClusterType: types.StringValue(testHeadRef)})
	if diags.HasError() {
		t.Fatal(diags.Errors())
	}
	var read map[string]instanceGroupPoolModel
	got.NodePool.ElementsAs(context.Background(), &read, false)
	if n := read["gpu-workers"].Size.ValueInt64(); n != 4 {
		t.Errorf("size = %d, want the desired 4, not a live count", n)
	}
}

func TestInstanceGroupStateFromSurvivesAnAllNilGroup(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("mapping a bare InstanceGroup panicked: %v", r)
		}
	}()

	got, diags := instanceGroupStateFrom(context.Background(),
		k0rdent.InstanceGroup{Id: "hosts", ClusterType: testHeadRef}, instanceGroupModel{})
	if diags.HasError() {
		t.Fatalf("mapping produced errors: %v", diags.Errors())
	}
	if !got.State.IsNull() {
		t.Errorf("state = %v, want null", got.State)
	}
}

// waitFor is the only place in the provider that loops, and D4 puts it here
// rather than in the client. Three properties are worth pinning: it probes
// before sleeping, it stops on the first error, and a timeout says the operation
// may still be running rather than that it failed.
func TestWaitForProbesBeforeSleeping(t *testing.T) {
	t.Parallel()

	calls := 0
	start := time.Now()
	err := waitFor(context.Background(), waitConfig{Timeout: time.Minute, Interval: time.Hour},
		"the thing", func(context.Context) (bool, error) {
			calls++
			return true, nil
		})
	if err != nil {
		t.Fatalf("waitFor returned %v", err)
	}
	if calls != 1 {
		t.Errorf("probed %d times, want 1", calls)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s; an already-settled operation must not pay a poll interval", elapsed)
	}
}

func TestWaitForLoopsUntilSettled(t *testing.T) {
	t.Parallel()

	calls := 0
	err := waitFor(context.Background(), waitConfig{Timeout: 10 * time.Second, Interval: time.Millisecond},
		"the thing", func(context.Context) (bool, error) {
			calls++
			return calls >= 3, nil
		})
	if err != nil {
		t.Fatalf("waitFor returned %v", err)
	}
	if calls != 3 {
		t.Errorf("probed %d times, want 3", calls)
	}
}

func TestWaitForStopsOnTheFirstError(t *testing.T) {
	t.Parallel()

	calls := 0
	err := waitFor(context.Background(), waitConfig{Timeout: 10 * time.Second, Interval: time.Millisecond},
		"the thing", func(context.Context) (bool, error) {
			calls++
			return false, errWaitTimeout // any error
		})
	if err == nil {
		t.Fatal("waitFor swallowed a probe error")
	}
	if calls != 1 {
		t.Errorf("probed %d times after an error, want 1; retrying is a separate decision "+
			"with separate hazards and is not a loop's job", calls)
	}
}

func TestWaitForTimeoutSaysTheOperationMayStillBeRunning(t *testing.T) {
	t.Parallel()

	err := waitFor(context.Background(), waitConfig{Timeout: 20 * time.Millisecond, Interval: time.Millisecond},
		"the cluster to finish creating", func(context.Context) (bool, error) {
			return false, nil
		})
	if err == nil {
		t.Fatal("waitFor returned nil after its timeout expired")
	}
	msg := err.Error()
	for _, want := range []string{"may still be running", "operation_timeout", "the cluster to finish creating"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the timeout message does not mention %q: %s", want, msg)
		}
	}
}

// The import id shape is a permanent public API, so both forms are pinned.
func TestImportIDAcceptsABareIDAndAScopedTriple(t *testing.T) {
	t.Parallel()

	id, region, project, diags := importID("  my-cluster  ", ResourceName(clusterResourceSuffix))
	if diags.HasError() {
		t.Fatalf("a bare id was rejected: %v", diags.Errors())
	}
	if id != "my-cluster" || region != "" || project != "" {
		t.Errorf("got %q/%q/%q, want a trimmed id and no scope", region, project, id)
	}

	id, region, project, diags = importID("us-texas-1/demo/my-cluster", ResourceName(clusterResourceSuffix))
	if diags.HasError() {
		t.Fatalf("a scoped triple was rejected: %v", diags.Errors())
	}
	if region != "us-texas-1" || project != "demo" || id != "my-cluster" {
		t.Errorf("got %q/%q/%q, want us-texas-1/demo/my-cluster", region, project, id)
	}

	if _, _, _, diags = importID("", ResourceName(clusterResourceSuffix)); !diags.HasError() {
		t.Error("an empty import id was accepted")
	}
	if _, _, _, diags = importID("us-texas-1//my-cluster", ResourceName(clusterResourceSuffix)); !diags.HasError() {
		t.Error("a triple with an empty project was accepted")
	}
}

// An import id writes straight to state, past every schema validator, and
// each part of it becomes a segment of the request URL. So the id and the
// parent are checked like the region and project beside them, and refused
// with the part that's wrong named, before anything is read.
func TestAnImportIDThatIsNotMadeOfIdentifiersIsRefused(t *testing.T) {
	t.Parallel()

	name := ResourceName(clusterResourceSuffix)
	for _, raw := range []string{"..", ".", "My-Cluster", "us-texas-1/demo/..", "us-texas-1/demo/a.b"} {
		if _, _, _, diags := importID(raw, name); !diags.HasError() {
			t.Errorf("importID(%q) was accepted", raw)
		} else if got := diags.Errors()[0].Detail(); !strings.Contains(got, "`id`") {
			t.Errorf("importID(%q) doesn't name the id: %s", raw, got)
		}
	}

	subnet := ResourceName(subnetResourceSuffix)
	for raw, part := range map[string]string{
		"../s1":                    "network",
		"n1/..":                    "id",
		"n1/S1":                    "id",
		"us-texas-1/demo/../s1":    "network",
		"us-texas-1/demo/n1/.":     "id",
		"us-texas-1/demo/n1/a%2fb": "id",
	} {
		_, _, _, _, diags := nestedImportID(raw, subnet, "network", "n1/s1")
		if !diags.HasError() {
			t.Errorf("nestedImportID(%q) was accepted", raw)
			continue
		}
		if got := diags.Errors()[0].Detail(); !strings.Contains(got, "`"+part+"`") {
			t.Errorf("nestedImportID(%q) doesn't name the %s: %s", raw, part, got)
		}
	}
	if parent, id, _, _, diags := nestedImportID("n1/s1", subnet, "network", "n1/s1"); diags.HasError() || parent != "n1" || id != "s1" {
		t.Errorf("a valid pair became %q/%q: %v", parent, id, diags)
	}
}

// The users as written: zoe first, the order a k0rdent API build from main was
// sent them in when it answered with adam first.
func sshUsersAsWritten() []k0rdent.SshUser {
	return []k0rdent.SshUser{
		{Username: "zoe", SshPublicKeys: &[]string{"zed", "abe"}, Sudo: ptr("ALL=(ALL) NOPASSWD:ALL")},
		{Username: "adam", SshPublicKeys: &[]string{"abe"}, DisplayName: ptr("Adam")},
	}
}

func sshAccessSet(t *testing.T, users []k0rdent.SshUser) types.Set {
	t.Helper()
	set, diags := sshAccessValue(context.Background(), &users)
	if diags.HasError() {
		t.Fatal(diags.Errors())
	}
	return set
}

// The platform sorts the users by username. A set compares without order, so
// the echo of what was written is no change, and the plan stays clean.
func TestInstanceGroupStateFromTakesTheSortedSSHEchoAsNoChange(t *testing.T) {
	t.Parallel()

	written := sshUsersAsWritten()
	prior := sshAccessSet(t, written)
	echo := []k0rdent.SshUser{written[1], written[0]}

	got, diags := instanceGroupStateFrom(context.Background(),
		k0rdent.InstanceGroup{Id: "hosts", ClusterType: testPinnedRef, SshAccess: &echo},
		instanceGroupModel{ClusterType: types.StringValue(testPinnedRef), SSHAccess: prior})
	if diags.HasError() {
		t.Fatal(diags.Errors())
	}
	if !got.SSHAccess.Equal(prior) {
		t.Errorf("ssh_access = %s, want %s: the entries only came back in another order", got.SSHAccess, prior)
	}
}

// A user's keys are a list, so the platform answering them in another order
// is a difference the plan shows.
func TestInstanceGroupStateFromSeesReorderedKeys(t *testing.T) {
	t.Parallel()

	written := sshUsersAsWritten()
	prior := sshAccessSet(t, written)
	echo := sshUsersAsWritten()
	echo[0].SshPublicKeys = &[]string{"abe", "zed"}

	got, _ := instanceGroupStateFrom(context.Background(),
		k0rdent.InstanceGroup{Id: "hosts", ClusterType: testPinnedRef, SshAccess: &echo},
		instanceGroupModel{ClusterType: types.StringValue(testPinnedRef), SSHAccess: prior})
	if got.SSHAccess.Equal(prior) {
		t.Error("ssh_access unchanged, want the reordered keys to show")
	}
}

// An API that does not echo `sshAccess` says nothing about it. Reading that as
// "removed" would plan a replacement of the group on every refresh.
func TestInstanceGroupStateFromKeepsSSHAccessThePlatformDidNotEcho(t *testing.T) {
	t.Parallel()

	prior := sshAccessSet(t, sshUsersAsWritten())
	got, diags := instanceGroupStateFrom(context.Background(),
		k0rdent.InstanceGroup{Id: "hosts", ClusterType: testPinnedRef},
		instanceGroupModel{ClusterType: types.StringValue(testPinnedRef), SSHAccess: prior})
	if diags.HasError() {
		t.Fatal(diags.Errors())
	}
	if !got.SSHAccess.Equal(prior) {
		t.Errorf("ssh_access = %s, want the prior value kept", got.SSHAccess)
	}
}

// A group created without SSH access reads null whether the platform leaves the
// field out or answers an empty list, so an unset attribute plans clean.
func TestInstanceGroupStateFromLeavesAbsentSSHAccessNull(t *testing.T) {
	t.Parallel()

	empty := []k0rdent.SshUser{}
	for name, echo := range map[string]*[]k0rdent.SshUser{"absent": nil, "empty": &empty} {
		got, diags := instanceGroupStateFrom(context.Background(),
			k0rdent.InstanceGroup{Id: "hosts", ClusterType: testPinnedRef, SshAccess: echo},
			instanceGroupModel{ClusterType: types.StringValue(testPinnedRef),
				SSHAccess: types.SetNull(sshUserObjectType())})
		if diags.HasError() {
			t.Fatal(diags.Errors())
		}
		if !got.SSHAccess.IsNull() {
			t.Errorf("%s echo: ssh_access = %s, want null", name, got.SSHAccess)
		}
	}
}

// Import starts from nothing, so whatever the platform echoes is adopted.
func TestInstanceGroupStateFromAdoptsTheSSHEchoOnImport(t *testing.T) {
	t.Parallel()

	echo := sshUsersAsWritten()
	got, diags := instanceGroupStateFrom(context.Background(),
		k0rdent.InstanceGroup{Id: "hosts", ClusterType: testPinnedRef, SshAccess: &echo}, instanceGroupModel{})
	if diags.HasError() {
		t.Fatal(diags.Errors())
	}
	if n := len(got.SSHAccess.Elements()); n != 2 {
		t.Errorf("ssh_access has %d users, want the 2 echoed", n)
	}
}
