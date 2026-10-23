package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/setplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// A create carries the configured users, and leaves the field out when there
// are none. State holds what was sent: the next refresh is what reads it.
func TestAClusterCreateSendsItsSSHAccess(t *testing.T) {
	for name, tt := range map[string]struct {
		planned tftypes.Value
		want    any
	}{
		"one user": {opsAccess(), []any{map[string]any{"username": "ops", "sshPublicKeys": []any{"ssh-ed25519 AAAA ops"}}}},
		"unset":    {tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, nil), nil},
	} {
		t.Run(name, func(t *testing.T) {
			var g groupsServer
			server := g.start(t)
			r := &clusterResource{meta: testClusterMeta(t, server.URL, time.Minute)}
			planRaw, s := testValue(t, r, existingCluster(map[string]tftypes.Value{"ssh_access": tt.planned}))
			resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(planRaw.Type(), nil), Schema: s.Schema}}
			r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}, &resp)
			if resp.Diagnostics.HasError() {
				t.Fatalf("create: %v", resp.Diagnostics.Errors())
			}
			sent := g.bodies()
			if len(sent) != 1 {
				t.Fatalf("sent %d mutating requests, want 1: %v", len(sent), sent)
			}
			got, ok := sent[0]["sshAccess"]
			if tt.want == nil && ok {
				t.Errorf("create carried sshAccess = %v, want it left out", got)
			}
			if tt.want != nil && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("create carried sshAccess = %v, want %v", got, tt.want)
			}
			var stored types.Set
			resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root("ssh_access"), &stored)...)
			if want := setOf(t, tt.planned); !stored.Equal(want) {
				t.Errorf("state ssh_access = %v, want what was sent, %v", stored, want)
			}
		})
	}
}

// The cluster's own body says nothing about its users, so mapping it keeps the
// ones state holds. A model with none maps to a typed null.
func TestAClusterBodyKeepsTheSSHAccessItSent(t *testing.T) {
	prior := testClusterPriorState()
	prior.SSHAccess = setOf(t, opsAccess())
	got, diags := clusterStateFrom(context.Background(), k0rdent.Cluster{Id: "tf-demo"}, prior, defaultTags{})
	if diags.HasError() || !got.SSHAccess.Equal(prior.SSHAccess) {
		t.Errorf("ssh_access = %v (%v), want it kept from state", got.SSHAccess, diags)
	}
	got, _ = clusterStateFrom(context.Background(), k0rdent.Cluster{Id: "bare"}, clusterModel{}, defaultTags{})
	if !got.SSHAccess.IsNull() || got.SSHAccess.ElementType(context.Background()) == nil {
		t.Errorf("ssh_access = %#v, want a typed null", got.SSHAccess)
	}
}

// A change replaces a cluster whose users state knows. Without that record,
// an imported cluster where the platform doesn't report them, it's recorded
// in place.
func TestAClusterSSHAccessChangeReplacesOnlyWhenRecorded(t *testing.T) {
	recorded := memPrivate{}
	if d := recordSSHAccess(context.Background(), recorded); d.HasError() {
		t.Fatal(d)
	}
	if !sshAccessRecorded(context.Background(), recorded) {
		t.Error("a recorded cluster reads as unrecorded")
	}
	if sshAccessRecorded(context.Background(), memPrivate{}) {
		t.Error("a cluster with no record reads as recorded")
	}
	var none *memPrivate
	if sshAccessRecorded(context.Background(), none) || recordSSHAccess(context.Background(), none).HasError() {
		t.Error("no private state should read as unrecorded, and recording into it should do nothing")
	}
}

// On an unrecorded cluster, a changed `ssh_access` warns that the nodes keep
// their users. An unchanged one says nothing.
func TestAnUnrecordedSSHAccessChangeWarns(t *testing.T) {
	r := &clusterResource{}
	stateRaw, s := testValue(t, r, existingCluster(nil))
	changed, _ := testValue(t, r, existingCluster(map[string]tftypes.Value{"ssh_access": opsAccess()}))
	for name, tt := range map[string]struct {
		plan tftypes.Value
		warn bool
	}{"changed": {changed, true}, "unchanged": {stateRaw, false}} {
		t.Run(name, func(t *testing.T) {
			var diags diag.Diagnostics
			unrecordedSSHAccessWarning(context.Background(), tfsdk.State{Raw: stateRaw, Schema: s.Schema},
				tfsdk.Plan{Raw: tt.plan, Schema: s.Schema}, &diags)
			if got := diags.WarningsCount() == 1; got != tt.warn {
				t.Fatalf("warned = %v, want %v: %v", got, tt.warn, diags)
			}
			if tt.warn && !strings.Contains(diags.Warnings()[0].Detail(), "terraform apply -replace") {
				t.Errorf("the warning doesn't say how to put the users on the nodes:\n%s", diags.Warnings()[0].Detail())
			}
		})
	}
}

