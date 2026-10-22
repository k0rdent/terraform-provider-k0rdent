package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

func fileStoreBody(state string) map[string]any {
	return map[string]any{
		"id": "fs1", "shareType": "nfs", "capacityGiB": 100, "state": state, "etag": "e1",
		"reconciling": false,
		"selfLink":    "/v1/regions/test/projects/elsewhere/storage/file-stores/fs1",
	}
}

func fileStorePlan(extra map[string]tftypes.Value) map[string]tftypes.Value {
	v := map[string]tftypes.Value{
		"id": str("fs1"), "share_type": str("nfs"), "capacity_gib": tftypes.NewValue(tftypes.Number, 100),
		"region": str("test"), "project": str("elsewhere"),
	}
	for k, x := range extra {
		v[k] = x
	}
	return v
}

// consumerMapType is the `consumers` map as the schema declares it.
func consumerMapType(t *testing.T, r resource.Resource) (tftypes.Map, tftypes.Object) {
	t.Helper()
	var s resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &s)
	m, ok := s.Schema.Attributes["consumers"].GetType().TerraformType(context.Background()).(tftypes.Map)
	if !ok {
		t.Fatal("consumers is not a map")
	}
	obj, ok := m.ElementType.(tftypes.Object)
	if !ok {
		t.Fatal("a consumer is not an object")
	}
	return m, obj
}

// consumersValue builds a `consumers` map with one ipAddress entry per ref.
func consumersValue(t *testing.T, r resource.Resource, refs ...string) tftypes.Value {
	t.Helper()
	m, obj := consumerMapType(t, r)
	elems := map[string]tftypes.Value{}
	for _, ref := range refs {
		v := map[string]tftypes.Value{}
		for name, typ := range obj.AttributeTypes {
			v[name] = tftypes.NewValue(typ, nil)
		}
		v["type"], v["access_level"] = str("ipAddress"), str("rw")
		elems[ref] = tftypes.NewValue(obj, v)
	}
	return tftypes.NewValue(m, elems)
}

// groupConsumerValue builds a `consumers` map with one instanceGroup entry.
func groupConsumerValue(t *testing.T, r resource.Resource, group string) tftypes.Value {
	t.Helper()
	m, obj := consumerMapType(t, r)
	network, ok := obj.AttributeTypes["network"].(tftypes.Object)
	if !ok {
		t.Fatal("network is not an object")
	}
	return tftypes.NewValue(m, map[string]tftypes.Value{group: tftypes.NewValue(obj, map[string]tftypes.Value{
		"type": str("instanceGroup"), "access_level": str("rw"),
		"mount_path": tftypes.NewValue(tftypes.String, nil),
		"network": tftypes.NewValue(network, map[string]tftypes.Value{
			"type": str("ethernet"), "mac_address": tftypes.NewValue(tftypes.String, nil),
			"guid": tftypes.NewValue(tftypes.String, nil),
		}),
	})})
}

func runFileStoreUpdate(r *fileStoreResource, t *testing.T, state, plan map[string]tftypes.Value) resource.UpdateResponse {
	t.Helper()
	stateRaw, s := testValue(t, r, fileStorePlan(state))
	planRaw, _ := testValue(t, r, fileStorePlan(plan))
	resp := resource.UpdateResponse{State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}
	r.Update(context.Background(), resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}, &resp)
	return resp
}

func TestFileStoreCreateKeepsScopeWhenTheWaitFails(t *testing.T) {
	server := failingCreate(t, fileStoreBody("failed"))
	defer server.Close()
	r := &fileStoreResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	assertCreateKeepsScope(t, r, fileStorePlan(nil), func(req resource.CreateRequest, resp *resource.CreateResponse) {
		r.Create(context.Background(), req, resp)
	})
}

// fileStoreCreate504 answers the create with a 504 and every read with an active store.
func fileStoreCreate504(t *testing.T, details string, id tftypes.Value) resource.CreateResponse {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusGatewayTimeout)
			if _, err := w.Write([]byte(`{"error":{"code":504,"message":"timed out","details":` + details + `}}`)); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		got := fileStoreBody("active")
		got["id"] = r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if err := json.NewEncoder(w).Encode(got); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(server.Close)
	r := &fileStoreResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	raw, s := testValue(t, r, fileStorePlan(map[string]tftypes.Value{"id": id}))
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(raw.Type(), nil), Schema: s.Schema}}
	r.Create(context.Background(), resource.CreateRequest{Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}}, &resp)
	return resp
}

// The contract names the store a 504 committed. Dropping it leaves capacity
// nothing tracks, and a retry with the same id answers CONFLICT_DUPLICATE.
func TestFileStoreCreate504IsRecordedAndWaitedOut(t *testing.T) {
	resp := fileStoreCreate504(t, `[{"metadata":{"fileStoreId":"fs1"}}]`, str("fs1"))
	if resp.Diagnostics.HasError() {
		t.Fatalf("a committed store was reported as failed: %v", resp.Diagnostics.Errors())
	}
	var id types.String
	resp.State.GetAttribute(context.Background(), path.Root("id"), &id)
	if id.ValueString() != "fs1" {
		t.Errorf("id in state = %s, want fs1", id)
	}
}

