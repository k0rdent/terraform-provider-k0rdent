package provider

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

const testSubscriptionID = "0f8fad5b-d9cb-469f-a165-70867728950e"

func num(n int64) tftypes.Value { return tftypes.NewValue(tftypes.Number, big.NewFloat(float64(n))) }

// A rotation the platform accepted is in state even when the read after it
// fails. Otherwise state keeps the old secret_wo_version, and the next apply
// rotates again: a second new secret_version, and with the overlap on, the
// secret the receivers were on before the first rotation stops working.
func TestARotationIsRecordedWhenTheReadAfterItFails(t *testing.T) {
	var rotations int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/rotate"):
			rotations++
			if err := json.NewEncoder(w).Encode(map[string]any{
				"id": testSubscriptionID, "signing_secret": "s", "secret_version": 2,
				"previous_secret_expires_at": "2026-10-12T00:00:00Z", "rotated_at": "2026-10-11T00:00:00Z",
			}); err != nil {
				t.Error(err)
			}
		default:
			w.WriteHeader(http.StatusInternalServerError)
			if err := json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
				"code": 500, "message": "down", "requestId": "req-1",
			}}); err != nil {
				t.Error(err)
			}
		}
	}))
	defer server.Close()

	r := &webhookSubscriptionResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	prior, s := testValue(t, r, map[string]tftypes.Value{
		"id": str(testSubscriptionID), "url": str("https://hooks.example.test/x"),
		"secret_wo_version": num(1), "rotation_overlap": tftypes.NewValue(tftypes.Bool, true),
		"secret_version": num(1), "is_active": tftypes.NewValue(tftypes.Bool, true),
		"created_at": str("2026-10-10T00:00:00Z"),
	})
	planned, _ := testValue(t, r, map[string]tftypes.Value{
		"id": str(testSubscriptionID), "url": str("https://hooks.example.test/x"),
		"secret_wo_version": num(2), "rotation_overlap": tftypes.NewValue(tftypes.Bool, true),
		"secret_version":             tftypes.NewValue(tftypes.Number, tftypes.UnknownValue),
		"is_active":                  tftypes.NewValue(tftypes.Bool, tftypes.UnknownValue),
		"rotated_at":                 tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"previous_secret_expires_at": tftypes.NewValue(tftypes.String, tftypes.UnknownValue),
		"created_at":                 str("2026-10-10T00:00:00Z"),
	})
	config, _ := testValue(t, r, map[string]tftypes.Value{
		"url": str("https://hooks.example.test/x"), "secret_wo_version": num(2),
		"secret_wo": str(strings.Repeat("n", 32)),
	})

	resp := resource.UpdateResponse{State: tfsdk.State{Raw: prior, Schema: s.Schema}}
	r.Update(context.Background(), resource.UpdateRequest{
		State:  tfsdk.State{Raw: prior, Schema: s.Schema},
		Plan:   tfsdk.Plan{Raw: planned, Schema: s.Schema},
		Config: tfsdk.Config{Raw: config, Schema: s.Schema},
	}, &resp)

	if rotations != 1 {
		t.Fatalf("rotated %d times, want 1", rotations)
	}
	if !resp.Diagnostics.HasError() {
		t.Fatal("a failed read after the rotation reported no error")
	}
	var got webhookSubscriptionModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	if got.SecretWOVersion.ValueInt64() != 2 || got.SecretVersion.ValueInt64() != 2 {
		t.Errorf("state after the rotation has secret_wo_version %v and secret_version %v, want 2 and 2",
			got.SecretWOVersion, got.SecretVersion)
	}
	if !resp.State.Raw.IsFullyKnown() {
		t.Error("state after the rotation holds an unknown value")
	}
}

// webhookServer answers a create with status, contentType and body, and the
// list with whatever subscriptions the create made, as the platform would
// have stored them.
func webhookServer(t *testing.T, status int, contentType, body string, made string) *httptest.Server {
	t.Helper()
	created := false
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			created = true
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(status)
			if _, err := w.Write([]byte(body)); err != nil {
				t.Error(err)
			}
		default:
			subs := []any{}
			if created {
				subs = append(subs, map[string]any{
					"id": made, "url": "https://hooks.example.test/x", "event_types": []string{},
					"secret_version": 1, "is_active": true, "created_at": "2026-10-11T00:00:00Z",
				})
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]any{"subscriptions": subs}); err != nil {
				t.Error(err)
			}
		}
	}))
}

