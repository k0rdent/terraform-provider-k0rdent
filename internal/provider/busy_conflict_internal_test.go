package provider

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// busyFake is a networking API that refuses a write the way the contract
// says it does while something the write depends on is mid-change: 409 with
// details[].reason CONFLICT_IN_USE, nothing written.
type busyFake struct {
	t  *testing.T
	mu sync.Mutex
	// reads is what each GET path answers, one state per read, the last one
	// repeating. A path with none answers 404.
	reads map[string][]string
	// extra is merged into every GET of a path, for the fields a read needs.
	extra map[string]map[string]any
	// refusals is how many writes to a path are refused before one is taken.
	refusals map[string]int
	// reason is the details[].reason a path's refusal carries,
	// CONFLICT_IN_USE when unset.
	reason map[string]string
	// writes counts the writes each path was sent.
	writes map[string]int
	// created holds what an accepted create made, by its own path.
	created map[string]map[string]any
}

func newBusyFake(t *testing.T) *busyFake {
	return &busyFake{
		t: t, reads: map[string][]string{}, extra: map[string]map[string]any{},
		refusals: map[string]int{}, writes: map[string]int{}, created: map[string]map[string]any{},
		reason: map[string]string{},
	}
}

func (f *busyFake) serve() *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(f.handle))
	f.t.Cleanup(server.Close)
	return server
}

func (f *busyFake) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	path := r.URL.Path
	if r.Method == http.MethodGet {
		f.read(w, path)
		return
	}

	f.writes[path]++
	if f.refusals[path] > 0 {
		f.refusals[path]--
		reason := f.reason[path]
		if reason == "" {
			reason = "CONFLICT_IN_USE"
		}
		f.answer(w, http.StatusConflict, map[string]any{"error": map[string]any{
			"code": 409, "status": "CONFLICT", "message": "not in a state that accepts this change",
			"details": []any{map[string]any{"reason": reason}},
		}})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Errorf("read the write to %s: %v", path, err)
	}
	switch {
	case strings.HasSuffix(path, "/peerings") || strings.HasSuffix(path, "/subnets") ||
		strings.HasSuffix(path, "/security-groups"):
		made := map[string]any{"state": "active"}
		for k, v := range body {
			made[k] = v
		}
		id, ok := body["id"].(string)
		if !ok {
			f.t.Errorf("the create to %s named no id", path)
		}
		f.created[path+"/"+id] = made
		f.answer(w, http.StatusCreated, made)
	case r.Method == http.MethodPost:
		// setVPCSecurityGroups: the binding is committed and renders.
		f.extra[path]["securityGroups"] = body["securityGroups"]
		f.reads[path] = []string{"updating", "active"}
		f.answer(w, http.StatusAccepted, f.body(path, "updating"))
	default:
		f.reads[path] = []string{"updating", "active"}
		f.answer(w, http.StatusOK, f.body(path, "updating"))
	}
}

func (f *busyFake) read(w http.ResponseWriter, path string) {
	if made, ok := f.created[path]; ok {
		f.answer(w, http.StatusOK, made)
		return
	}
	states := f.reads[path]
	if len(states) == 0 || states[0] == gone {
		f.answer(w, http.StatusNotFound, map[string]any{"error": map[string]any{
			"code": 404, "status": "NOT_FOUND", "message": "not found",
			"details": []any{map[string]any{"reason": "NOT_FOUND"}},
		}})
		return
	}
	state := states[0]
	if len(states) > 1 {
		f.reads[path] = states[1:]
	}
	f.answer(w, http.StatusOK, f.body(path, state))
}

func (f *busyFake) body(path, state string) map[string]any {
	out := map[string]any{"id": path[strings.LastIndex(path, "/")+1:], "state": state, "selfLink": path}
	for k, v := range f.extra[path] {
		out[k] = v
	}
	return out
}