// A 504 that names no store is read back under the id the provider sent. With
// no id configured that id is one this Create generated, which is proof
// enough that the store found under it is this one.
func TestFileStoreCreate504WithNoNameIsReadBackUnderTheGeneratedID(t *testing.T) {
	resp := fileStoreCreate504(t, `[]`, tftypes.NewValue(tftypes.String, nil))
	if resp.Diagnostics.HasError() {
		t.Fatalf("a store under the generated id was not taken up: %v", resp.Diagnostics.Errors())
	}
	var id types.String
	resp.State.GetAttribute(context.Background(), path.Root("id"), &id)
	if !strings.HasPrefix(id.ValueString(), generatedIDPrefix) {
		t.Errorf("id in state = %s, want the generated one", id)
	}
}

// A store with the configured id may already exist and belong to someone else.
// Adopting it on an unnamed 504 would hand it to this configuration, and a
// later destroy would delete it.
func TestFileStoreCreate504AdoptsOnlyTheNamedStore(t *testing.T) {
	resp := fileStoreCreate504(t, `[]`, str("fs1"))
	if !resp.Diagnostics.HasError() {
		t.Fatal("an unnamed 504 was taken as the configured store")
	}
	if !resp.State.Raw.IsNull() {
		t.Errorf("state was written for a store the platform did not name: %v", resp.State.Raw)
	}
}

// "Send an empty string to clear it." Sending nothing kept the old value and
// failed every apply after.
func TestFileStoreUpdateClearsRemovedMetadata(t *testing.T) {
	var rec patchRecorder
	server := rec.server(t, fileStoreBody("active"))
	defer server.Close()
	r := &fileStoreResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	runFileStoreUpdate(r, t, map[string]tftypes.Value{
		"display_name": str("old"), "description": str("old"), "etag": str("e1"),
	}, map[string]tftypes.Value{"etag": str("e1")})
	for _, field := range []string{"displayName", "description"} {
		if got, ok := rec.sent[field]; !ok || got != "" {
			t.Errorf("%s = %#v in the PATCH, want an empty string", field, got)
		}
	}
}

// Re-sending an instance-group consumer resolves the group again, so an update
// that does not change the consumers must not send them.
func TestFileStoreUpdateLeavesUnchangedConsumersAlone(t *testing.T) {
	r := &fileStoreResource{}
	for name, consumers := range map[string]tftypes.Value{
		"address": consumersValue(t, r, "10.0.0.31"),
		"group":   groupConsumerValue(t, r, "trainers"),
	} {
		t.Run(name, func(t *testing.T) {
			var rec patchRecorder
			server := rec.server(t, fileStoreBody("active"))
			defer server.Close()
			r.meta = testClusterMeta(t, server.URL, time.Minute)
			runFileStoreUpdate(r, t,
				map[string]tftypes.Value{"etag": str("e1"), "consumers": consumers},
				map[string]tftypes.Value{
					"etag": str("e1"), "consumers": consumers, "capacity_gib": tftypes.NewValue(tftypes.Number, 200),
				})
			if _, sent := rec.sent["consumers"]; sent {
				t.Errorf("consumers were re-sent on a capacity-only change: %v", rec.sent["consumers"])
			}
		})
	}
}

// A configuration that builds `consumers` from another resource's attributes
// has an unknown set until apply. Reading it must not fail the plan.
func TestFileStoreAcceptsUnknownConsumers(t *testing.T) {
	r := &fileStoreResource{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)}
	m, _ := consumerMapType(t, r)
	unknown := tftypes.NewValue(m, tftypes.UnknownValue)

	raw, s := testValue(t, r, fileStorePlan(map[string]tftypes.Value{"consumers": unknown}))
	var validate resource.ValidateConfigResponse
	r.ValidateConfig(context.Background(), resource.ValidateConfigRequest{
		Config: tfsdk.Config{Raw: raw, Schema: s.Schema},
	}, &validate)
	if validate.Diagnostics.HasError() {
		t.Fatalf("ValidateConfig: %v", validate.Diagnostics.Errors())
	}

	stateRaw, _ := testValue(t, r, fileStorePlan(nil))
	plan := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: raw, Schema: s.Schema},
		Plan:   tfsdk.Plan{Raw: raw, Schema: s.Schema},
		State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
	}, &plan)
	if plan.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan: %v", plan.Diagnostics.Errors())
	}
}

// `cond ? {...} : null` leaves a null entry in the map. It panicked the
// provider, and skipping it would plan a consumer the platform never reports.
func TestFileStoreRefusesANullConsumer(t *testing.T) {
	r := &fileStoreResource{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)}
	m, obj := consumerMapType(t, r)
	consumers := tftypes.NewValue(m, map[string]tftypes.Value{"10.0.0.31": tftypes.NewValue(obj, nil)})

	raw, s := testValue(t, r, fileStorePlan(map[string]tftypes.Value{"consumers": consumers}))
	var validate resource.ValidateConfigResponse
	r.ValidateConfig(context.Background(), resource.ValidateConfigRequest{
		Config: tfsdk.Config{Raw: raw, Schema: s.Schema},
	}, &validate)
	found := false
	for _, d := range validate.Diagnostics.Errors() {
		found = found || strings.Contains(d.Summary(), "null")
	}
	if !found {
		t.Errorf("a null consumer was not refused: %v", validate.Diagnostics.Errors())
	}
}