// A create the platform accepted with an answer the client couldn't decode
// lands in state, by the id in the body when there is one and by the list
// otherwise, and the body is never quoted. Left out of state, the next apply
// would make a second subscription and deliver every event twice.
func TestACreateWithAnUnreadableAnswerIsRecorded(t *testing.T) {
	for name, c := range map[string]struct {
		status            int
		contentType, body string
	}{
		"a 200 carrying the answer": {http.StatusOK, "application/json",
			`{"id":"` + testSubscriptionID + `","signing_secret":"whsec_leaked_value","created_at":"2026-10-11T00:00:00Z"}`},
		"a 201 with no body to read": {http.StatusCreated, "text/plain", "whsec_leaked_value"},
		"a 201 with a field that doesn't parse": {http.StatusCreated, "application/json",
			`{"id":"` + testSubscriptionID + `","signing_secret":"whsec_leaked_value","created_at":"not a time"}`},
	} {
		t.Run(name, func(t *testing.T) {
			server := webhookServer(t, c.status, c.contentType, c.body, testSubscriptionID)
			defer server.Close()
			r := &webhookSubscriptionResource{meta: testClusterMeta(t, server.URL, time.Minute)}
			unknown := func(typ tftypes.Type) tftypes.Value { return tftypes.NewValue(typ, tftypes.UnknownValue) }
			planned, s := testValue(t, r, map[string]tftypes.Value{
				"id": unknown(tftypes.String), "url": str("https://hooks.example.test/x"),
				"secret_wo_version": num(1), "rotation_overlap": tftypes.NewValue(tftypes.Bool, true),
				"secret_version": unknown(tftypes.Number), "is_active": unknown(tftypes.Bool),
				"created_at": unknown(tftypes.String), "rotated_at": unknown(tftypes.String),
				"previous_secret_expires_at": unknown(tftypes.String),
			})
			config, _ := testValue(t, r, map[string]tftypes.Value{
				"url": str("https://hooks.example.test/x"), "secret_wo_version": num(1),
				"secret_wo": str(strings.Repeat("n", 32)),
			})
			empty, _ := testValue(t, r, nil)
			resp := resource.CreateResponse{State: tfsdk.State{Raw: empty, Schema: s.Schema}}
			r.Create(context.Background(), resource.CreateRequest{
				Plan:   tfsdk.Plan{Raw: planned, Schema: s.Schema},
				Config: tfsdk.Config{Raw: config, Schema: s.Schema},
			}, &resp)

			for _, d := range resp.Diagnostics {
				if strings.Contains(d.Summary()+d.Detail(), "whsec_leaked_value") {
					t.Errorf("a diagnostic quotes the answer: %q", d.Detail())
				}
			}
			if resp.Diagnostics.HasError() {
				t.Fatalf("the create failed: %v", resp.Diagnostics)
			}
			var got webhookSubscriptionModel
			resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
			if got.ID.ValueString() != testSubscriptionID || got.SecretVersion.ValueInt64() != 1 {
				t.Errorf("state holds id %v and secret_version %v", got.ID, got.SecretVersion)
			}
		})
	}
}

// A rotation somebody made outside Terraform moves secret_version past what
// state last held. The refresh then forgets secret_wo_version, so the next
// plan rotates back to the configured secret instead of leaving receivers on
// one the configuration never had.
func TestARotationOutsideTerraformPlansARotationBack(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{"subscriptions": []any{map[string]any{
			"id": testSubscriptionID, "url": "https://hooks.example.test/x", "event_types": []string{},
			"secret_version": 3, "is_active": true, "created_at": "2026-10-10T00:00:00Z",
			"rotated_at": "2026-10-11T00:00:00Z",
		}}}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	r := &webhookSubscriptionResource{meta: testClusterMeta(t, server.URL, time.Minute)}
	prior, s := testValue(t, r, map[string]tftypes.Value{
		"id": str(testSubscriptionID), "url": str("https://hooks.example.test/x"),
		"secret_wo_version": num(2), "rotation_overlap": tftypes.NewValue(tftypes.Bool, true),
		"secret_version": num(2), "is_active": tftypes.NewValue(tftypes.Bool, true),
		"created_at": str("2026-10-10T00:00:00Z"),
	})
	resp := resource.ReadResponse{State: tfsdk.State{Raw: prior, Schema: s.Schema}}
	r.Read(context.Background(), resource.ReadRequest{State: tfsdk.State{Raw: prior, Schema: s.Schema}}, &resp)
	var got webhookSubscriptionModel
	resp.Diagnostics.Append(resp.State.Get(context.Background(), &got)...)
	if resp.Diagnostics.HasError() {
		t.Fatal(resp.Diagnostics)
	}
	if !got.SecretWOVersion.IsNull() || got.SecretVersion.ValueInt64() != 3 {
		t.Errorf("after a rotation outside Terraform state has secret_wo_version %v and secret_version %v, "+
			"want null and 3", got.SecretWOVersion, got.SecretVersion)
	}
}

