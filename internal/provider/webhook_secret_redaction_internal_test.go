package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// An answer that quotes the signing secret it was sent mustn't print it:
// `secret_wo` is write-only so that it reaches no log. A refusal is still
// shown, secret hidden. A 2xx body is never shown.
func TestAWebhookRefusalHidesTheSecretItWasSent(t *testing.T) {
	secret := strings.Repeat("n", 32)
	quoted := "secret " + secret + " rejected"
	refuse := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		writeJSON(w, map[string]any{"error": map[string]any{"code": 422, "message": quoted}})
	}
	// acceptUnreadably takes the create with a body the client can't decode,
	// and lists no subscription, so the create can't be recorded.
	acceptUnreadably := func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, map[string]any{"subscriptions": []any{}})
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
		if _, err := w.Write([]byte(quoted)); err != nil {
			t.Error(err)
		}
	}
	unknown := func(typ tftypes.Type) tftypes.Value { return tftypes.NewValue(typ, tftypes.UnknownValue) }

	for name, c := range map[string]struct {
		serve func(http.ResponseWriter, *http.Request)
		shows string
		run   func(*testing.T, *webhookSubscriptionResource, tfsdk.Config) diag.Diagnostics
	}{
		"a refused create":             {func(w http.ResponseWriter, _ *http.Request) { refuse(w) }, "secret [REDACTED] rejected", runWebhookCreate},
		"a create accepted unreadably": {acceptUnreadably, "The body isn't shown", runWebhookCreate},
		"a refused rotation": {func(w http.ResponseWriter, _ *http.Request) { refuse(w) }, "secret [REDACTED] rejected",
			func(t *testing.T, r *webhookSubscriptionResource, config tfsdk.Config) diag.Diagnostics {
				prior, _ := testValue(t, r, map[string]tftypes.Value{
					"id": str(testSubscriptionID), "url": str("https://hooks.example.test/x"),
					"secret_wo_version": num(1), "rotation_overlap": tftypes.NewValue(tftypes.Bool, true),
					"secret_version": num(1), "is_active": tftypes.NewValue(tftypes.Bool, true),
					"created_at": str("2026-10-10T00:00:00Z"),
				})
				planned, _ := testValue(t, r, map[string]tftypes.Value{
					"id": str(testSubscriptionID), "url": str("https://hooks.example.test/x"),
					"secret_wo_version": num(2), "rotation_overlap": tftypes.NewValue(tftypes.Bool, true),
					"secret_version": unknown(tftypes.Number), "is_active": unknown(tftypes.Bool),
					"rotated_at": unknown(tftypes.String), "previous_secret_expires_at": unknown(tftypes.String),
					"created_at": str("2026-10-10T00:00:00Z"),
				})
				resp := resource.UpdateResponse{State: tfsdk.State{Raw: prior, Schema: config.Schema}}
				r.Update(context.Background(), resource.UpdateRequest{
					State:  tfsdk.State{Raw: prior, Schema: config.Schema},
					Plan:   tfsdk.Plan{Raw: planned, Schema: config.Schema},
					Config: config,
				}, &resp)
				return resp.Diagnostics
			}},
	} {
		t.Run(name, func(t *testing.T) {
			var writes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					writes.Add(1)
				}
				c.serve(w, r)
			}))
			defer server.Close()
			r := &webhookSubscriptionResource{meta: testClusterMeta(t, server.URL, time.Minute)}
			raw, s := testValue(t, r, map[string]tftypes.Value{
				"url": str("https://hooks.example.test/x"), "secret_wo_version": num(2), "secret_wo": str(secret),
			})
			diags := c.run(t, r, tfsdk.Config{Raw: raw, Schema: s.Schema})
			text := diagText(diags)
			switch {
			case writes.Load() == 0:
				t.Fatal("nothing was sent")
			case !diags.HasError():
				t.Fatalf("no error:\n%s", text)
			case strings.Contains(text, secret):
				t.Errorf("the diagnostic prints the secret:\n%s", text)
			case !strings.Contains(text, c.shows):
				t.Errorf("the diagnostic doesn't say %q:\n%s", c.shows, text)
			}
		})
	}
}

func runWebhookCreate(t *testing.T, r *webhookSubscriptionResource, config tfsdk.Config) diag.Diagnostics {
	unknown := func(typ tftypes.Type) tftypes.Value { return tftypes.NewValue(typ, tftypes.UnknownValue) }
	planned, _ := testValue(t, r, map[string]tftypes.Value{
		"id": unknown(tftypes.String), "url": str("https://hooks.example.test/x"),
		"secret_wo_version": num(2), "rotation_overlap": tftypes.NewValue(tftypes.Bool, true),
		"secret_version": unknown(tftypes.Number), "is_active": unknown(tftypes.Bool),
		"created_at": unknown(tftypes.String), "rotated_at": unknown(tftypes.String),
		"previous_secret_expires_at": unknown(tftypes.String),
	})
	empty, _ := testValue(t, r, nil)
	resp := resource.CreateResponse{State: tfsdk.State{Raw: empty, Schema: config.Schema}}
	r.Create(context.Background(), resource.CreateRequest{
		Plan:   tfsdk.Plan{Raw: planned, Schema: config.Schema},
		Config: config,
	}, &resp)
	return resp.Diagnostics
}