// Recorded in place, an `ssh_access` change sends nothing to the platform.
func TestAnInPlaceSSHAccessChangeSendsNothing(t *testing.T) {
	if sent := runClusterUpdate(t, nil, map[string]tftypes.Value{"ssh_access": opsAccess()}); len(sent) != 0 {
		t.Errorf("sent %v, want nothing", sent)
	}
}

func setOf(t *testing.T, v tftypes.Value) types.Set {
	t.Helper()
	got, err := types.SetType{ElemType: sshUserObjectType()}.ValueFromTerraform(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	set, ok := got.(types.Set)
	if !ok {
		t.Fatalf("got %T, want types.Set", got)
	}
	return set
}

// sshAccessAPI is a cluster c1 whose ssh-access read answers with answer, or
// has no route when answer is nil.
func sshAccessAPI(t *testing.T, answer http.HandlerFunc) string {
	t.Helper()
	const cluster = "/v1/regions/test/projects/test/compute/clusters/c1"
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+cluster, func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"id": "c1", "state": "active", "clusterType": "/v1/regions/test/compute/cluster-types/test"})
	})
	if answer != nil {
		mux.HandleFunc("GET "+cluster+"/ssh-access", answer)
	}
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server.URL
}

func answerJSON(t *testing.T, status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := io.WriteString(w, body); err != nil {
			t.Errorf("write: %v", err)
		}
	}
}

// readCluster refreshes cluster c1, held in state with prior's users, against
// the API at url, and returns the read and whether it recorded the users.
func readCluster(t *testing.T, url string, prior tftypes.Value) (resource.ReadResponse, bool) {
	t.Helper()
	r := &clusterResource{meta: testClusterMeta(t, url, time.Minute)}
	raw, s := testValue(t, r, existingCluster(map[string]tftypes.Value{"ssh_access": prior}))
	st := tfsdk.State{Raw: raw, Schema: s.Schema}
	resp := resource.ReadResponse{State: st}
	freshPrivate(t, &resp.Private, nil)
	r.Read(context.Background(), resource.ReadRequest{State: st}, &resp)
	p, ok := any(resp.Private).(privateState)
	if !ok {
		t.Fatal("private state cannot be read")
	}
	return resp, sshAccessRecorded(context.Background(), p)
}

// A refresh takes the users the platform reports, so a difference from state
// is drift the plan shows, and records them, so a change to them replaces.
// None reported is null.
func TestAClusterReadTakesTheSSHAccessThePlatformReports(t *testing.T) {
	adam := tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, []tftypes.Value{
		tftypes.NewValue(sshUserTF, map[string]tftypes.Value{
			"username":        str("adam"),
			"ssh_public_keys": tftypes.NewValue(tftypes.List{ElementType: tftypes.String}, []tftypes.Value{str("ssh-ed25519 abe")}),
			"sudo":            tftypes.NewValue(tftypes.String, nil),
			"display_name":    str("Adam"),
		}),
	})
	for name, tt := range map[string]struct {
		body string
		want tftypes.Value
	}{
		"other users": {`[{"username":"adam","sshPublicKeys":["ssh-ed25519 abe"],"displayName":"Adam"}]`, adam},
		"none":        {`[]`, tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, nil)},
	} {
		t.Run(name, func(t *testing.T) {
			resp, recorded := readCluster(t, sshAccessAPI(t, answerJSON(t, http.StatusOK, tt.body)), opsAccess())
			if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() > 0 {
				t.Fatalf("read: %v", resp.Diagnostics)
			}
			var got types.Set
			resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root("ssh_access"), &got)...)
			if want := setOf(t, tt.want); !got.Equal(want) {
				t.Errorf("ssh_access = %v, want %v", got, want)
			}
			if !recorded {
				t.Error("the users the platform reported weren't recorded")
			}
		})
	}
}

