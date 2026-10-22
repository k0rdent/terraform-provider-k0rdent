package provider

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-log/tflogtest"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

// answer is a response as the generated client hands it back, with the body
// already read.
func answer(status int, header, body string) *http.Response {
	h := http.Header{}
	if header != "" {
		h.Set("X-Request-Id", header)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// A request id is the one handle a support ticket needs, so every diagnostic
// raised over a response the platform sent has to carry it, whichever path
// the failure took to get there, and say it once.
func TestEveryDiagnosticOverAnAnswerCarriesItsRequestID(t *testing.T) {
	mintRefusal := &url.Error{Op: "Post", URL: "https://api.example/v1/regions/global/auth/token",
		Err: fmt.Errorf("%w: %w", k0rdent.ErrTokenMint, k0rdent.NewAPIError(answer(401, "", ""),
			[]byte(`{"error":{"code":401,"message":"invalid_client","requestId":"rid-mint"}}`)))}

	cases := []struct {
		name, want string
		raise      func(*diag.Diagnostics)
	}{
		{"a status the provider doesn't handle", "rid-unhandled", func(d *diag.Diagnostics) {
			addAPIError(d, "read the cluster", answer(http.StatusAccepted, "rid-unhandled", "{}"), []byte("{}"), nil)
		}},
		{"a refused mint, which reaches the caller as a transport error", "rid-mint", func(d *diag.Diagnostics) {
			addAPIError(d, "read the cluster", nil, nil, mintRefusal)
		}},
		{"a wait cancelled after a 429, which keeps the 429 mid-sentence", "rid-wait", func(d *diag.Diagnostics) {
			addAPIError(d, "read the cluster", nil, nil, fmt.Errorf(
				"stopped waiting to retry GET /v1/regions, which the platform had answered %w: %w",
				k0rdent.NewAPIError(answer(429, "", ""),
					[]byte(`{"error":{"code":429,"message":"slow down","requestId":"rid-wait"}}`)),
				context.Canceled))
		}},
		{"a success with nothing in it, through a waiter", "rid-empty", func(d *diag.Diagnostics) {
			addWaitError(d, "Could not delete the cluster", answeredWithout(answer(http.StatusOK, "rid-empty", ""), "cluster"))
		}},
		{"a success with nothing in it, with advice", "rid-advised", func(d *diag.Diagnostics) {
			addAdvisedError(d, "Could not cancel", "Run it again.", answeredWithout(answer(http.StatusOK, "rid-advised", ""), "cluster"))
		}},
	}
	for _, c := range cases {
		var diags diag.Diagnostics
		c.raise(&diags)
		if len(diags) != 1 {
			t.Fatalf("%s: %d diagnostics, want 1", c.name, len(diags))
		}
		detail := diags[0].Detail()
		if !strings.Contains(detail, "Request id: "+c.want) {
			t.Errorf("%s: the diagnostic doesn't say \"Request id: %s\":\n%s", c.name, c.want, detail)
		}
		// Once, on its own line: a second copy mid-sentence is noise a
		// support ticket has to be told to ignore.
		if n := strings.Count(detail, c.want); n != 1 {
			t.Errorf("%s: the diagnostic names %s %d times, want once:\n%s", c.name, c.want, n, detail)
		}
	}
}

// A redirect is refused before the generated client sees a response, so the
// refusal is the only place its request id can go.
func TestARefusedRedirectCarriesItsRequestID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Kong-Request-Id", "rid-redirect")
		http.Redirect(w, r, "https://elsewhere.example/", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)

	meta, err := newMeta(Settings{Endpoint: srv.URL, Credential: CredentialSource{Kind: APIKeyCredential, APIKey: "k"}}, "test")
	if err != nil {
		t.Fatalf("new meta: %v", err)
	}
	_, err = meta.Client.ListRegionsWithResponse(context.Background())
	if err == nil || !strings.Contains(err.Error(), "rid-redirect") {
		t.Errorf("error %v, want one carrying the request id", err)
	}
}

// TF_LOG=DEBUG is where a throttled or dropped run is read, and a retry line
// without the request id can't be matched to the gateway's log.
func TestTheRetryLogCarriesTheRequestID(t *testing.T) {
	var out bytes.Buffer
	ctx := tflogtest.RootLogger(context.Background(), &out)
	srv := newScriptedServer(t, reply{status: 429}, reply{status: 200})
	rt, _ := fastTransport(t)

	if _, err := send(t, ctx, rt, http.MethodGet, srv.URL, ""); err != nil {
		t.Fatal(err)
	}
	entries, err := tflogtest.MultilineJSONDecode(&out)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if msg, ok := e["@message"].(string); ok && strings.Contains(msg, "waiting before retrying") {
			found = true
			if e["request_id"] != "req-1" {
				t.Errorf("the retry line carries request_id %v, want req-1: %v", e["request_id"], e)
			}
		}
	}
	if !found {
		t.Fatalf("no retry line was logged: %s", out.String())
	}
}

// Fields for a log line about a response gain the platform's request id, from
// the error body when there is one and the headers otherwise, and are left
// alone when there's no response to read it from.
func TestLogFieldsGainTheRequestID(t *testing.T) {
	body := `{"error":{"code":503,"message":"m","requestId":"rid-body"}}`
	cases := []struct {
		resp *http.Response
		body string
		want any
	}{
		{answer(http.StatusServiceUnavailable, "rid-header", body), body, "rid-body"},
		{answer(http.StatusOK, "rid-header", ""), "", "rid-header"},
		{nil, "", nil},
	}
	for i, c := range cases {
		got := withRequestID(map[string]any{"k": "v"}, c.resp, []byte(c.body))
		if got["request_id"] != c.want || got["k"] != "v" {
			t.Errorf("case %d: fields %v, want request_id %v beside k", i, got, c.want)
		}
	}
}
