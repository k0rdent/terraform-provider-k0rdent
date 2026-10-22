package provider

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// mintAnswering is a platform whose token endpoint answers status with body,
// and whose operations all answer 401, so a test can tell which of the two a
// diagnostic is about.
func mintAnswering(t *testing.T, status int, body string) *k0rdent.ClientWithResponses {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/regions/global/auth/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Kong-Request-Id", "rid-mint")
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Kong-Request-Id", "rid-op")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = fmt.Fprint(w, `{"error":{"code":401,"message":"token is not active"}}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client, err := k0rdent.New(srv.URL,
		k0rdent.WithCredential(k0rdent.NewServiceAccountCredential("sa", "secret", nil)),
		k0rdent.WithTransport(testTransport(t)))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

const goodMint = `{"access_token":"tok","token_type":"Bearer","expires_in":900}`

// A revoked or wrong credential fails every operation with a 401, and the
// diagnostic has to say it's the credential, not the cluster, or an operator
// goes looking for a problem with the resource. A mint that failed for some
// other reason mustn't be blamed on the credential.
func TestAMintFailureSaysItIsAboutTheCredential(t *testing.T) {
	cases := []struct {
		name, body             string
		code                   int
		wantSummary, wantInDet string
		wantNotInDet           string
	}{
		{name: "the credential is refused", code: http.StatusUnauthorized,
			body:        `{"error":{"code":401,"message":"invalid_client"}}`,
			wantSummary: "Could not create the cluster because the platform refused the provider's credential",
			wantInDet:   "invalid_client"},
		{name: "the credential isn't allowed the token", code: http.StatusForbidden,
			body:        `{"error":{"code":403,"message":"non-interactive principals may not mint","reason":"NON_INTERACTIVE_PRINCIPAL"}}`,
			wantSummary: "Could not create the cluster because the platform refused the provider's credential",
			wantInDet:   "NON_INTERACTIVE_PRINCIPAL"},
		{name: "the mint is unavailable", code: http.StatusServiceUnavailable,
			body:         `{"error":{"code":503,"message":"identity provider unavailable"}}`,
			wantSummary:  "Could not create the cluster because the provider couldn't get an access token",
			wantInDet:    "identity provider unavailable",
			wantNotInDet: "refused"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			client := mintAnswering(t, c.code, c.body)
			_, err := client.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil)
			if err == nil {
				t.Fatal("the read succeeded despite the mint failing")
			}
			for _, raise := range []func(*diag.Diagnostics){
				func(d *diag.Diagnostics) { addAPIError(d, "create the cluster", nil, nil, err) },
				func(d *diag.Diagnostics) { addWaitError(d, "Could not create the cluster", err) },
				func(d *diag.Diagnostics) { addAdvisedError(d, "Could not create the cluster", "Run it again.", err) },
			} {
				var diags diag.Diagnostics
				raise(&diags)
				if len(diags) != 1 {
					t.Fatalf("%d diagnostics, want 1", len(diags))
				}
				if got := diags[0].Summary(); got != c.wantSummary {
					t.Errorf("summary %q, want %q", got, c.wantSummary)
				}
				detail := diags[0].Detail()
				if !strings.Contains(detail, c.wantInDet) {
					t.Errorf("detail doesn't carry the platform's answer %q:\n%s", c.wantInDet, detail)
				}
				if n := strings.Count(detail, "Request id: rid-mint"); n != 1 {
					t.Errorf("detail names the mint's request id %d times, want once:\n%s", n, detail)
				}
				if c.wantNotInDet != "" && strings.Contains(detail, c.wantNotInDet) {
					t.Errorf("detail blames the credential for a mint that wasn't refused:\n%s", detail)
				}
			}
		})
	}
}

// The operation's own 401, after a good mint, is a refusal of that
// operation and keeps the ordinary diagnostic.
func TestAnOperationsOwn401IsNotBlamedOnTheMint(t *testing.T) {
	client := mintAnswering(t, http.StatusOK, goodMint)
	r, err := client.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	var diags diag.Diagnostics
	addAPIError(&diags, "create the cluster", r.HTTPResponse, r.Body, nil)
	if got := diags[0].Summary(); got != "Could not create the cluster" {
		t.Errorf("summary %q, want the ordinary one", got)
	}
	if !strings.Contains(diags[0].Detail(), "rid-op") {
		t.Errorf("detail doesn't name the operation's request id:\n%s", diags[0].Detail())
	}
}