func (f *busyFake) answer(w http.ResponseWriter, status int, body any) {
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		f.t.Errorf("write: %v", err)
	}
}

func (f *busyFake) sent(path string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes[path]
}

// gone is a read that answers 404, for a resource a test deletes partway.
const gone = "gone"

const (
	nearVPC    = "/v1/regions/test/projects/test/networking/vpcs/near"
	peerings   = nearVPC + "/peerings"
	farVPC     = "/v1/regions/test/projects/other/networking/vpcs/far"
	sameVPC    = "/v1/regions/test/projects/test/networking/vpcs/beside"
	subnets    = "/v1/regions/test/projects/test/networking/networks/n1/subnets"
	poolPath   = "/v1/regions/test/networking/address-pools/pool-a"
	groupsPath = "/v1/regions/test/projects/test/networking/security-groups"
	groupPath  = groupsPath + "/g1"
)

// createPeering runs a peering Create against the fake, to remote.
func createPeering(t *testing.T, f *busyFake, remote string, timeout time.Duration) resource.CreateResponse {
	t.Helper()
	r := &vpcPeeringResource{meta: testClusterMeta(t, f.serve().URL, timeout)}
	raw, s := testValue(t, r, map[string]tftypes.Value{
		"id": str("p1"), "vpc": str("near"), "region": str("test"), "project": str("test"),
		"remote_vpc": str(remote),
	})
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(raw.Type(), nil), Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}}, &resp)
	return resp
}

// The race a configuration that binds and peers one VPC runs into. The
// binding and the peering both depend only on the cluster, so Terraform sends
// them in the same second.
// The binding moves the VPC to `updating`, and the peering is refused until it
// settles. The refusal wrote nothing, so the peering is sent again once the
// VPC reads `active`.
func TestAPeeringRefusedWhileItsVPCIsUpdatingIsSentAgainOnceItSettles(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[peerings] = 1
	f.reads[nearVPC] = []string{"updating", "updating", "active"}

	resp := createPeering(t, f, farVPC, time.Minute)
	if resp.Diagnostics.HasError() {
		t.Fatalf("the peering failed on a refusal that waiting clears: %v", resp.Diagnostics.Errors())
	}
	if got := f.sent(peerings); got != 2 {
		t.Errorf("the create was sent %d times, want 2: refused once, then taken", got)
	}
}

// The same 409 also means the direction is already peered, which no wait can
// change. With the VPC settled, the create is sent once more, in case the VPC
// settled between the refusal and the read, and a second refusal on a VPC
// still `active` is reported.
func TestAPeeringConflictOnASettledVPCIsReportedAfterOneResend(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[peerings] = 5
	f.reads[nearVPC] = []string{"active"}

	resp := createPeering(t, f, farVPC, time.Minute)
	assertConflictReported(t, resp.Diagnostics)
	if got := f.sent(peerings); got != 2 {
		t.Errorf("the create was sent %d times, want 2: once, then once more on a settled VPC", got)
	}
}

// The VPC was `updating` when the platform refused the peering and `active` a
// moment later, by the time the provider read it. Reporting the refusal then
// would lose a race the user has no part in, so the create is sent again.
func TestAPeeringRefusedJustBeforeItsVPCSettledIsSentAgain(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[peerings] = 1
	f.reads[nearVPC] = []string{"active"}

	resp := createPeering(t, f, farVPC, time.Minute)
	if resp.Diagnostics.HasError() {
		t.Fatalf("the peering failed on a refusal its VPC had already moved past: %v", resp.Diagnostics.Errors())
	}
	if got := f.sent(peerings); got != 2 {
		t.Errorf("the create was sent %d times, want 2", got)
	}
}