// A landscape that doesn't serve the read keeps the users state holds, and
// says nothing: no route, a 404 with or without the service's envelope, or a
// 405. Any other failure keeps them too, and warns. Neither records them.
func TestAClusterReadKeepsItsSSHAccessWhenThePlatformDoesNotReportIt(t *testing.T) {
	for name, tt := range map[string]struct {
		answer http.HandlerFunc
		warn   bool
	}{
		"no route":            {nil, false},
		"a gateway's 404":     {answerJSON(t, http.StatusNotFound, `{"message":"no Route matched with those values"}`), false},
		"the service's 404":   {answerJSON(t, http.StatusNotFound, `{"error":{"code":404,"message":"not found"}}`), false},
		"405":                 {answerJSON(t, http.StatusMethodNotAllowed, ``), false},
		"403":                 {answerJSON(t, http.StatusForbidden, `{"error":{"code":403,"message":"denied","requestId":"req-ssh"}}`), true},
		"a body that's wrong": {answerJSON(t, http.StatusOK, `{"users":[]}`), true},
	} {
		t.Run(name, func(t *testing.T) {
			resp, recorded := readCluster(t, sshAccessAPI(t, tt.answer), opsAccess())
			if resp.Diagnostics.HasError() {
				t.Fatalf("read failed: %v", resp.Diagnostics)
			}
			if got := resp.Diagnostics.WarningsCount() > 0; got != tt.warn {
				t.Errorf("warned = %v, want %v: %v", got, tt.warn, resp.Diagnostics)
			}
			if tt.warn && !strings.Contains(resp.Diagnostics.Warnings()[0].Detail(), "State keeps") {
				t.Errorf("the warning doesn't say state keeps its users:\n%s", resp.Diagnostics.Warnings()[0].Detail())
			}
			var got types.Set
			resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root("ssh_access"), &got)...)
			if want := setOf(t, opsAccess()); !got.Equal(want) {
				t.Errorf("ssh_access = %v, want it kept from state", got)
			}
			if recorded {
				t.Error("users the platform didn't report were recorded")
			}
		})
	}
}

// Null and `[]` both ask for no users, so moving between them replaces
// nothing. Anything else on a recorded cluster replaces.
func TestNoSSHUsersEitherWayReplacesNothing(t *testing.T) {
	empty := tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, []tftypes.Value{})
	null := tftypes.NewValue(tftypes.Set{ElementType: sshUserTF}, nil)
	for name, tt := range map[string]struct {
		state, plan tftypes.Value
		replace     bool
	}{
		"null to []":   {null, empty, false},
		"[] to null":   {empty, null, false},
		"null to some": {null, opsAccess(), true},
		"some to []":   {opsAccess(), empty, true},
	} {
		t.Run(name, func(t *testing.T) {
			req := planmodifier.SetRequest{StateValue: setOf(t, tt.state), PlanValue: setOf(t, tt.plan)}
			freshPrivate(t, &req.Private, nil)
			p, ok := any(req.Private).(privateWriter)
			if !ok {
				t.Fatal("private state cannot be written")
			}
			if d := recordSSHAccess(context.Background(), p); d.HasError() {
				t.Fatal(d)
			}
			var resp setplanmodifier.RequiresReplaceIfFuncResponse
			clusterSSHAccessRequiresReplace(context.Background(), req, &resp)
			if resp.RequiresReplace != tt.replace {
				t.Errorf("replace = %v, want %v", resp.RequiresReplace, tt.replace)
			}
		})
	}
}

// From the k0rdent API v1.3.0-rc.1, keys sent on a cluster create come back as
// references to SSH key credentials, not the keys: self-links, as v1.3.0-rc.2
// types them. A user with none echoed
// keeps the keys state held for that username, so the plan stays clean
// instead of proposing a replacement on every refresh. Keys echoed are still
// the value, and a user state never held has none to keep.
func TestAClusterReadKeepsKeysTheCredentialReferencesStandFor(t *testing.T) {
	for name, tt := range map[string]struct {
		body string
		want []string
	}{
		"credential references":  {`[{"username":"ops","sshPublicKeyCredentials":["/v1/regions/test/projects/test/credentials/mcred-01jabcdefghjkmnpqrstvwxyz0"]}]`, []string{"ssh-ed25519 AAAA ops"}},
		"no keys at all":         {`[{"username":"ops"}]`, []string{"ssh-ed25519 AAAA ops"}},
		"other keys":             {`[{"username":"ops","sshPublicKeys":["ssh-ed25519 BBBB new"]}]`, []string{"ssh-ed25519 BBBB new"}},
		"a user state never had": {`[{"username":"eve","sshPublicKeyCredentials":["/v1/regions/test/projects/test/credentials/mcred-01jabcdefghjkmnpqrstvwxyz1"]}]`, nil},
	} {
		t.Run(name, func(t *testing.T) {
			resp, _ := readCluster(t, sshAccessAPI(t, answerJSON(t, http.StatusOK, tt.body)), opsAccess())
			if resp.Diagnostics.HasError() {
				t.Fatalf("read: %v", resp.Diagnostics)
			}
			var got []sshUserModel
			var set types.Set
			resp.Diagnostics.Append(resp.State.GetAttribute(context.Background(), path.Root("ssh_access"), &set)...)
			resp.Diagnostics.Append(set.ElementsAs(context.Background(), &got, false)...)
			if resp.Diagnostics.HasError() || len(got) != 1 {
				t.Fatalf("ssh_access = %v: %v", set, resp.Diagnostics)
			}
			var keys []string
			resp.Diagnostics.Append(got[0].SSHPublicKeys.ElementsAs(context.Background(), &keys, false)...)
			if strings.Join(keys, ",") != strings.Join(tt.want, ",") {
				t.Errorf("keys = %q, want %q", keys, tt.want)
			}
		})
	}
}