// storeAPI serves one store through its delete. reads counts GETs, so a test
// can hold the store in a state for a while; patch and remove answer the PATCH
// and the DELETE, calls records the method of every request in order, and
// patches records every PATCH body.
type storeAPI struct {
	mu      sync.Mutex
	calls   []string
	patches []map[string]any
	query   []string
	gone    bool
	reads   int
	get     func(reads int) map[string]any
	patch   func(w http.ResponseWriter)
	remove  func(w http.ResponseWriter) bool
}

func (a *storeAPI) serve(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.calls = append(a.calls, r.Method)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if r.Method == http.MethodPatch {
			sent := map[string]any{}
			if err := json.Unmarshal(raw, &sent); err != nil {
				t.Errorf("PATCH body: %v", err)
			}
			a.patches = append(a.patches, sent)
		}
		w.Header().Set("Content-Type", "application/json")
		writeBody := func(status int, body string) {
			w.WriteHeader(status)
			if _, err := w.Write([]byte(body)); err != nil {
				t.Errorf("write: %v", err)
			}
		}
		switch {
		case a.gone:
			writeBody(http.StatusNotFound, `{"error":{"code":404,"message":"no such file store"}}`)
		case r.Method == http.MethodGet:
			a.reads++
			if err := json.NewEncoder(w).Encode(a.get(a.reads)); err != nil {
				t.Errorf("write: %v", err)
			}
		case r.Method == http.MethodPatch:
			a.patch(w)
		case r.Method == http.MethodDelete:
			a.query = append(a.query, r.URL.RawQuery)
			a.gone = a.remove(w)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func deleteFileStore(t *testing.T, url string, state map[string]tftypes.Value) resource.DeleteResponse {
	t.Helper()
	r := &fileStoreResource{meta: testClusterMeta(t, url, 2*time.Second)}
	raw, s := testValue(t, r, fileStorePlan(state))
	st := tfsdk.State{Raw: raw, Schema: s.Schema}
	resp := resource.DeleteResponse{State: st}
	r.Delete(context.Background(), resource.DeleteRequest{State: st}, &resp)
	return resp
}

func withConsumers(state string, refs ...string) map[string]any {
	body := fileStoreBody(state)
	consumers := []any{}
	for _, ref := range refs {
		consumers = append(consumers, map[string]any{"ref": ref})
	}
	body["consumers"] = consumers
	return body
}

func conflict(t *testing.T) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusConflict)
		if _, err := w.Write([]byte(`{"error":{"code":409,"message":"in use","details":[{"reason":"CONFLICT_IN_USE"}]}}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}
}

func accepted(w http.ResponseWriter) bool {
	w.WriteHeader(http.StatusNoContent)
	return true
}

// The destroy detaches and then deletes, and never with `force=true`, which
// would leave the backing storage and its data behind.
func TestFileStoreDeleteDetachesFirstAndNeverForces(t *testing.T) {
	patched := false
	api := &storeAPI{
		get: func(int) map[string]any {
			if patched {
				return fileStoreBody("active")
			}
			return withConsumers("active", "10.0.0.31")
		},
		patch: func(w http.ResponseWriter) {
			patched = true
			if err := json.NewEncoder(w).Encode(fileStoreBody("active")); err != nil {
				t.Errorf("write: %v", err)
			}
		},
		remove: accepted,
	}
	resp := deleteFileStore(t, api.serve(t).URL, map[string]tftypes.Value{
		"state": str("active"), "consumers": consumersValue(t, &fileStoreResource{}, "10.0.0.31"),
	})
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", resp.Diagnostics.Errors())
	}
	if got := strings.Join(api.calls, " "); !strings.HasPrefix(got, "GET PATCH") || !strings.Contains(got, "DELETE") {
		t.Errorf("requests = %s, want the PATCH before the DELETE", got)
	}
	for _, q := range api.query {
		if strings.Contains(q, "force") {
			t.Errorf("the DELETE carried %q", q)
		}
	}
}

// A store refreshed as active may list a consumer state does not know about.
// The detach is decided from the platform, not from state.
func TestFileStoreDeleteDetachesWhatThePlatformLists(t *testing.T) {
	patched := false
	api := &storeAPI{
		get: func(int) map[string]any {
			if patched {
				return fileStoreBody("active")
			}
			return withConsumers("active", "10.0.0.99")
		},
		patch: func(w http.ResponseWriter) {
			patched = true
			if err := json.NewEncoder(w).Encode(fileStoreBody("active")); err != nil {
				t.Errorf("write: %v", err)
			}
		},
		remove: func(w http.ResponseWriter) bool {
			if !patched {
				conflict(t)(w)
				return false
			}
			return accepted(w)
		},
	}
	resp := deleteFileStore(t, api.serve(t).URL, map[string]tftypes.Value{"state": str("active")})
	if resp.Diagnostics.HasError() || !patched {
		t.Errorf("patched=%v diags=%v", patched, resp.Diagnostics.Errors())
	}
}

// A failed store's grants were never enforced and the consumer check is on
// `active` alone, so the detach PATCH would only be refused and block a delete
// the platform accepts.
func TestFileStoreDeleteOfAFailedStoreSkipsTheDetach(t *testing.T) {
	patched := false
	api := &storeAPI{
		get: func(int) map[string]any { return withConsumers("failed", "10.0.0.31") },
		patch: func(w http.ResponseWriter) {
			patched = true
			conflict(t)(w)
		},
		remove: accepted,
	}
	resp := deleteFileStore(t, api.serve(t).URL, map[string]tftypes.Value{
		"state": str("failed"), "consumers": consumersValue(t, &fileStoreResource{}, "10.0.0.31"),
	})
	if resp.Diagnostics.HasError() || patched || !api.gone {
		t.Errorf("patched=%v deleted=%v diags=%v", patched, api.gone, resp.Diagnostics.Errors())
	}
}

// A store whose create timed out is tainted while still `creating`, and the
// replacement destroys it. Both the detach and the delete refuse a creating
// store, so the destroy waits for it to settle first.
func TestFileStoreDeleteWaitsOutACreatingStore(t *testing.T) {
	creating := func(reads int) bool { return reads <= 3 }
	patched := false
	var api *storeAPI
	api = &storeAPI{
		get: func(reads int) map[string]any {
			switch {
			case creating(reads):
				return withConsumers("creating", "10.0.0.31")
			case patched:
				return fileStoreBody("active")
			}
			return withConsumers("active", "10.0.0.31")
		},
		patch: func(w http.ResponseWriter) {
			if creating(api.reads) {
				conflict(t)(w)
				return
			}
			patched = true
			if err := json.NewEncoder(w).Encode(fileStoreBody("active")); err != nil {
				t.Errorf("write: %v", err)
			}
		},
		remove: func(w http.ResponseWriter) bool {
			if creating(api.reads) || !patched {
				conflict(t)(w)
				return false
			}
			return accepted(w)
		},
	}
	resp := deleteFileStore(t, api.serve(t).URL, map[string]tftypes.Value{
		"state": str("creating"), "consumers": consumersValue(t, &fileStoreResource{}, "10.0.0.31"),
	})
	if resp.Diagnostics.HasError() || !api.gone {
		t.Errorf("deleted=%v diags=%v", api.gone, resp.Diagnostics.Errors())
	}
}

// A failed store refused for a reason teardown cannot clear is reported on
// the first refusal, not after the whole delete timeout.
func TestFileStoreDeleteReportsAPermanentRefusalAtOnce(t *testing.T) {
	api := &storeAPI{
		get:   func(int) map[string]any { return fileStoreBody("failed") },
		patch: conflict(t),
		remove: func(w http.ResponseWriter) bool {
			conflict(t)(w)
			return false
		},
	}
	resp := deleteFileStore(t, api.serve(t).URL, map[string]tftypes.Value{"state": str("failed")})
	if !resp.Diagnostics.HasError() {
		t.Fatal("the refusal was not reported")
	}
	if len(api.query) != 1 {
		t.Errorf("the DELETE was sent %d times, want once", len(api.query))
	}
}

// A 404 on the pre-delete detach must let the delete go ahead.
func TestDetachConsumersProceedsWhenThePatchIs404(t *testing.T) {
	var patched bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patched = true
		}
		w.WriteHeader(http.StatusNotFound)
		if _, err := w.Write([]byte(`{"error":{"code":404,"message":"no such file store"}}`)); err != nil {
			t.Errorf("writing the error body: %v", err)
		}
	}))
	defer api.Close()

	r := &fileStoreResource{meta: testClusterMeta(t, api.URL, 2*time.Second)}
	var diags diag.Diagnostics
	proceed := r.detachConsumers(context.Background(),
		clusterTarget{region: "test", project: "test"}, "fs-1", 100, nil, &diags)
	if !patched {
		t.Fatal("detachConsumers never sent the PATCH")
	}
	if diags.HasError() || !proceed {
		t.Fatalf("a 404 on the detach stopped the delete: proceed=%v %v", proceed, diags.Errors())
	}
}

// statusValue builds a `consumer_status` map with one failed entry.
func statusValue(t *testing.T, r resource.Resource, ref, reason string) tftypes.Value {
	t.Helper()
	var s resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &s)
	statusType, ok := s.Schema.Attributes["consumer_status"].GetType().TerraformType(context.Background()).(tftypes.Map)
	if !ok {
		t.Fatal("consumer_status is not a map")
	}
	elem, ok := statusType.ElementType.(tftypes.Object)
	if !ok {
		t.Fatal("a consumer status is not an object")
	}
	return tftypes.NewValue(statusType, map[string]tftypes.Value{
		ref: tftypes.NewValue(elem, map[string]tftypes.Value{
			"state": str("failed"), "state_reason": str(reason),
			"resolved_addresses": tftypes.NewValue(elem.AttributeTypes["resolved_addresses"], nil),
		}),
	})
}

// A plan that could not read the keys reaches Update unchecked, and sending a
// group the platform calls no longer grantable grants whatever its id names now.
func TestFileStoreUpdateRefusesAnUngrantableGroup(t *testing.T) {
	var rec patchRecorder
	server := rec.server(t, fileStoreBody("active"))
	defer server.Close()
	r := &fileStoreResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	resp := runFileStoreUpdate(r, t,
		map[string]tftypes.Value{
			"etag": str("e1"), "consumers": groupConsumerValue(t, r, "trainers"),
			"consumer_status": statusValue(t, r, "trainers", "GROUP_NO_LONGER_GRANTABLE"),
		},
		map[string]tftypes.Value{
			"etag": str("e1"), "consumers": mergeConsumers(t, r, "trainers", "10.0.0.31"),
		})
	if !resp.Diagnostics.HasError() || rec.sent != nil {
		t.Errorf("sent=%v diags=%v", rec.sent, resp.Diagnostics.Errors())
	}
}

// A plan that could not read the keys can resolve them to exactly what state
// holds. Then no consumer changed, but a drifted group still asks for a
// re-send, and a re-send carries every entry, the ungrantable one included.
func TestFileStoreUpdateNeverResendsAnUngrantableGroup(t *testing.T) {
	var rec patchRecorder
	server := rec.server(t, fileStoreBody("active"))
	defer server.Close()
	r := &fileStoreResource{meta: testClusterMeta(t, server.URL, time.Minute)}

	consumers := twoGroupConsumers(t, r, "ig-a", "ig-b")
	status := ungrantableAndDrifted(t, r, "ig-a", "ig-b")
	resp := runFileStoreUpdate(r, t,
		map[string]tftypes.Value{"etag": str("e1"), "consumers": consumers, "consumer_status": status},
		map[string]tftypes.Value{"etag": str("e1"), "consumers": consumers})
	if rec.sent != nil {
		t.Errorf("a PATCH was sent: %v", rec.sent)
	}
	refused := false
	for _, d := range resp.Diagnostics.Errors() {
		refused = refused || d.Summary() == "A consumer group can no longer be granted"
	}
	if !refused {
		t.Errorf("the update did not refuse the ungrantable group: %v", resp.Diagnostics.Errors())
	}
}

// Taking the ungrantable group out of configuration is the way past the
// refusal, so that update has to go through and send the rest without it.
func TestFileStoreUpdateDropsARemovedUngrantableGroup(t *testing.T) {
	var rec patchRecorder
	server := rec.server(t, fileStoreBody("active"))
	defer server.Close()
	r := &fileStoreResource{meta: testClusterMeta(t, server.URL, time.Minute)}

	resp := runFileStoreUpdate(r, t,
		map[string]tftypes.Value{
			"etag": str("e1"), "consumers": twoGroupConsumers(t, r, "ig-a", "ig-b"),
			"consumer_status": statusValue(t, r, "ig-a", "GROUP_NO_LONGER_GRANTABLE"),
		},
		map[string]tftypes.Value{"etag": str("e1"), "consumers": groupConsumerValue(t, r, "ig-b")})
	if resp.Diagnostics.HasError() {
		t.Fatalf("removing the ungrantable group was refused: %v", resp.Diagnostics.Errors())
	}
	sent, ok := rec.sent["consumers"]
	if !ok {
		t.Fatalf("no PATCH carried the consumers: %v", rec.sent)
	}
	raw, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"ig-a"`) || !strings.Contains(string(raw), `"ig-b"`) {
		t.Errorf("consumers = %s, want ig-b without ig-a", raw)
	}
}

// ungrantableAndDrifted is a `consumer_status` with one group the platform
// calls no longer grantable and one whose membership drifted.
func ungrantableAndDrifted(t *testing.T, r resource.Resource, ungrantable, drifted string) tftypes.Value {
	t.Helper()
	status := statusValue(t, r, ungrantable, "GROUP_NO_LONGER_GRANTABLE")
	var entries map[string]tftypes.Value
	if err := status.As(&entries); err != nil {
		t.Fatal(err)
	}
	var other map[string]tftypes.Value
	if err := statusValue(t, r, drifted, "GROUP_MEMBERSHIP_DRIFTED").As(&other); err != nil {
		t.Fatal(err)
	}
	entries[drifted] = other[drifted]
	return tftypes.NewValue(status.Type(), entries)
}

// twoGroupConsumers is two instanceGroup entries in one map.
func twoGroupConsumers(t *testing.T, r resource.Resource, a, b string) tftypes.Value {
	t.Helper()
	m, _ := consumerMapType(t, r)
	elems := map[string]tftypes.Value{}
	for _, group := range []string{a, b} {
		var each map[string]tftypes.Value
		if err := groupConsumerValue(t, r, group).As(&each); err != nil {
			t.Fatal(err)
		}
		elems[group] = each[group]
	}
	return tftypes.NewValue(m, elems)
}

// mergeConsumers is a group entry plus address entries in one map.
func mergeConsumers(t *testing.T, r resource.Resource, group string, refs ...string) tftypes.Value {
	t.Helper()
	m, _ := consumerMapType(t, r)
	elems := map[string]tftypes.Value{}
	for _, v := range []tftypes.Value{groupConsumerValue(t, r, group), consumersValue(t, r, refs...)} {
		var each map[string]tftypes.Value
		if err := v.As(&each); err != nil {
			t.Fatal(err)
		}
		for k, e := range each {
			elems[k] = e
		}
	}
	return tftypes.NewValue(m, elems)
}

// A drifted group plans an update the user did not write, so every value the
// re-send can move has to be unknown, or a changed one fails the apply.
func TestFileStoreDriftPlansEveryServerValueUnknown(t *testing.T) {
	r := &fileStoreResource{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)}
	var s resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &s)
	status := statusValue(t, r, "trainers", "GROUP_MEMBERSHIP_DRIFTED")
	consumers := groupConsumerValue(t, r, "trainers")
	stateRaw, _ := testValue(t, r, fileStorePlan(map[string]tftypes.Value{
		"consumers": consumers, "consumer_status": status, "state": str("active"), "etag": str("e1"),
	}))
	plan := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Raw: stateRaw, Schema: s.Schema}}
	r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
		Config: tfsdk.Config{Raw: stateRaw, Schema: s.Schema},
		Plan:   tfsdk.Plan{Raw: stateRaw, Schema: s.Schema},
		State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
	}, &plan)
	if plan.Diagnostics.HasError() {
		t.Fatalf("ModifyPlan: %v", plan.Diagnostics.Errors())
	}
	for _, name := range []string{"consumer_status", "export_locations", "preferred_export_location", "etag"} {
		var v attr.Value
		plan.Plan.GetAttribute(context.Background(), path.Root(name), &v)
		if !v.IsUnknown() {
			t.Errorf("%s is %s in the plan, want unknown", name, v)
		}
	}
	// The update waits for the store to settle, so a settled one stays so.
	var state types.String
	plan.Plan.GetAttribute(context.Background(), path.Root("state"), &state)
	if state.ValueString() != "active" {
		t.Errorf("state is %s in the plan, want \"active\"", state)
	}
}

