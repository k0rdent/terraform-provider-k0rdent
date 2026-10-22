package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// A destroy warns through the plan, and a replacement deletes the store as
// surely, so it warns the same way, once. A change made in place deletes
// nothing, so it doesn't.
func TestFileStoreReplacementWarnsAboutEnforcedConsumers(t *testing.T) {
	r := guardedModifier(t, &fileStoreResource{meta: testClusterMeta(t, "https://unused.invalid", time.Minute)},
		fileStoreTenantState)
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
	addrType, ok := elem.AttributeTypes["resolved_addresses"].(tftypes.Set)
	if !ok {
		t.Fatal("resolved_addresses is not a set")
	}
	status := tftypes.NewValue(statusType, map[string]tftypes.Value{
		"10.0.1.7": tftypes.NewValue(elem, map[string]tftypes.Value{
			"state": str("active"), "state_reason": tftypes.NewValue(tftypes.String, nil),
			"resolved_addresses": tftypes.NewValue(addrType, []tftypes.Value{str("10.0.1.7")}),
		}),
	})
	consumers := consumersValue(t, r, "10.0.1.7")
	stateRaw, _ := testValue(t, r, fileStorePlan(map[string]tftypes.Value{
		"consumers": consumers, "consumer_status": status, "state": str("active"), "etag": str("e1"),
	}))
	modifyPlan := func(t *testing.T, change map[string]tftypes.Value) diag.Diagnostics {
		t.Helper()
		planned := map[string]tftypes.Value{
			"consumers": consumers, "consumer_status": status, "state": str("active"), "etag": str("e1"),
		}
		for k, v := range change {
			planned[k] = v
		}
		planRaw, _ := testValue(t, r, fileStorePlan(planned))
		resp := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Raw: planRaw, Schema: s.Schema}}
		r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
			Config: tfsdk.Config{Raw: planRaw, Schema: s.Schema},
			Plan:   tfsdk.Plan{Raw: planRaw, Schema: s.Schema},
			State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		}, &resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("ModifyPlan: %v", resp.Diagnostics.Errors())
		}
		return resp.Diagnostics
	}

	t.Run("replacement", func(t *testing.T) {
		// Permitted, because the warning belongs to a replacement that goes ahead.
		diags := modifyPlan(t, map[string]tftypes.Value{
			"share_type": str("high-speed"), "allow_replace": tftypes.NewValue(tftypes.Bool, true),
		})
		if diags.WarningsCount() != 1 {
			t.Fatalf("%d warnings, want 1: %v", diags.WarningsCount(), diags)
		}
		if d := diags.Warnings()[0].Detail(); !strings.Contains(d, "10.0.1.7") {
			t.Errorf("the warning does not name the consumer: %q", d)
		}
	})
	t.Run("destroy", func(t *testing.T) {
		plan := tfsdk.Plan{Raw: tftypes.NewValue(stateRaw.Type(), nil), Schema: s.Schema}
		resp := resource.ModifyPlanResponse{Plan: plan}
		r.ModifyPlan(context.Background(), resource.ModifyPlanRequest{
			Config: tfsdk.Config{Raw: tftypes.NewValue(stateRaw.Type(), nil), Schema: s.Schema},
			Plan:   plan,
			State:  tfsdk.State{Raw: stateRaw, Schema: s.Schema},
		}, &resp)
		if resp.Diagnostics.HasError() {
			t.Fatalf("ModifyPlan: %v", resp.Diagnostics.Errors())
		}
		if resp.Diagnostics.WarningsCount() != 1 {
			t.Fatalf("%d warnings, want 1: %v", resp.Diagnostics.WarningsCount(), resp.Diagnostics)
		}
		if w := resp.Diagnostics.Warnings()[0]; !strings.Contains(w.Summary(), "will cut off 1") ||
			!strings.Contains(w.Detail(), "10.0.1.7") {
			t.Errorf("the destroy warning does not name the consumer: %q / %q", w.Summary(), w.Detail())
		}
	})
	t.Run("in place", func(t *testing.T) {
		diags := modifyPlan(t, map[string]tftypes.Value{"capacity_gib": tftypes.NewValue(tftypes.Number, 200)})
		if diags.WarningsCount() != 0 {
			t.Errorf("a capacity change warned as if it deleted the store: %v", diags)
		}
	})
}

// consumersInState is the refs `consumers` holds in a destroy's response.
func consumersInState(t *testing.T, resp resource.DeleteResponse) []string {
	t.Helper()
	var m fileStoreModel
	if d := resp.State.Get(context.Background(), &m); d.HasError() {
		t.Fatal(d)
	}
	var refs []string
	for ref := range m.Consumers.Elements() {
		refs = append(refs, ref)
	}
	return refs
}

