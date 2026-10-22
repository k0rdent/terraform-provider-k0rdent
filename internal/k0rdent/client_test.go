package k0rdent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// elsewhere is the host a redirect points at. It records what reached it, so a
// test can say the secret or the token never did.
type elsewhere struct {
	hits atomic.Int64
	seen atomic.Value // last body and Authorization header, as one string
}

func (e *elsewhere) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		e.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		e.seen.Store(r.Header.Get("Authorization") + " " + string(body))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (e *elsewhere) assertUntouched(t *testing.T) {
	t.Helper()
	if n := e.hits.Load(); n != 0 {
		seen, _ := e.seen.Load().(string)
		t.Errorf("the redirect target was reached %d time(s), last with %q", n, seen)
	}
}

// A 307 replays the method and body, and the mint's body is the client secret.
// Following it hands the secret to whatever host the Location names.
func TestNew_MintRefusesA307(t *testing.T) {
	target := &elsewhere{}
	to := target.server(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Kong-Request-Id", "rid-307")
		http.Redirect(w, r, to.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	_, err := testClient(t, origin).ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil)
	if err == nil {
		t.Fatal("the call succeeded, want the refused redirect as its error")
	}
	target.assertUntouched(t)
	for _, want := range []string{"307", to.URL, "rid-307"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// A 308 on an API call is the same replay with the bearer token on it: the
// authenticating transport sees the follow-up as a fresh request and stamps it.
func TestNew_APICallRefusesA308(t *testing.T) {
	target := &elsewhere{}
	to := target.server(t)
	var mints atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/regions/global/auth/token", func(w http.ResponseWriter, _ *http.Request) {
		mints.Add(1)
		mintOK(w)
	})
	mux.HandleFunc("GET /v1/regions/{region}/compute/cluster-types", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, to.URL+r.URL.Path, http.StatusPermanentRedirect)
	})
	origin := httptest.NewServer(mux)
	t.Cleanup(origin.Close)

	_, err := testClient(t, origin).ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil)
	if err == nil {
		t.Fatal("the call succeeded, want the refused redirect as its error")
	}
	if n := mints.Load(); n != 1 {
		t.Fatalf("mints = %d, want 1 before the read", n)
	}
	target.assertUntouched(t)
	if !strings.Contains(err.Error(), "308") {
		t.Errorf("error %q does not name the 308", err)
	}
}