// The platform declares no order for export locations, so the same locations
// in another order must read back as the same state.
func TestFileStoreExportLocationsIgnoreTheResponseOrder(t *testing.T) {
	nfs := k0rdent.ExportLocationProtocol("nfs")
	yes := true
	a := k0rdent.ExportLocation{Server: ptr("10.97.0.20"), Path: ptr("/exports/fs1"), Protocol: &nfs, AdminOnly: &yes}
	b := k0rdent.ExportLocation{Server: ptr("10.96.0.20"), Path: ptr("/exports/fs1"), Protocol: &nfs, Preferred: &yes}
	read := func(order ...k0rdent.ExportLocation) fileStoreModel {
		t.Helper()
		got, diags := fileStoreStateFrom(context.Background(),
			k0rdent.FileStore{Id: "fs1", ExportLocations: &order}, fileStoreModel{})
		if diags.HasError() {
			t.Fatal(diags.Errors())
		}
		return got
	}
	first, second := read(a, b), read(b, a)
	if !first.ExportLocations.Equal(second.ExportLocations) {
		t.Errorf("a reordered response changed export_locations:\n%v\n%v", first.ExportLocations, second.ExportLocations)
	}
	server := first.PreferredExport.Attributes()["server"]
	if server == nil || server.String() != `"10.96.0.20"` {
		t.Errorf("preferred_export_location = %v, want the preferred tenant location", first.PreferredExport)
	}
}