// A secret version that's unknown passes validation, where every variable is
// unknown, and is refused at plan time, where only a value the apply works out
// still is.
func TestAnUnknownSecretVersionIsRefusedAtPlanNotAtValidate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := &webhookSubscriptionResource{}
	raw, s := testValue(t, r, map[string]tftypes.Value{
		"url":               str("https://hooks.example.test/x"),
		"secret_wo":         str(strings.Repeat("s", 40)),
		"secret_wo_version": tftypes.NewValue(tftypes.Number, tftypes.UnknownValue),
		"rotation_overlap":  tftypes.NewValue(tftypes.Bool, true),
	})
	config := tfsdk.Config{Raw: raw, Schema: s.Schema}

	var validated resource.ValidateConfigResponse
	for _, v := range r.ConfigValidators(ctx) {
		v.ValidateResource(ctx, resource.ValidateConfigRequest{Config: config}, &validated)
	}
	if v, ok := any(r).(resource.ResourceWithValidateConfig); ok {
		v.ValidateConfig(ctx, resource.ValidateConfigRequest{Config: config}, &validated)
	}
	if validated.Diagnostics.HasError() {
		t.Errorf("validation refused a version from a variable: %v", validated.Diagnostics)
	}

	none := tftypes.NewValue(s.Schema.Type().TerraformType(ctx), nil)
	planned := resource.ModifyPlanResponse{Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}}
	r.ModifyPlan(ctx, resource.ModifyPlanRequest{
		Config: config, Plan: tfsdk.Plan{Raw: raw, Schema: s.Schema}, State: tfsdk.State{Raw: none, Schema: s.Schema},
	}, &planned)
	if errs := planned.Diagnostics.Errors(); len(errs) != 1 || !strings.Contains(errs[0].Summary(), "known at plan time") {
		t.Errorf("the plan didn't refuse a version only the apply knows: %v", planned.Diagnostics)
	}
}

// An event type outside the catalog's form is warned about, not refused: the
// subscription's filter declares no pattern of its own.
func TestAnEventTypeOutsideTheCatalogFormWarns(t *testing.T) {
	t.Parallel()
	set, _ := types.SetValueFrom(context.Background(), types.StringType, []string{"Cluster.Ready", "io.k0rdent.cluster.ready.v1"})
	var resp validator.SetResponse
	eventTypesInCatalogForm{}.ValidateSet(context.Background(), validator.SetRequest{Path: path.Root("event_types"), ConfigValue: set}, &resp)
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Errorf("want one warning and no error, got %v", resp.Diagnostics)
	}
}

// A second rotation while the first one's overlap is still open drops the
// secret parked by the first, which receivers may still be on.
func TestARotationInsideAnOpenOverlapIsNamed(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	open := types.StringValue("2026-10-12T00:00:00Z")
	if _, ok := parkedSecretDropped(open, types.BoolValue(true), now); !ok {
		t.Error("a rotation inside an open overlap wasn't named")
	}
	for name, until := range map[string]types.String{
		"closed": types.StringValue("2026-10-11T00:00:00Z"), "none": types.StringNull(),
	} {
		if _, ok := parkedSecretDropped(until, types.BoolValue(true), now); ok {
			t.Errorf("an overlap that's %s was named", name)
		}
	}
}

// A 200 without `subscriptions` isn't the list. Reading it as an empty one
// would drop the subscription from state, and the next apply would create a
// second one. Only an answer that holds the list, empty or not, says it's gone.
func TestAListAnswerWithoutSubscriptionsKeepsTheSubscription(t *testing.T) {
	for _, c := range []struct {
		body string
		kept bool
	}{
		{`{}`, true},
		{`{"subscriptions": null}`, true},
		{`{"subscriptions": []}`, false},
	} {
		t.Run(c.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if _, err := w.Write([]byte(c.body)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			r := &webhookSubscriptionResource{meta: testClusterMeta(t, server.URL, time.Minute)}
			prior, s := testValue(t, r, map[string]tftypes.Value{
				"id": str(testSubscriptionID), "url": str("https://hooks.example.test/x"),
				"secret_version": num(1), "is_active": tftypes.NewValue(tftypes.Bool, true),
				"created_at": str("2026-10-10T00:00:00Z"),
			})
			resp := resource.ReadResponse{State: tfsdk.State{Raw: prior, Schema: s.Schema}}
			r.Read(context.Background(), resource.ReadRequest{State: tfsdk.State{Raw: prior, Schema: s.Schema}}, &resp)
			if kept := !resp.State.Raw.IsNull(); kept != c.kept {
				t.Fatalf("after %s the subscription is in state: %v, want %v", c.body, kept, c.kept)
			}
			if c.kept && !resp.Diagnostics.HasError() {
				t.Errorf("after %s Read raised no error", c.body)
			}
		})
	}
}