// The same race on the binding, which is where it was first seen.
func TestABindingRefusedJustBeforeTheVPCSettledIsSentAgain(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[nearVPC] = 1
	f.reads[nearVPC] = []string{"active"}
	f.extra[nearVPC] = map[string]any{"securityGroups": []any{"g1"}}

	r := &vpcSecurityGroupsResource{meta: testClusterMeta(t, f.serve().URL, time.Minute)}
	groups, _ := types.ListValueFrom(context.Background(), types.StringType, []string{"g1"})
	var s resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &s)
	dst := tfsdk.State{Raw: tftypes.NewValue(s.Schema.Type().TerraformType(context.Background()), nil), Schema: s.Schema}
	_, diags := r.apply(context.Background(), vpcSecurityGroupsModel{
		VPC: types.StringValue("near"), SecurityGroups: groups,
		Region: types.StringValue("test"), Project: types.StringValue("test"),
	}, &dst, nil)
	if diags.HasError() {
		t.Fatalf("the binding failed on a refusal its VPC had already moved past: %v", diags.Errors())
	}
	if got := f.sent(nearVPC); got != 2 {
		t.Errorf("the binding was sent %d times, want 2", got)
	}
}

// A VPC that failed doesn't settle into anything a peering can be declared on,
// so the refusal is reported rather than waited on.
func TestAPeeringConflictOnAFailedVPCIsReportedAtOnce(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[peerings] = 1
	f.reads[nearVPC] = []string{"failed"}

	resp := createPeering(t, f, farVPC, time.Minute)
	assertConflictReported(t, resp.Diagnostics)
	if got := f.sent(peerings); got != 1 {
		t.Errorf("the create was sent %d times, want 1", got)
	}
}

// The contract's other unsettled VPC is a remoteVpc in the same project.
func TestAPeeringWaitsForARemoteVPCInItsOwnProject(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[peerings] = 1
	f.reads[nearVPC] = []string{"active"}
	f.reads[sameVPC] = []string{"creating", "active"}

	resp := createPeering(t, f, sameVPC, time.Minute)
	if resp.Diagnostics.HasError() {
		t.Fatalf("the peering failed on a refusal that waiting clears: %v", resp.Diagnostics.Errors())
	}
	if got := f.sent(peerings); got != 2 {
		t.Errorf("the create was sent %d times, want 2", got)
	}
}

// A wait the timeout ends is reported with the platform's own refusal, so
// the reason and the request id still reach the user.
func TestAPeeringWaitThatRunsOutReportsTheRefusal(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[peerings] = 1
	f.reads[nearVPC] = []string{"updating"}

	resp := createPeering(t, f, farVPC, 50*time.Millisecond)
	assertConflictReported(t, resp.Diagnostics)
	if detail := resp.Diagnostics.Errors()[0].Detail(); !strings.Contains(detail, "timed out waiting") {
		t.Errorf("the error doesn't say the wait ran out:\n%s", detail)
	}
	if got := f.sent(peerings); got != 1 {
		t.Errorf("the create was sent %d times, want 1", got)
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state was written for a create the platform refused: %v", resp.State.Raw)
	}
}

func assertConflictReported(t *testing.T, diags diag.Diagnostics) {
	t.Helper()
	if !diags.HasError() {
		t.Fatal("the refusal was not reported")
	}
	if detail := diags.Errors()[0].Detail(); !strings.Contains(detail, "CONFLICT_IN_USE") {
		t.Errorf("the error doesn't carry the platform's reason:\n%s", detail)
	}
}