// An entry with no ref has no key to live under. Keyed as "", two of them
// would overwrite each other and one would shadow the other's state.
func TestFileStoreStateSkipsAConsumerStatusWithNoRef(t *testing.T) {
	var fs k0rdent.FileStore
	if err := json.Unmarshal([]byte(`{"id":"fs1","consumerStatus":[
		{"ref":"10.0.1.7","state":"active"},
		{"state":"failed"},
		{"ref":"","state":"creating"}]}`), &fs); err != nil {
		t.Fatal(err)
	}
	got, diags := fileStoreStateFrom(context.Background(), fs, fileStoreModel{})
	if diags.HasError() {
		t.Fatal(diags.Errors())
	}
	if keys := got.ConsumerStatus.Elements(); len(keys) != 1 || keys["10.0.1.7"] == nil {
		t.Errorf("consumer_status = %v, want only the entry with a ref", got.ConsumerStatus)
	}
}

// freshPrivate gives a request or response the private state the framework
// would. Its type is internal to the framework, so it is built by reflection.
func freshPrivate[T any](t *testing.T, field *T, marker map[string]string) {
	t.Helper()
	v := reflect.ValueOf(field).Elem()
	v.Set(reflect.New(v.Type().Elem()))
	if marker == nil {
		return
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	w, ok := any(*field).(privateWriter)
	if !ok {
		t.Fatal("private state cannot be written")
	}
	if d := w.SetKey(context.Background(), refusedDriftKey, raw); d.HasError() {
		t.Fatal(d)
	}
}

// refusedIn is the refusal memory a response leaves in private state.
func refusedIn(t *testing.T, private any) refusedResends {
	t.Helper()
	p, ok := private.(privateState)
	if !ok {
		t.Fatal("private state cannot be read")
	}
	var diags diag.Diagnostics
	got := readRefused(context.Background(), p, &diags)
	if diags.HasError() {
		t.Fatal(diags)
	}
	return got
}

// driftedStatus is a `consumer_status` map with one drifted group and the
// addresses its grant still covers.
func driftedStatus(t *testing.T, r resource.Resource, ref string, addrs ...string) tftypes.Value {
	t.Helper()
	var s resource.SchemaResponse
	r.Schema(context.Background(), resource.SchemaRequest{}, &s)
	statusType, ok := s.Schema.Attributes["consumer_status"].GetType().TerraformType(context.Background()).(tftypes.Map)
	if !ok {
		t.Fatal("consumer_status is not a map")
	}
	elem, ok := statusType.ElementType.(tftypes.Object)
	if !ok {
		t.Fatal("a consumer status is not an object")
	}
	addrType := elem.AttributeTypes["resolved_addresses"]
	values := []tftypes.Value{}
	for _, a := range addrs {
		values = append(values, str(a))
	}
	return tftypes.NewValue(statusType, map[string]tftypes.Value{
		ref: tftypes.NewValue(elem, map[string]tftypes.Value{
			"state": str("failed"), "state_reason": str("GROUP_MEMBERSHIP_DRIFTED"),
			"resolved_addresses": tftypes.NewValue(addrType, values),
		}),
	})
}

// updateWithPrivate runs a capacity change against a store whose group
// drifted, with prior holding the refusals already remembered.
func updateWithPrivate(t *testing.T, url string, prior map[string]string) resource.UpdateResponse {
	t.Helper()
	return updateDrifted(t, url, prior, 200)
}

// updateDrifted runs an update to capacity against a store whose group
// drifted. A capacity of 100, the store's own, makes the re-send the whole
// change.
func updateDrifted(t *testing.T, url string, prior map[string]string, capacity int) resource.UpdateResponse {
	t.Helper()
	r := &fileStoreResource{meta: testClusterMeta(t, url, 2*time.Second)}
	consumers := groupConsumerValue(t, r, "trainers")
	status := driftedStatus(t, r, "trainers", "10.0.1.8", "10.0.1.7")
	stateRaw, s := testValue(t, r, fileStorePlan(map[string]tftypes.Value{
		"etag": str("e1"), "consumers": consumers, "consumer_status": status,
	}))
	planRaw, _ := testValue(t, r, fileStorePlan(map[string]tftypes.Value{
		"etag": str("e1"), "consumers": consumers, "capacity_gib": tftypes.NewValue(tftypes.Number, capacity),
	}))
	req := resource.UpdateRequest{
		State: tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		Plan:  tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
	}
	resp := resource.UpdateResponse{State: tfsdk.State{Raw: stateRaw, Schema: s.Schema}}
	// The framework hands the response a copy of the prior private state.
	freshPrivate(t, &req.Private, prior)
	freshPrivate(t, &resp.Private, prior)
	r.Update(context.Background(), req, &resp)
	return resp
}

// A re-send only drift asked for, refused with 422, is retried without
// `consumers`, and the refusal is remembered against the addresses the group
// had, so the next plan holds it back.
func TestFileStoreUpdateRemembersARefusedResend(t *testing.T) {
	api := &storeAPI{
		get: func(int) map[string]any { return fileStoreBody("active") },
	}
	api.patch = func(w http.ResponseWriter) {
		if len(api.patches) == 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
			if _, err := w.Write([]byte(`{"error":{"code":422,"message":"a member cannot be vouched for"}}`)); err != nil {
				t.Errorf("write: %v", err)
			}
			return
		}
		if err := json.NewEncoder(w).Encode(fileStoreBody("active")); err != nil {
			t.Errorf("write: %v", err)
		}
	}
	resp := updateWithPrivate(t, api.serve(t).URL, map[string]string{"evaluators": "10.0.2.1"})
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics.Errors())
	}
	if len(api.patches) != 2 {
		t.Fatalf("%d PATCHes, want the re-send and the retry", len(api.patches))
	}
	if _, sent := api.patches[0]["consumers"]; !sent {
		t.Error("the first PATCH did not re-send the drifted group")
	}
	if _, sent := api.patches[1]["consumers"]; sent {
		t.Error("the retry sent `consumers` again")
	}
	want := refusedResends{"evaluators": "10.0.2.1", "trainers": "10.0.1.7,10.0.1.8"}
	if got := refusedIn(t, resp.Private); !reflect.DeepEqual(got, want) {
		t.Errorf("remembered %v, want %v", got, want)
	}
	if resp.Diagnostics.WarningsCount() != 1 ||
		!strings.Contains(resp.Diagnostics.Warnings()[0].Detail(), "a member cannot be vouched for") {
		t.Errorf("the refusal is not reported by its message: %v", resp.Diagnostics)
	}
}

