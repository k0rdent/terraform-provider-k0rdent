package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// apiIDPattern is the id pattern every create in the contract declares.
var apiIDPattern = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// clusterAPI is a cluster endpoint that keeps what it was sent, answers a
// repeated id with 409 the way the platform does, and lets a test decide what
// each create does to its answer.
type clusterAPI struct {
	t  *testing.T
	mu sync.Mutex
	// tags holds each cluster this API has, by id.
	tags map[string]map[string]string
	// hidden ids read as 404, as a create still in flight would.
	hidden map[string]bool
	// sent is the id of every create, in order.
	sent []string
	// answer, when set, decides the nth create's answer once it has been
	// carried out. It returns false to give the ordinary one.
	answer func(n int, id string, w http.ResponseWriter) bool
	// readsFail makes every read answer 500.
	readsFail bool
}

func newClusterAPI(t *testing.T) *clusterAPI {
	return &clusterAPI{t: t, tags: map[string]map[string]string{}, hidden: map[string]bool{}}
}

func (a *clusterAPI) serve() *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(a.handle))
	a.t.Cleanup(s.Close)
	return s
}

func (a *clusterAPI) handle(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.Method {
	case http.MethodPost:
		var body struct {
			ID   string            `json:"id"`
			Tags map[string]string `json:"tags"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			a.t.Errorf("read create: %v", err)
		}
		a.sent = append(a.sent, body.ID)
		if _, exists := a.tags[body.ID]; exists {
			delete(a.hidden, body.ID)
			w.WriteHeader(http.StatusConflict)
			a.write(w, map[string]any{"error": map[string]any{
				"code": 409, "message": "cluster \"" + body.ID + "\" already exists in project",
			}})
			return
		}
		a.tags[body.ID] = body.Tags
		if a.answer != nil && a.answer(len(a.sent), body.ID, w) {
			return
		}
		w.WriteHeader(http.StatusCreated)
		a.write(w, a.cluster(body.ID))
	case http.MethodGet:
		if a.readsFail {
			w.WriteHeader(http.StatusInternalServerError)
			a.write(w, map[string]any{"error": map[string]any{"code": 500, "message": "read failed"}})
			return
		}
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if _, exists := a.tags[id]; !exists || a.hidden[id] {
			w.WriteHeader(http.StatusNotFound)
			a.write(w, map[string]any{"error": map[string]any{"code": 404, "message": "not found"}})
			return
		}
		a.write(w, a.cluster(id))
	default:
		http.NotFound(w, r)
	}
}

// cluster is an active cluster. It has no uid, so a read does not go on to
// look for its VPC.
func (a *clusterAPI) cluster(id string) map[string]any {
	c := map[string]any{
		"id": id, "clusterType": "/v1/regions/test/compute/cluster-types/test",
		"nodePools": []any{}, "nodeCount": 0, "state": "active", "securityGroups": []any{},
		"selfLink":  "/v1/regions/test/projects/test/compute/clusters/" + id,
		"createdAt": "2026-10-08T00:00:00Z", "updatedAt": "2026-10-08T00:00:00Z",
		"health": map[string]string{"state": "unknown"},
	}
	if tags := a.tags[id]; tags != nil {
		c["tags"] = tags
	}
	return c
}

func (a *clusterAPI) write(w http.ResponseWriter, v any) {
	if err := json.NewEncoder(w).Encode(v); err != nil {
		a.t.Errorf("write: %v", err)
	}
}

// loseAnswer closes the connection without answering, after the platform has
// carried the create out: the lost answer of an interrupted run.
func loseAnswer(t *testing.T, w http.ResponseWriter) bool {
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("the test server cannot hijack a connection")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		t.Fatalf("hijack: %v", err)
	}
	_ = conn.Close()
	return true
}

// createCluster runs Create with the given configured id, empty for none.
func createCluster(t *testing.T, server *httptest.Server, id string) resource.CreateResponse {
	t.Helper()
	meta := testClusterMeta(t, server.URL, time.Minute)
	meta.Wait.Interval = time.Millisecond
	r := &clusterResource{meta: meta}
	plan := testClusterState(t, id, "/v1/regions/test/compute/cluster-types/test")
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(plan.Raw.Type(), nil), Schema: plan.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan(plan)}, &resp)
	return resp
}

func stateID(t *testing.T, s tfsdk.State) string {
	t.Helper()
	if s.Raw.IsNull() {
		return ""
	}
	var id types.String
	if d := s.GetAttribute(context.Background(), path.Root("id"), &id); d.HasError() {
		t.Fatalf("read id: %v", d.Errors())
	}
	return id.ValueString()
}

func assertRefusedWithImport(t *testing.T, diags diag.Diagnostics, command string) {
	t.Helper()
	for _, d := range diags.Errors() {
		if strings.Contains(d.Summary(), "already exists") && strings.Contains(d.Detail(), command) {
			return
		}
	}
	t.Errorf("no refusal naming %q in %v", command, diags.Errors())
}

func TestAGeneratedIDMatchesTheAPIPattern(t *testing.T) {
	for range 100 {
		key, err := newCreateKey(types.StringNull(), true)
		if err != nil {
			t.Fatal(err)
		}
		if !key.generated || !apiIDPattern.MatchString(key.id) || len(key.id) > 63 {
			t.Fatalf("generated id %q does not match the API's pattern", key.id)
		}
		if len(key.token) != 32 {
			t.Fatalf("token %q is not 16 random bytes", key.token)
		}
	}
	key, err := newCreateKey(types.StringValue("mine"), false)
	if err != nil {
		t.Fatal(err)
	}
	if key.id != "mine" || key.generated || key.token != "" {
		t.Errorf("a configured id became %+v", key)
	}
}

func TestTheTokenIsAddedWithoutTouchingTheUsersTags(t *testing.T) {
	key := createKey{id: "c", token: "t"}
	user := map[string]string{"team": "a"}
	sent := key.tagged(&user)
	if len(user) != 1 {
		t.Errorf("the user's map was changed: %v", user)
	}
	if (*sent)[createTokenTag()] != "t" || (*sent)["team"] != "a" {
		t.Errorf("sent tags = %v", *sent)
	}
	if got := tagAnswerOf(sent).tags; len(got) != 1 || got["team"] != "a" {
		t.Errorf("read back = %v, want only the user's tag", got)
	}
	only := key.tagged(nil)
	if got := tagAnswerOf(only).forDataSource(); !got.IsNull() {
		t.Errorf("a data source reads a token-only map as %v, want no tags", got)
	}
}

// The token is the provider's. A cluster whose only tag is the token reads as
// having none, so a configuration without tags plans clean.
func TestTheTokenNeverReachesState(t *testing.T) {
	ctx := context.Background()
	only := map[string]string{createTokenTag(): "t"}
	both := map[string]string{createTokenTag(): "t", "team": "a"}
	prior := clusterModel{Tags: types.MapNull(types.StringType), NodePool: types.MapNull(clusterNodePoolObjectType())}

	got, d := clusterStateFrom(ctx, k0rdent.Cluster{Id: "c", Tags: only}, prior, defaultTags{})
	if d.HasError() || !got.Tags.IsNull() {
		t.Errorf("tags = %v (%v), want null", got.Tags, d)
	}
	got, d = clusterStateFrom(ctx, k0rdent.Cluster{Id: "c", Tags: both}, prior, defaultTags{})
	if d.HasError() || len(got.Tags.Elements()) != 1 {
		t.Errorf("tags = %v (%v), want only team", got.Tags, d)
	}

	net, d := networkStateFrom(ctx, evpnNetwork(t, &only), networkModel{Tags: types.MapNull(types.StringType)}, defaultTags{})
	if d.HasError() || !net.Tags.IsNull() {
		t.Errorf("network tags = %v (%v), want null", net.Tags, d)
	}
}

func evpnNetwork(t *testing.T, tags *map[string]string) k0rdent.Network {
	t.Helper()
	var n k0rdent.Network
	id := "n"
	v := k0rdent.NetworkEVPN{Id: &id, Tags: tags, Type: k0rdent.NetworkEVPNTypeEvpn}
	if err := n.FromNetworkEVPN(v); err != nil {
		t.Fatal(err)
	}
	return n
}

// With no id configured the create still names the cluster, so that a lost
// answer leaves something to read back.
func TestAClusterCreateWithNoIDSendsAGeneratedOne(t *testing.T) {
	api := newClusterAPI(t)
	resp := createCluster(t, api.serve(), "")
	if resp.Diagnostics.HasError() {
		t.Fatalf("create: %v", resp.Diagnostics.Errors())
	}
	if len(api.sent) != 1 || !strings.HasPrefix(api.sent[0], generatedIDPrefix) || !apiIDPattern.MatchString(api.sent[0]) {
		t.Fatalf("ids sent = %v, want one generated id", api.sent)
	}
	if api.tags[api.sent[0]][createTokenTag()] == "" {
		t.Errorf("the create carried no token: %v", api.tags[api.sent[0]])
	}
	if got := stateID(t, resp.State); got != api.sent[0] {
		t.Errorf("id in state = %q, want %q", got, api.sent[0])
	}
}

// The platform carried the create out and the answer never arrived. The
// cluster under the generated id carries this create's token, so it is kept
// and nothing is sent twice.
func TestALostCreateAnswerIsReadBackRatherThanSentAgain(t *testing.T) {
	api := newClusterAPI(t)
	api.answer = func(n int, _ string, w http.ResponseWriter) bool { return n == 1 && loseAnswer(t, w) }
	resp := createCluster(t, api.serve(), "")
	if resp.Diagnostics.HasError() {
		t.Fatalf("a lost answer was not recovered: %v", resp.Diagnostics.Errors())
	}
	if len(api.sent) != 1 || len(api.tags) != 1 {
		t.Fatalf("creates sent = %v, clusters = %d, want one of each", api.sent, len(api.tags))
	}
	if got := stateID(t, resp.State); got != api.sent[0] {
		t.Errorf("id in state = %q, want %q", got, api.sent[0])
	}
	if len(resp.Diagnostics.Warnings()) == 0 {
		t.Error("the recovery was not reported")
	}
}

// The answer is lost and the cluster does not read back yet, so the create is
// sent again under the same id and answers 409. The token it carries is this
// create's, so it is adopted, with a configured id as with a generated one.
func TestA409CarryingThisCreatesTokenIsAdopted(t *testing.T) {
	api := newClusterAPI(t)
	api.answer = func(n int, id string, w http.ResponseWriter) bool {
		if n != 1 {
			return false
		}
		api.hidden[id] = true
		return loseAnswer(t, w)
	}
	resp := createCluster(t, api.serve(), "c1")
	if resp.Diagnostics.HasError() {
		t.Fatalf("a 409 for this create's own cluster was not adopted: %v", resp.Diagnostics.Errors())
	}
	if len(api.sent) != 2 || len(api.tags) != 1 {
		t.Fatalf("creates sent = %v, clusters = %d, want two sends and one cluster", api.sent, len(api.tags))
	}
	if got := stateID(t, resp.State); got != "c1" {
		t.Errorf("id in state = %q, want c1", got)
	}
}

// A cluster with the configured id that this create did not make is not taken
// over. The error says so, quotes the platform and names the import command.
// One carrying another apply's token is most often what a killed run left, and
// the error says that too, but only then.
func TestA409WithoutThisCreatesTokenIsRefused(t *testing.T) {
	const killed = "Most often that's an apply stopped before it could save state, such as a killed process"
	for name, tc := range map[string]struct {
		tags      map[string]string
		saysKills bool
	}{
		"no token":            {nil, false},
		"another run's token": {map[string]string{createTokenTag(): "another-run"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			api := newClusterAPI(t)
			api.tags["c1"] = tc.tags
			resp := createCluster(t, api.serve(), "c1")
			assertRefusedWithImport(t, resp.Diagnostics, "terraform import <address> test/test/c1")
			found, kills := false, false
			for _, d := range resp.Diagnostics.Errors() {
				found = found || strings.Contains(d.Detail(), `cluster "c1" already exists in project`)
				kills = kills || strings.Contains(d.Detail(), killed)
			}
			if !found {
				t.Errorf("the platform's own message is missing: %v", resp.Diagnostics.Errors())
			}
			if kills != tc.saysKills {
				t.Errorf("names an interrupted apply = %v, want %v: %v", kills, tc.saysKills, resp.Diagnostics.Errors())
			}
			if !resp.State.Raw.IsNull() {
				t.Errorf("state was written for a cluster this create did not make: %v", resp.State.Raw)
			}
		})
	}
}

// A 504 whose workflow start timed out names the cluster it persisted. That
// cluster is recorded and waited out like a 201.
func TestACluster504NamingTheClusterIsWaitedOut(t *testing.T) {
	api := newClusterAPI(t)
	api.answer = func(_ int, id string, w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusGatewayTimeout)
		api.write(w, map[string]any{"error": map[string]any{
			"code": 504, "message": "deadline exceeded", "reason": "DEADLINE_EXCEEDED",
			"details": []any{map[string]any{"reason": "DEADLINE_EXCEEDED",
				"metadata": map[string]any{"clusterId": id, "selfLink": "/x/" + id}}},
		}})
		return true
	}
	resp := createCluster(t, api.serve(), "")
	if resp.Diagnostics.HasError() {
		t.Fatalf("a 504 naming the cluster failed the create: %v", resp.Diagnostics.Errors())
	}
	if got := stateID(t, resp.State); got == "" || got != api.sent[0] {
		t.Errorf("id in state = %q, want %q", got, api.sent[0])
	}
	if len(api.sent) != 1 {
		t.Errorf("creates sent = %v, want one", api.sent)
	}
}

// Nothing can say whether the create landed. The generated id goes to state
// anyway, so the next run reads it back rather than creating a second cluster.
func TestALostAnswerThatCannotBeReadBackRecordsTheID(t *testing.T) {
	api := newClusterAPI(t)
	api.readsFail = true
	api.answer = func(_ int, _ string, w http.ResponseWriter) bool { return loseAnswer(t, w) }
	resp := createCluster(t, api.serve(), "")
	if !resp.Diagnostics.HasError() {
		t.Fatal("an unconfirmed create reported success")
	}
	if got := stateID(t, resp.State); got == "" || got != api.sent[0] {
		t.Errorf("id in state = %q, want the one sent, %v", got, api.sent)
	}
	if len(api.sent) != 1 {
		t.Errorf("creates sent = %v, want one", api.sent)
	}
	// What the configuration asked for is kept, so the refresh that follows
	// reads the cluster against it rather than as an import.
	var clusterType types.String
	resp.State.GetAttribute(context.Background(), path.Root("cluster_type"), &clusterType)
	if clusterType.ValueString() != "/v1/regions/test/compute/cluster-types/test" {
		t.Errorf("cluster_type in state = %s, want the configured one", clusterType)
	}
}

// A configured id may name a cluster this state never managed. When neither
// the create nor the read back answers, nothing goes to state: a tainted row
// under that id would let the next run refresh someone else's cluster and plan
// to destroy it. Both the lost answer and a gateway error end the same way.
func TestAnUnconfirmedCreateWithAConfiguredIDRecordsNothing(t *testing.T) {
	for name, answer := range map[string]func(*clusterAPI, http.ResponseWriter) bool{
		"lost answer": func(_ *clusterAPI, w http.ResponseWriter) bool { return loseAnswer(t, w) },
		"502": func(api *clusterAPI, w http.ResponseWriter) bool {
			w.WriteHeader(http.StatusBadGateway)
			api.write(w, map[string]any{"error": map[string]any{"code": 502, "message": "bad gateway"}})
			return true
		},
	} {
		t.Run(name, func(t *testing.T) {
			api := newClusterAPI(t)
			api.readsFail = true
			api.answer = func(_ int, _ string, w http.ResponseWriter) bool { return answer(api, w) }
			resp := createCluster(t, api.serve(), "c1")
			if !resp.Diagnostics.HasError() {
				t.Fatal("an unconfirmed create reported success")
			}
			if !resp.State.Raw.IsNull() {
				t.Errorf("state was written for a configured id this create could not confirm: %v", resp.State.Raw)
			}
			if len(api.sent) != 1 {
				t.Errorf("creates sent = %v, want one", api.sent)
			}
		})
	}
}

// The next run after that, when the create had landed, stops with the import
// command rather than adopting a cluster it cannot show it made.
func TestAConfiguredIDThatLandedUnconfirmedIsRefusedNextRun(t *testing.T) {
	api := newClusterAPI(t)
	api.tags["c1"] = map[string]string{createTokenTag(): "from-the-earlier-run"}
	resp := createCluster(t, api.serve(), "c1")
	assertRefusedWithImport(t, resp.Diagnostics, "terraform import <address> test/test/c1")
	if !resp.State.Raw.IsNull() {
		t.Errorf("state was written for a cluster this create cannot show it made: %v", resp.State.Raw)
	}
}

// A file store has no tags to carry a token. A configured id that answers 409
// is reported with the import command, never adopted.
func TestAFileStore409WithAConfiguredIDIsRefused(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			if _, err := w.Write([]byte(`{"error":{"code":409,"reason":"CONFLICT_DUPLICATE",` +
				`"message":"file store \"fs1\" already exists in project"}}`)); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		if err := json.NewEncoder(w).Encode(fileStoreBody("active")); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()
	r := &fileStoreResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	raw, s := testValue(t, r, fileStorePlan(nil))
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(raw.Type(), nil), Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}}, &resp)
	assertRefusedWithImport(t, resp.Diagnostics, "terraform import <address> test/elsewhere/fs1")
	if !resp.State.Raw.IsNull() {
		t.Errorf("state was written for a store this create cannot show it made: %v", resp.State.Raw)
	}
}

// The platform names the resource by the id it was sent. An answer that names
// another one is not taken at its word: adopting it would put an id in state
// that this create never asked for, and a later destroy would delete whatever
// it names. The create is refused, with both ids, and nothing is recorded.
func TestAnAnswerNamingAnotherIDIsRefused(t *testing.T) {
	for name, answer := range map[string]func(*clusterAPI, string, http.ResponseWriter){
		"201": func(api *clusterAPI, _ string, w http.ResponseWriter) {
			w.WriteHeader(http.StatusCreated)
			api.write(w, api.cluster("someone-else"))
		},
		"504": func(api *clusterAPI, _ string, w http.ResponseWriter) {
			w.WriteHeader(http.StatusGatewayTimeout)
			api.write(w, map[string]any{"error": map[string]any{
				"code": 504, "message": "deadline exceeded", "reason": "DEADLINE_EXCEEDED",
				"details": []any{map[string]any{"reason": "DEADLINE_EXCEEDED",
					"metadata": map[string]any{"clusterId": "someone-else"}}},
			}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			for _, configured := range []string{"", "c1"} {
				api := newClusterAPI(t)
				api.answer = func(_ int, id string, w http.ResponseWriter) bool {
					answer(api, id, w)
					return true
				}
				resp := createCluster(t, api.serve(), configured)
				if !resp.State.Raw.IsNull() {
					t.Errorf("configured %q: state was written for an id the create never sent: %v",
						configured, resp.State.Raw)
				}
				found := false
				for _, d := range resp.Diagnostics.Errors() {
					found = found || (strings.Contains(d.Summary(), "named another") &&
						strings.Contains(d.Detail(), `"someone-else"`) &&
						strings.Contains(d.Detail(), `"`+api.sent[0]+`"`))
				}
				if !found {
					t.Errorf("configured %q: no refusal naming both ids in %v", configured, resp.Diagnostics)
				}
				if len(api.sent) != 1 {
					t.Errorf("configured %q: creates sent = %v, want one", configured, api.sent)
				}
			}
		})
	}
}

// A tag PATCH replaces the platform's tags, and the user's never carry the
// create token, so the token the platform holds goes back in with them. A
// removal of every user tag still keeps it, and a cluster that holds none
// gets none made up.
func TestATagUpdateKeepsTheCreateToken(t *testing.T) {
	tok := createTokenTag()
	for _, tc := range []struct {
		name  string
		tags  *map[string]string
		token string
		want  map[string]string // nil for a nil result
	}{
		{"user tags", &map[string]string{"a": "b"}, "t1", map[string]string{"a": "b", tok: "t1"}},
		{"every user tag removed", &map[string]string{}, "t1", map[string]string{tok: "t1"}},
		{"no token held", &map[string]string{"a": "b"}, "", map[string]string{"a": "b"}},
		{"tags not sent", nil, "t1", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := keepCreateToken(tc.tags, tc.token)
			if tc.want == nil {
				if got != nil {
					t.Fatalf("got %v, want no tags sent", *got)
				}
				return
			}
			if got == nil || len(*got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if (*got)[k] != v {
					t.Errorf("%s = %q, want %q", k, (*got)[k], v)
				}
			}
			if tc.tags != nil && tc.token != "" {
				if _, leaked := (*tc.tags)[tok]; leaked {
					t.Error("the user's tags were changed in place")
				}
			}
		})
	}
}