// setVPCSecurityGroups is refused while a binding change is still rendering,
// and the contract says to wait for the VPC to settle and retry.
func TestABindingRefusedWhileTheVPCIsUpdatingIsSentAgain(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[nearVPC] = 1
	// The first read is the destroy's own, comparing the binding with state
	// before it clears anything. The VPC is still rendering for that one and
	// for the read after the refusal.
	f.reads[nearVPC] = []string{"updating", "updating", "active"}
	f.extra[nearVPC] = map[string]any{"securityGroups": []any{"g1"}}

	r := &vpcSecurityGroupsResource{meta: testClusterMeta(t, f.serve().URL, time.Minute)}
	groups, _ := types.ListValueFrom(context.Background(), types.StringType, []string{"g1"})
	var s resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &s)
	dst := tfsdk.State{Raw: tftypes.NewValue(s.Schema.Type().TerraformType(context.Background()), nil), Schema: s.Schema}
	_, diags := r.apply(context.Background(), vpcSecurityGroupsModel{
		VPC: types.StringValue("near"), SecurityGroups: groups,
		Region: types.StringValue("test"), Project: types.StringValue("test"),
	}, &dst, nil)
	if diags.HasError() {
		t.Fatalf("the binding failed on a refusal that waiting clears: %v", diags.Errors())
	}
	if got := f.sent(nearVPC); got != 2 {
		t.Errorf("the binding was sent %d times, want 2", got)
	}
}

// createSubnet is refused while its AddressPool isn't active.
func TestASubnetRefusedWhileItsAddressPoolIsCreatingIsSentAgain(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[subnets] = 1
	f.reads[poolPath] = []string{"creating", "active"}

	r := &subnetResource{meta: testClusterMeta(t, f.serve().URL, time.Minute)}
	raw, s := testValue(t, r, map[string]tftypes.Value{
		"id": str("s1"), "network": str("n1"), "region": str("test"), "project": str("test"),
		"address_pool":  str(poolPath),
		"prefix_length": tftypes.NewValue(tftypes.Number, 24),
	})
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(raw.Type(), nil), Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("the subnet failed on a refusal that waiting clears: %v", resp.Diagnostics.Errors())
	}
	if got := f.sent(subnets); got != 2 {
		t.Errorf("the create was sent %d times, want 2", got)
	}
}

// updateSecurityGroup is refused while the group is still applying an earlier
// change, and the contract says to poll until it settles, then retry.
func TestASecurityGroupUpdateRefusedWhileItIsUpdatingIsSentAgain(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[groupPath] = 1
	f.reads[groupPath] = []string{"updating", "active"}
	f.extra[groupPath] = map[string]any{"rules": map[string]any{"ingress": []any{}, "egress": []any{}}, "ownerKind": "tenant"}

	r := &securityGroupResource{meta: testClusterMeta(t, f.serve().URL, time.Minute)}
	base := map[string]tftypes.Value{"id": str("g1"), "region": str("test"), "project": str("test")}
	stateRaw, s := testValue(t, r, base)
	// A change to send: an update that changes nothing sends nothing.
	planRaw, _ := testValue(t, r, with(base, map[string]tftypes.Value{"display_name": str("renamed")}))
	resp := resource.UpdateResponse{State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}
	r.Update(context.Background(), resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("the update failed on a refusal that waiting clears: %v", resp.Diagnostics.Errors())
	}
	if got := f.sent(groupPath); got != 2 {
		t.Errorf("the update was sent %d times, want 2", got)
	}
}

// unsettled409 is how a 409 description says the operation can be refused
// while something isn't settled. It's deliberately broad: a match that isn't
// that case is decided once, with the words that show it.
var unsettled409 = regexp.MustCompile("(?i)settle|not active|`creating`|`updating`|retry|in.flight|" +
	"transient|tearing down|still being|clears|once it")