// A refusal remembered against addresses the group has since left is no
// longer held. The re-send goes, and once it is accepted only that group's
// refusal is forgotten.
func TestFileStoreUpdateForgetsAResendThatWentThrough(t *testing.T) {
	api := &storeAPI{
		get: func(int) map[string]any { return fileStoreBody("active") },
	}
	api.patch = func(w http.ResponseWriter) {
		if err := json.NewEncoder(w).Encode(fileStoreBody("active")); err != nil {
			t.Errorf("write: %v", err)
		}
	}
	resp := updateWithPrivate(t, api.serve(t).URL, map[string]string{
		"trainers": "10.0.1.6", "evaluators": "10.0.2.1",
	})
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics.Errors())
	}
	if len(api.patches) != 1 {
		t.Fatalf("%d PATCHes, want 1", len(api.patches))
	}
	if _, sent := api.patches[0]["consumers"]; !sent {
		t.Error("the drifted group was not re-sent")
	}
	want := refusedResends{"evaluators": "10.0.2.1"}
	if got := refusedIn(t, resp.Private); !reflect.DeepEqual(got, want) {
		t.Errorf("remembered %v after the re-send, want %v", got, want)
	}
}

// unprocessable answers a PATCH the way the platform refuses a member it
// cannot vouch for.
func unprocessable(t *testing.T, w http.ResponseWriter) {
	w.WriteHeader(http.StatusUnprocessableEntity)
	if _, err := w.Write([]byte(`{"error":{"code":422,"message":"a member cannot be vouched for"}}`)); err != nil {
		t.Errorf("write: %v", err)
	}
}