// mintOK answers a mint the way the token endpoint does.
func mintOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"access_token":"token-1","token_type":"Bearer","expires_in":900}`)
}

// countingServer serves the cluster-types read and counts the connections
// dialled to it.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var opened atomic.Int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"clusterTypes": []any{}})
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			opened.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	return srv, &opened
}

func read(t *testing.T, c *ClientWithResponses) {
	t.Helper()
	r, err := c.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if r.StatusCode() != http.StatusOK {
		t.Fatalf("read: status %d", r.StatusCode())
	}
}

// Two clients hold two pools. On a shared one the second client would pick up
// the first one's idle connection rather than dialling its own.
func TestNew_ClientsDoNotShareAConnectionPool(t *testing.T) {
	srv, opened := countingServer(t)
	for range 2 {
		c, err := New(srv.URL)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		read(t, c)
	}
	if got := opened.Load(); got != 2 {
		t.Errorf("two clients dialled %d connection(s), want 2, one each", got)
	}
}

// Closing http.DefaultTransport's idle connections, which httptest.Server.Close
// does on every call, leaves a client's connections alone. A request that had
// just picked one up from the shared pool fails with "http:
// CloseIdleConnections called", and a POST is not replayed after that.
func TestNew_SurvivesTheSharedPoolClosing(t *testing.T) {
	shared, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Skip("http.DefaultTransport is not an *http.Transport in this process")
	}
	srv, opened := countingServer(t)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	read(t, c)
	before := opened.Load()
	shared.CloseIdleConnections()
	read(t, c)
	if after := opened.Load(); after != before {
		t.Errorf("the client dialled again after the shared pool closed its idle connections, " +
			"so its connection was in that pool")
	}
}

// The User-Agent goes on the mint as well as the API call, so a server-side
// log can say which consumer and version spent a mint.
func TestWithUserAgent_ReachesTheMintAndTheCall(t *testing.T) {
	var mintUA, readUA atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/regions/global/auth/token", func(w http.ResponseWriter, r *http.Request) {
		mintUA.Store(r.Header.Get("User-Agent"))
		mintOK(w)
	})
	mux.HandleFunc("GET /v1/regions/{region}/compute/cluster-types", func(w http.ResponseWriter, r *http.Request) {
		readUA.Store(r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"clusterTypes":[]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	read(t, testClient(t, srv, WithUserAgent("terraform-provider-k0rdent/1.2.3")))
	for name, v := range map[string]*atomic.Value{"mint": &mintUA, "read": &readUA} {
		if got, _ := v.Load().(string); got != "terraform-provider-k0rdent/1.2.3" {
			t.Errorf("%s User-Agent = %q, want the configured one", name, got)
		}
	}
}

// A caller's WithHTTPClient would replace the client New built, auth transport
// and all, and every request would go out with no token and no error to say
// why. New refuses it instead and names the options that do the same job.
func TestNew_RefusesAReplacementHTTPClient(t *testing.T) {
	f := &fakeAPI{}
	srv := f.server(t)
	c, err := New(srv.URL,
		WithCredential(NewServiceAccountCredential("sa-tf@vendor.iam", "k0r_secret", nil)),
		WithClientOption(WithHTTPClient(&http.Client{Transport: &http.Transport{}})))
	if err == nil {
		// Show what the client would have done, which is the reason for the refusal.
		_, _ = c.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil)
		auth, _ := f.authSeen.Load().(string)
		t.Fatalf("New accepted WithHTTPClient: %d mint(s), read sent Authorization %q", f.mints.Load(), auth)
	}
	if !strings.Contains(err.Error(), "WithTransport") {
		t.Errorf("error %q does not point at WithTransport", err)
	}
}

// Wrapping the doer New built is a replacement too: the wrapper is what the
// generated constructor keeps, and New cannot see whether it still calls
// through. New refuses it rather than guess.
func TestNew_RefusesAWrappedHTTPClient(t *testing.T) {
	wrap := func(c *Client) error {
		inner := c.Client
		if inner == nil {
			return errors.New("option saw no doer to wrap")
		}
		c.Client = wrappedDoer{inner}
		return nil
	}
	_, err := New("https://api.example.test",
		WithCredential(NewServiceAccountCredential("sa-tf@vendor.iam", "k0r_secret", nil)),
		WithClientOption(wrap))
	if err == nil {
		t.Fatal("New accepted an option that wraps the doer")
	}
	if !strings.Contains(err.Error(), "replaced the HTTP client") {
		t.Errorf("error %q does not say the option replaced the HTTP client", err)
	}
}

type wrappedDoer struct{ inner HttpRequestDoer }

func (w wrappedDoer) Do(req *http.Request) (*http.Response, error) { return w.inner.Do(req) }

// The mint sends the client secret in its body, so New takes an absolute
// https URL, or http only to this machine, where the local fake runs.
func TestNew_ValidatesTheEndpoint(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		ok       bool
	}{
		{"https://api.k0rdent.example", true},
		{"https://api.k0rdent.example/base", true},
		{"http://127.0.0.1:8080", true},
		{"http://localhost:8080", true},
		{"http://LocalHost:8080", true},
		{"http://localhost.:8080", true},
		{"http://[::1]:8080", true},
		{"", false},
		{"api.k0rdent.example", false},
		{"://api.k0rdent.example", false},
		{"ftp://api.k0rdent.example", false},
		{"https://", false},
		{"http://api.k0rdent.example", false},
		{"http://10.0.0.5", false},
		{"http://localhost.example.com", false},
		{"http://localhost..:8080", false},
	} {
		_, err := New(tc.endpoint)
		if (err == nil) != tc.ok {
			t.Errorf("New(%q) error = %v, want ok = %v", tc.endpoint, err, tc.ok)
		}
	}
}

// WithBaseURL moves the client after New has seen the endpoint, so the check
// applies to where requests actually go.
func TestNew_ValidatesABaseURLOverride(t *testing.T) {
	_, err := New("https://api.k0rdent.example", WithClientOption(WithBaseURL("http://api.k0rdent.example")))
	if err == nil || !strings.Contains(err.Error(), "cleartext") {
		t.Errorf("error %v, want the cleartext refusal", err)
	}
}

// redirectingAPI mints normally and answers the read with a redirect to to.
func redirectingAPI(t *testing.T, status int, to string, tls bool) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/regions/global/auth/token", func(w http.ResponseWriter, _ *http.Request) { mintOK(w) })
	mux.HandleFunc("GET /v1/regions/{region}/compute/cluster-types", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, to+r.URL.Path, status)
	})
	var srv *httptest.Server
	if tls {
		srv = httptest.NewTLSServer(mux)
	} else {
		srv = httptest.NewServer(mux)
	}
	t.Cleanup(srv.Close)
	return srv
}

// A 302 on a GET is followed by default, and the follow-up comes back through
// the auth transport, which would stamp the token on it for the other host.
func TestNew_APICallRefusesA302ToAnotherHost(t *testing.T) {
	target := &elsewhere{}
	to := target.server(t)
	origin := redirectingAPI(t, http.StatusFound, to.URL, false)

	if _, err := testClient(t, origin).ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil); err == nil {
		t.Fatal("the call succeeded, want the refused redirect as its error")
	}
	target.assertUntouched(t)
}

// An https endpoint redirecting to http would put the token on the wire in
// cleartext.
func TestNew_APICallRefusesARedirectFromHTTPSToHTTP(t *testing.T) {
	target := &elsewhere{}
	to := target.server(t)
	origin := redirectingAPI(t, http.StatusFound, to.URL, true)

	c := testClient(t, origin, WithTransport(origin.Client().Transport))
	if _, err := c.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil); err == nil {
		t.Fatal("the call succeeded, want the refused redirect as its error")
	}
	target.assertUntouched(t)
}

// The token belongs to the endpoint. A request the client's own doer sends
// anywhere else, by a redirect or otherwise, goes without it.
func TestNew_TokenStaysOnTheEndpointsHost(t *testing.T) {
	var seen atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Store(r.Header.Get("Authorization"))
	}))
	t.Cleanup(other.Close)
	origin := redirectingAPI(t, http.StatusFound, other.URL, false)

	doer := testClient(t, origin).ClientInterface.(*Client).Client
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, other.URL+"/anything", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := doer.Do(req)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	_ = resp.Body.Close()
	if got, _ := seen.Load().(string); got != "" {
		t.Errorf("another host received Authorization %q", got)
	}
}

// The binding is on the scheme as well as the host. An https endpoint's token
// must not go to the same host and port over http, where the path can read it.
func TestNew_TokenStaysOffHTTPToTheEndpointsHost(t *testing.T) {
	seen := map[string]string{}
	rt := schemeRecorder(func(req *http.Request) *http.Response {
		if req.URL.Path == "/v1/regions/global/auth/token" {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": {"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"access_token":"token-1","token_type":"Bearer","expires_in":900}`)),
				Request:    req,
			}
		}
		seen[req.URL.Scheme] = req.Header.Get("Authorization")
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: req}
	})
	c, err := New("https://api.k0rdent.example:8443",
		WithCredential(NewServiceAccountCredential("sa-tf@vendor.iam", "k0r_secret", nil)),
		WithTransport(rt))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	doer := c.ClientInterface.(*Client).Client
	for _, scheme := range []string{"https", "http"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet,
			scheme+"://api.k0rdent.example:8443/v1/anything", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := doer.Do(req)
		if err != nil {
			t.Fatalf("send over %s: %v", scheme, err)
		}
		_ = resp.Body.Close()
	}
	if got := seen["https"]; got != "Bearer token-1" {
		t.Fatalf("https to the endpoint sent Authorization %q, want the token", got)
	}
	if got := seen["http"]; got != "" {
		t.Errorf("http to the endpoint's host and port sent Authorization %q", got)
	}
}

// schemeRecorder answers every request in process, so no request leaves it.
type schemeRecorder func(*http.Request) *http.Response

func (f schemeRecorder) RoundTrip(req *http.Request) (*http.Response, error) { return f(req), nil }
