package k0rdent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// answering serves every request with one canned response.
func answering(t *testing.T, status int, header http.Header, body string) *ClientWithResponses {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		for k, v := range header {
			w.Header()[k] = v
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}
	return c
}

var jsonHeader = http.Header{"Content-Type": {"application/json"}}

// The subscription was created and its signing secret is in the body, but one
// field does not match the schema. The secret is shown once, so a parse that
// drops the response on a decode error loses it for good.
func TestParse_DecodeErrorKeepsTheBody(t *testing.T) {
	const raw = `{"id":"7d0c1c35-7a3f-4c3e-9b55-0f6f3c1f8a10","signing_secret":"whsec_once","created_at":"yesterday"}`
	c := answering(t, http.StatusCreated, jsonHeader, raw)

	r, err := c.CreateWebhookSubscriptionWithResponse(context.Background(),
		CreateWebhookSubscriptionJSONRequestBody{})
	if err == nil {
		t.Fatal("err = nil, want the decode error for created_at")
	}
	if r == nil {
		t.Fatal("response = nil, want it returned with the error")
	}
	if string(r.Body) != raw {
		t.Errorf("Body = %q, want %q", r.Body, raw)
	}
	if r.HTTPResponse == nil || r.HTTPResponse.StatusCode != http.StatusCreated {
		t.Errorf("HTTPResponse = %v, want the 201", r.HTTPResponse)
	}
	if r.JSON201 != nil {
		t.Errorf("JSON201 = %+v, want nil when the body did not decode", r.JSON201)
	}
}

// Retry-After is declared as an integer, and RFC 9110 also allows an
// HTTP-date. A 429 carrying the date form fails to bind, and the caller still
// needs the response to report the throttle.
func TestParse_HeaderBindErrorKeepsTheResponse(t *testing.T) {
	header := http.Header{
		"Content-Type": {"application/json"},
		"Retry-After":  {"Wed, 21 Oct 2026 07:28:00 GMT"},
	}
	const raw = `{"error":{"code":429,"message":"slow down"}}`
	c := answering(t, http.StatusTooManyRequests, header, raw)

	r, err := c.MintTokenWithBodyWithResponse(context.Background(), "application/json", strings.NewReader(`{}`))
	if err == nil {
		t.Fatal("err = nil, want the Retry-After bind error")
	}
	if r == nil {
		t.Fatal("response = nil, want it returned with the error")
	}
	apiErr, ok := CheckResponse(r.HTTPResult()).(*APIError)
	if !ok || apiErr.StatusCode != http.StatusTooManyRequests || apiErr.Message != "slow down" {
		t.Errorf("CheckResponse = %+v, want the 429 and its message", apiErr)
	}
}

// A response that never arrived is still nil. That is what tells a caller a
// transport failure apart from a body that arrived and did not decode.
func TestParse_TransportErrorStillReturnsNil(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	c, err := NewClientWithResponses(srv.URL)
	if err != nil {
		t.Fatalf("NewClientWithResponses: %v", err)
	}

	r, err := c.CreateWebhookSubscriptionWithResponse(context.Background(),
		CreateWebhookSubscriptionJSONRequestBody{})
	if err == nil {
		t.Fatal("err = nil, want the connection error")
	}
	if r != nil {
		t.Errorf("response = %+v, want nil when nothing came back", r)
	}
}