// When the re-send is the whole change, a 422 can only be about it. A second
// PATCH would carry nothing but the unchanged capacity and the etag, a write
// nobody asked for, so none is sent.
func TestFileStoreUpdateSendsNothingMoreWhenTheResendWasTheWholeChange(t *testing.T) {
	api := &storeAPI{
		get: func(int) map[string]any { return fileStoreBody("active") },
	}
	// The platform refuses the re-send and accepts anything without it.
	api.patch = func(w http.ResponseWriter) {
		if _, resend := api.patches[len(api.patches)-1]["consumers"]; resend {
			unprocessable(t, w)
			return
		}
		if err := json.NewEncoder(w).Encode(fileStoreBody("active")); err != nil {
			t.Errorf("write: %v", err)
		}
	}
	resp := updateDrifted(t, api.serve(t).URL, nil, 100)
	if resp.Diagnostics.HasError() {
		t.Fatalf("Update: %v", resp.Diagnostics.Errors())
	}
	if len(api.patches) != 1 {
		t.Fatalf("%d PATCHes, want only the re-send: %v", len(api.patches), api.patches)
	}
	want := refusedResends{"trainers": "10.0.1.7,10.0.1.8"}
	if got := refusedIn(t, resp.Private); !reflect.DeepEqual(got, want) {
		t.Errorf("remembered %v, want %v", got, want)
	}
	if !hasWarning(resp.Diagnostics, "a member cannot be vouched for") {
		t.Errorf("the refusal is not reported by its message: %v", resp.Diagnostics)
	}
}