// The list of writes whose unsettled 409 is waited out is prose turned into
// code, as post503Decisions is. So every direction it could drift in fails
// here instead.
func TestEveryUnsettled409IsDecided(t *testing.T) {
	ops, _ := pinnedContract(t)
	for id, op := range ops {
		if op.method != http.MethodPost && op.method != http.MethodPatch {
			continue
		}
		says := op.own409 + " " + op.shared409
		if !unsettled409.MatchString(foldSpace.ReplaceAllString(says, " ")) {
			continue
		}
		if _, ok := busyConflictDecisions[id]; !ok {
			t.Errorf("%s %s (%s) has a 409 that can mean something isn't settled, and "+
				"busyConflictDecisions has no decision on it. Read its description, then decide it as "+
				"waited out, waited out elsewhere, or refused, with the words that decide it:\n%s",
				op.method, op.path, id, says)
		}
	}

	for id, d := range busyConflictDecisions {
		op, ok := ops[id]
		switch {
		case !ok:
			t.Errorf("busyConflictDecisions decides %s, which the contract no longer has", id)
			continue
		case op.method != http.MethodPost && op.method != http.MethodPatch:
			t.Errorf("busyConflictDecisions decides %s, which is a %s", id, op.method)
		case !strings.Contains(foldSpace.ReplaceAllString(op.own409, " "), d.says):
			t.Errorf("%s's 409 no longer says %q, which is what its decision stands on. It says:\n%s",
				id, d.says, op.own409)
		}
		set := 0
		for _, on := range []bool{d.waits, d.elsewhere != "", d.refused != ""} {
			if on {
				set++
			}
		}
		if set != 1 {
			t.Errorf("%s: a decision waits, names where it's waited out, or says why it isn't, "+
				"and this one does %d of those", id, set)
		}
	}
}

// A repeated refusal waits longer before each resend, from the poll interval
// up to the cap, and every pause is jittered.
func TestARepeatedRefusalBacksOff(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[peerings] = 5
	f.reads[nearVPC] = []string{"updating", "active", "updating", "active", "updating", "active",
		"updating", "active", "updating", "active", "active"}
	meta := testClusterMeta(t, f.serve().URL, time.Minute)
	meta.Wait.Interval = time.Millisecond

	var paused []time.Duration
	busy := newBusyRetry("createVPCPeering", meta.Wait, vpcDependency(meta, clusterTarget{region: "test", project: "test"}, "near"))
	busy.backoffCap = 4 * time.Millisecond
	busy.jitter = func(d time.Duration) time.Duration { return d }
	busy.sleep = func(_ context.Context, d time.Duration) error {
		paused = append(paused, d)
		return nil
	}
	sends := 0
	err := busy.sendUntilSettled(context.Background(), func(ctx context.Context) (*http.Response, []byte, error) {
		sends++
		got, err := meta.Client.CreateVPCPeeringWithResponse(ctx, "test", "test", "near",
			k0rdent.VPCPeeringCreateRequest{Id: ptr("p1"), RemoteVpc: farVPC})
		if err != nil {
			return nil, nil, err
		}
		return got.HTTPResponse, got.Body, nil
	})
	if err != nil {
		t.Fatalf("the refusals were not waited out: %v", err)
	}
	if sends != 6 {
		t.Errorf("sent %d times, want 6: five refusals, then taken", sends)
	}
	want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 4 * time.Millisecond}
	if !slices.Equal(paused, want) {
		t.Errorf("paused %v before the resends, want %v: none before the first, then doubling to the cap", paused, want)
	}
}

// A pause the deadline ends stops the resends, and says so.
func TestABackoffTheDeadlineEndsIsReported(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[peerings] = 2
	f.reads[nearVPC] = []string{"updating", "active", "updating"}
	meta := testClusterMeta(t, f.serve().URL, time.Minute)

	busy := newBusyRetry("createVPCPeering", meta.Wait, vpcDependency(meta, clusterTarget{region: "test", project: "test"}, "near"))
	busy.sleep = func(context.Context, time.Duration) error { return context.DeadlineExceeded }
	err := busy.sendUntilSettled(context.Background(), func(ctx context.Context) (*http.Response, []byte, error) {
		got, err := meta.Client.CreateVPCPeeringWithResponse(ctx, "test", "test", "near",
			k0rdent.VPCPeeringCreateRequest{Id: ptr("p1"), RemoteVpc: farVPC})
		if err != nil {
			return nil, nil, err
		}
		return got.HTTPResponse, got.Body, nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the deadline", err)
	}
	if got := f.sent(peerings); got != 2 {
		t.Errorf("sent %d times, want 2", got)
	}
}

// Only an operation decided as waiting can build one, so a call site can't
// wait out a 409 nobody read the contract for.
func TestABusyRetryNeedsAWaitingDecision(t *testing.T) {
	for _, op := range []string{"updateNetwork", "scaleCluster", "createCluster"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s built a busyRetry", op)
				}
			}()
			newBusyRetry(op, waitConfig{})
		}()
	}
}

