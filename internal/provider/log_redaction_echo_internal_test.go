package provider

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// A token endpoint that echoes the client secret in its refusal mustn't put
// it in TF_LOG through the plan-time read's log line, any more than in a
// diagnostic.
func TestAPlanTimeReadLogsNoEchoedSecret(t *testing.T) {
	const secret = "dummy-secret-123"
	t.Setenv(EnvVar(envClientSecret), secret)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		if _, err := w.Write([]byte(`{"error":{"code":400,"message":"invalid client_secret ` + secret + `"}}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()
	client, err := k0rdent.New(server.URL, k0rdent.WithTransport(testTransport(t)),
		k0rdent.WithCredential(k0rdent.NewServiceAccountCredential("id", secret, nil)))
	if err != nil {
		t.Fatal(err)
	}
	r := &clusterResource{meta: &Meta{Client: client, Region: "test", Project: "test",
		Wait: waitConfig{Timeout: time.Second, Interval: time.Microsecond}}}
	st := testClusterState(t, "c1", "/v1/regions/test/compute/cluster-types/t")
	var out bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &out)
	var diags diag.Diagnostics
	r.refuseExisting(ctx, tfsdk.Plan(st), &diags)
	if logged := out.String(); !strings.Contains(logged, "mint access token") || strings.Contains(logged, secret) {
		t.Errorf("log: %s", logged)
	}
}
