package provider

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

func createClusterTokenPlan(t *testing.T, ctx context.Context, r *clusterResource, plan tfsdk.State, token string) resource.CreateResponse {
	t.Helper()
	if diags := plan.SetAttribute(ctx, path.Root(createTokenAttribute), types.StringValue(token)); diags.HasError() {
		t.Fatal(diags)
	}
	resp := resource.CreateResponse{State: tfsdk.State{Raw: tftypes.NewValue(plan.Raw.Type(), nil), Schema: plan.Schema}}
	r.Create(ctx, resource.CreateRequest{Plan: tfsdk.Plan(plan), Config: tfsdk.Config(plan)}, &resp)
	return resp
}

func requireCreateTokenRedacted(t *testing.T, resp resource.CreateResponse, logs, token string) {
	t.Helper()
	if !resp.Diagnostics.HasError() {
		t.Fatal("the upstream failure returned no error")
	}
	var diagnostics strings.Builder
	for _, d := range resp.Diagnostics {
		diagnostics.WriteString(d.Summary())
		diagnostics.WriteString(d.Detail())
	}
	if strings.Contains(diagnostics.String(), token) || strings.Contains(logs, token) {
		t.Fatalf("the durable token leaked\nDiagnostics:\n%s\nLogs:\n%s", diagnostics.String(), logs)
	}
	if !strings.Contains(diagnostics.String(), "[REDACTED]") {
		t.Fatalf("the diagnostic has no redaction marker:\n%s", diagnostics.String())
	}
}

func TestClusterCreateTokenIsRedactedFromErrorsAndTraceLogs(t *testing.T) {
	const token = "review-durable-token-that-must-not-leak"
	for _, tc := range []struct {
		name      string
		configure func(*clusterAPI)
	}{
		{
			name: "create response",
			configure: func(api *clusterAPI) {
				api.answer = func(_ int, _ string, w http.ResponseWriter) bool {
					w.WriteHeader(http.StatusBadRequest)
					api.write(w, map[string]any{"error": map[string]any{
						"code": 400, "message": "upstream echoed " + token,
					}})
					return true
				}
			},
		},
		{
			name: "readback response",
			configure: func(api *clusterAPI) {
				api.tags["c1"] = map[string]string{createTokenTag(): "another-run"}
				api.readAnswer = func(w http.ResponseWriter) bool {
					w.WriteHeader(http.StatusBadRequest)
					api.write(w, map[string]any{"error": map[string]any{
						"code": 400, "message": "readback echoed " + token,
					}})
					return true
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TF_LOG", "TRACE")
			api := newClusterAPI(t)
			tc.configure(api)
			server := api.serve()
			meta := testClusterMeta(t, server.URL, time.Minute)
			meta.Wait.Interval = time.Millisecond
			var logs bytes.Buffer
			ctx := tflogtest.RootLogger(context.Background(), &logs)
			plan := testClusterState(t, "c1", "/v1/regions/test/compute/cluster-types/test")
			resp := createClusterTokenPlan(t, ctx, &clusterResource{meta: meta}, plan, token)
			requireCreateTokenRedacted(t, resp, logs.String(), token)
		})
	}
}

type tokenRoundTripFunc func(*http.Request) (*http.Response, error)

func (f tokenRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestClusterCreateTokenIsRedactedFromPlanTimeTraceLogs(t *testing.T) {
	const token = "review-durable-token-that-must-not-leak"
	t.Setenv("TF_LOG", "TRACE")
	transport := loggingTransport{base: tokenRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("plan read echoed " + token)
	})}
	client, err := k0rdent.New("https://unused.invalid", k0rdent.WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	r := &clusterResource{meta: &Meta{Client: client, Region: "test", Project: "test",
		Wait: waitConfig{Timeout: time.Minute, Interval: time.Millisecond}}}
	var logs bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &logs)
	plan := testClusterState(t, "c1", "/v1/regions/test/compute/cluster-types/test")
	if d := plan.SetAttribute(ctx, path.Root(createTokenAttribute), types.StringValue(token)); d.HasError() {
		t.Fatal(d)
	}
	var diags diag.Diagnostics
	r.refuseExisting(ctx, tfsdk.Plan(plan), tfsdk.Config(plan), &diags)
	for _, d := range diags {
		if strings.Contains(d.Summary()+d.Detail(), token) {
			t.Fatalf("the plan-time diagnostic leaked the token: %s: %s", d.Summary(), d.Detail())
		}
	}
	if !strings.Contains(logs.String(), "plan-time read of the cluster failed") ||
		!strings.Contains(logs.String(), "[REDACTED]") {
		t.Fatalf("the plan-time trace path did not run with redaction:\n%s", logs.String())
	}
	if strings.Contains(logs.String(), token) {
		t.Fatalf("the plan-time trace leaked the token:\n%s", logs.String())
	}
}

func TestClusterCreateTokenIsRedactedFromTransportTraceLogs(t *testing.T) {
	const token = "review-durable-token-that-must-not-leak"
	t.Setenv("TF_LOG", "TRACE")
	transport := loggingTransport{base: tokenRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Status:     "404 Not Found",
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"code":404,"message":"not found"}}`)),
				Request:    req,
			}, nil
		}
		return nil, errors.New("transport echoed " + token)
	})}
	client, err := k0rdent.New("https://unused.invalid", k0rdent.WithTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	meta := &Meta{Client: client, Region: "test", Project: "test",
		Wait: waitConfig{Timeout: time.Minute, Interval: time.Millisecond}}
	var logs bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &logs)
	plan := testClusterState(t, "c1", "/v1/regions/test/compute/cluster-types/test")
	resp := createClusterTokenPlan(t, ctx, &clusterResource{meta: meta}, plan, token)
	requireCreateTokenRedacted(t, resp, logs.String(), token)
	for _, want := range []string{"platform request failed before a response", "sending it again"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("trace path %q did not run:\n%s", want, logs.String())
		}
	}
}