// A destroy hands the VPC back to its default group with the same call, so a
// VPC still rendering an earlier binding refuses it the same way.
func TestABindingDestroyWhileTheVPCIsUpdatingIsSentAgain(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[nearVPC] = 1
	// The first read is the destroy's own, comparing the binding with state
	// before it clears anything. The VPC is still rendering for that one and
	// for the read after the refusal.
	f.reads[nearVPC] = []string{"updating", "updating", "active"}
	f.extra[nearVPC] = map[string]any{"securityGroups": []any{"g1"}}

	r := &vpcSecurityGroupsResource{meta: testClusterMeta(t, f.serve().URL, time.Minute)}
	raw, s := testValue(t, r, map[string]tftypes.Value{
		"vpc": str("near"), "region": str("test"), "project": str("test"),
	})
	resp := resource.DeleteResponse{State: tfsdk.State{Raw: raw, Schema: s.Schema}}
	r.Delete(context.Background(), resource.DeleteRequest{State: tfsdk.State{Raw: raw, Schema: s.Schema}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("the destroy failed on a refusal that waiting clears: %v", resp.Diagnostics.Errors())
	}
	if got := f.sent(nearVPC); got != 2 {
		t.Errorf("the binding was sent %d times, want 2", got)
	}
}

// A group still tearing down hasn't released its id, so a create under that
// id answers CONFLICT_DUPLICATE until it's gone. That's what a replacement
// with a configured id meets when another run is deleting the old group.
func TestASecurityGroupCreateWaitsForTheOldOneToFinishDeleting(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[groupsPath] = 1
	f.reason[groupsPath] = "CONFLICT_DUPLICATE"
	f.reads[groupPath] = []string{"deleting", "deleting", "deleting", gone}
	f.extra[groupPath] = map[string]any{"rules": map[string]any{"ingress": []any{}, "egress": []any{}}}

	r := &securityGroupResource{meta: testClusterMeta(t, f.serve().URL, time.Minute)}
	raw, s := testValue(t, r, map[string]tftypes.Value{"id": str("g1"), "region": str("test"), "project": str("test")})
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(raw.Type(), nil), Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}}, &resp)
	if resp.Diagnostics.HasError() {
		t.Fatalf("the create failed on a refusal that waiting clears: %v", resp.Diagnostics.Errors())
	}
	if got := f.sent(groupsPath); got != 2 {
		t.Errorf("the create was sent %d times, want 2", got)
	}
}

// The same CONFLICT_DUPLICATE on a group that isn't going anywhere is a
// name in use, and stays a refusal with the import command.
func TestASecurityGroupCreateOnALiveDuplicateIsRefused(t *testing.T) {
	f := newBusyFake(t)
	f.refusals[groupsPath] = 1
	f.reason[groupsPath] = "CONFLICT_DUPLICATE"
	f.reads[groupPath] = []string{"active"}
	f.extra[groupPath] = map[string]any{"rules": map[string]any{"ingress": []any{}, "egress": []any{}}}

	r := &securityGroupResource{meta: testClusterMeta(t, f.serve().URL, time.Minute)}
	raw, s := testValue(t, r, map[string]tftypes.Value{"id": str("g1"), "region": str("test"), "project": str("test")})
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(raw.Type(), nil), Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}}, &resp)
	assertRefusedWithImport(t, resp.Diagnostics, "terraform import <address> test/test/g1")
	if got := f.sent(groupsPath); got != 1 {
		t.Errorf("the create was sent %d times, want 1", got)
	}
}