// When the PATCH without the re-send is refused too, its answer is the one
// about the user's change, so that's what's reported. Reporting the 422 would
// blame the drifted group for a refusal it may have had nothing to do with,
// and nothing is remembered, since the 422 was never shown to be the re-send's.
func TestFileStoreUpdateReportsTheRetrysOwnRefusal(t *testing.T) {
	api := &storeAPI{
		get: func(int) map[string]any { return fileStoreBody("active") },
	}
	api.patch = func(w http.ResponseWriter) {
		if len(api.patches) == 1 {
			unprocessable(t, w)
			return
		}
		w.WriteHeader(http.StatusConflict)
		if _, err := w.Write([]byte(`{"error":{"code":409,"message":"the store changed since it was read",` +
			`"details":[{"reason":"CONFLICT_VERSION"}]}}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}
	resp := updateWithPrivate(t, api.serve(t).URL, nil)
	if !resp.Diagnostics.HasError() {
		t.Fatal("a refused change reported success")
	}
	detail := resp.Diagnostics.Errors()[0].Detail()
	if !strings.Contains(detail, "the store changed since it was read") || strings.Contains(detail, "vouched") {
		t.Errorf("the error reports the wrong refusal: %s", detail)
	}
	if got := refusedIn(t, resp.Private); len(got) != 0 {
		t.Errorf("remembered %v from a 422 nobody showed was the re-send's", got)
	}
}