// detachedAPI is a store with one consumer whose detach is accepted. revoke
// says whether the revocation ever finishes, and remove answers the DELETE.
func detachedAPI(t *testing.T, revoke bool, remove func(http.ResponseWriter) bool) *storeAPI {
	patched := false
	return &storeAPI{
		get: func(int) map[string]any {
			switch {
			case !patched:
				return withConsumers("active", "10.0.0.31")
			case revoke:
				return fileStoreBody("active")
			}
			body := fileStoreBody("active")
			body["reconciling"] = true
			body["consumerStatus"] = []any{map[string]any{"ref": "10.0.0.31", "state": "deleting"}}
			return body
		},
		patch: func(w http.ResponseWriter) {
			patched = true
			if err := json.NewEncoder(w).Encode(fileStoreBody("active")); err != nil {
				t.Errorf("write: %v", err)
			}
		},
		remove: remove,
	}
}

// A destroy that detached the consumers and then could not delete the store
// has still taken their access away. State says so, so the next plan shows
// the grants going back rather than claiming they are there, and the error
// says what was done and how to go on.
func TestFileStoreDestroyThatFailsAfterTheDetachRecordsIt(t *testing.T) {
	api := detachedAPI(t, true, func(w http.ResponseWriter) bool {
		w.WriteHeader(http.StatusForbidden)
		if _, err := w.Write([]byte(`{"error":{"code":403,"message":"not allowed"}}`)); err != nil {
			t.Errorf("write: %v", err)
		}
		return false
	})
	resp := deleteFileStore(t, api.serve(t).URL, map[string]tftypes.Value{
		"state": str("active"), "consumers": consumersValue(t, &fileStoreResource{}, "10.0.0.31"),
	})
	if !resp.Diagnostics.HasError() {
		t.Fatal("a refused delete reported success")
	}
	if got := consumersInState(t, resp); len(got) != 0 {
		t.Errorf("state still lists %v after the platform detached them", got)
	}
	if !hasWarning(resp.Diagnostics, "were detached") {
		t.Errorf("nothing says the consumers already lost access: %v", resp.Diagnostics)
	}
}

// The same holds when the revocation never finishes inside the timeout, and
// no DELETE goes out while it's still running.
func TestFileStoreDestroyWhoseDetachNeverSettlesRecordsIt(t *testing.T) {
	api := detachedAPI(t, false, accepted)
	resp := deleteFileStore(t, api.serve(t).URL, map[string]tftypes.Value{
		"state": str("active"), "consumers": consumersValue(t, &fileStoreResource{}, "10.0.0.31"),
	})
	if !resp.Diagnostics.HasError() {
		t.Fatal("a detach that never settled reported success")
	}
	if len(api.query) != 0 {
		t.Errorf("a DELETE went out while the revocation was still running")
	}
	if got := consumersInState(t, resp); len(got) != 0 {
		t.Errorf("state still lists %v after the platform accepted the detach", got)
	}
	if !hasWarning(resp.Diagnostics, "were detached") {
		t.Errorf("nothing says the consumers already lost access: %v", resp.Diagnostics)
	}
}

// The plan's warning names the consumers state knew of. One the platform
// lists that state didn't, attached after the refresh or skipped by
// `-refresh=false`, loses access too, so the apply names it.
func TestFileStoreDestroyNamesAConsumerStateDidNotList(t *testing.T) {
	api := detachedAPI(t, true, accepted)
	resp := deleteFileStore(t, api.serve(t).URL, map[string]tftypes.Value{"state": str("active")})
	if resp.Diagnostics.HasError() {
		t.Fatalf("delete: %v", resp.Diagnostics.Errors())
	}
	if !hasWarning(resp.Diagnostics, "10.0.0.31") {
		t.Errorf("the consumer state did not list is not named: %v", resp.Diagnostics)
	}
}

// Recording the detach is bookkeeping for a destroy that may still succeed.
// When it can't be written, the destroy mustn't fail over it: an error there
// would fail a destroy whose DELETE went through, and keep a store that's
// gone in state. So it warns, and the destroy goes on.
func TestRecordingTheDetachNeverFailsTheDestroy(t *testing.T) {
	var diags diag.Diagnostics
	// A state of another shape can't take the store, the way a conversion
	// that fails can't.
	other := schema.Schema{Attributes: map[string]schema.Attribute{"other": schema.StringAttribute{Optional: true}}}
	st := tfsdk.State{Raw: tftypes.NewValue(other.Type().TerraformType(context.Background()), nil), Schema: other}
	recordDetach(context.Background(), k0rdent.FileStore{Id: "fs1"}, fileStoreModel{}, &st, &diags)
	if diags.HasError() {
		t.Fatalf("a detach that couldn't be recorded failed the destroy: %v", diags.Errors())
	}
	if !hasWarning(diags, "fs1") {
		t.Errorf("nothing says the detach of fs1 wasn't recorded: %v", diags)
	}
}
