package provider

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral/schema"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"go.yaml.in/yaml/v3"
	"google.golang.org/protobuf/encoding/protowire"
)

type relayCluster struct {
	server *httptest.Server

	mu       sync.Mutex
	valid    map[string]bool
	seen     []relaySeen
	handlers map[string]http.HandlerFunc // by path, after the token check
}

type relaySeen struct {
	path, token, body string
	status            int
}

func newRelayCluster(t *testing.T) *relayCluster {
	t.Helper()
	return newRelayClusterTLS(t, nil)
}

func newRelayClusterTLS(t *testing.T, cfg *tls.Config) *relayCluster {
	t.Helper()
	c := &relayCluster{valid: map[string]bool{}, handlers: map[string]http.HandlerFunc{}}
	c.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("the cluster could not read a body: %v", err)
		}
		c.mu.Lock()
		ok := c.valid[token]
		h := c.handlers[r.URL.Path]
		status := http.StatusOK
		if !ok {
			status = http.StatusUnauthorized
		}
		c.seen = append(c.seen, relaySeen{path: r.URL.Path, token: token, body: string(body), status: status})
		c.mu.Unlock()
		if !ok {
			if strings.HasPrefix(r.Header.Get("Accept"), relayProtobuf) {
				w.Header().Set("Content-Type", relayProtobuf)
				w.WriteHeader(http.StatusUnauthorized)
				if _, err := w.Write(protobufStatus("Unauthorized")); err != nil {
					t.Errorf("the cluster could not answer: %v", err)
				}
				return
			}
			writeRelayStatus(w, http.StatusUnauthorized, "Unauthorized", "Unauthorized")
			return
		}
		if h != nil {
			h(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"kind":"Status","status":"Success","path":%q}`, r.URL.Path)
	}))
	c.server.TLS = cfg
	c.server.StartTLS()
	t.Cleanup(c.server.Close)
	return c
}

func (c *relayCluster) allow(tokens ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tok := range tokens {
		c.valid[tok] = true
	}
}

func (c *relayCluster) revoke(tokens ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tok := range tokens {
		delete(c.valid, tok)
	}
}

func (c *relayCluster) handle(path string, h http.HandlerFunc) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.handlers[path] = h
}

func (c *relayCluster) requests() []relaySeen {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]relaySeen(nil), c.seen...)
}

// The server sits under a path, as behind a gateway.
func (c *relayCluster) conn(token string) kubeconfigConnection {
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.server.Certificate().Raw})
	return kubeconfigConnection{Host: c.server.URL + "/gw/c1", ClusterCACertificate: string(ca), Token: token}
}

type relayClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *relayClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *relayClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// The signature part is the name, so tests can tell tokens apart.
func testJWT(name string, exp time.Time) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"RS256"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d,"aud":"k8s"}`, exp.Unix()))) + "." + name
}

type relayFetcher struct {
	mu     sync.Mutex
	tokens []string
	err    error
	calls  atomic.Int32
	gate   chan struct{} // when set, each fetch waits for it
	cl     *relayCluster
}

func (f *relayFetcher) set(err error, tokens ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err, f.tokens = err, tokens
}

func (f *relayFetcher) fetch(ctx context.Context) (kubeconfigConnection, error) {
	f.calls.Add(1)
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return kubeconfigConnection{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return kubeconfigConnection{}, f.err
	}
	if len(f.tokens) == 0 {
		return kubeconfigConnection{}, errors.New("the test has no more tokens")
	}
	tok := f.tokens[0]
	f.tokens = f.tokens[1:]
	return f.cl.conn(tok), nil
}

func startTestRelay(t *testing.T, cl *relayCluster, first string, f *relayFetcher, clock *relayClock) *kubeconfigRelay {
	t.Helper()
	f.cl = cl
	r, err := startKubeconfigRelayAt(context.Background(), cl.conn(first), f.fetch, time.Minute, clock.now)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(r.close)
	return r
}

func relayClient(t *testing.T, r *kubeconfigRelay) *http.Client {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(r.caPEM)) {
		t.Fatal("the relay's CA is not PEM")
	}
	// The timeout fails a relay stuck in a loop instead of hanging the suite.
	return &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
	}
}

func relayDo(t *testing.T, client *http.Client, method, url, auth, body string) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the body: %v", method, url, err)
	}
	return resp.StatusCode, string(b)
}

func newTestClock() *relayClock { return &relayClock{t: time.Unix(1_800_000_000, 0)} }

// The cluster sees the platform's token, never the secret.
func TestTheRelayAcceptsOnlyItsSecret(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	cl := newRelayCluster(t)
	tok := testJWT("first", clock.now().Add(15*time.Minute))
	cl.allow(tok)
	r := startTestRelay(t, cl, tok, &relayFetcher{}, clock)
	client := relayClient(t, r)

	if !strings.HasPrefix(r.host, "https://127.0.0.1:") {
		t.Errorf("the relay is at %q, not on the loopback address over TLS", r.host)
	}
	for _, auth := range []string{"", "Bearer ", "Bearer " + r.secret[:len(r.secret)-1], "Bearer " + r.secret + "x",
		"bearer " + r.secret, r.secret, "Bearer " + tok} {
		code, body := relayDo(t, client, http.MethodGet, r.host+"/api/v1/namespaces", auth, "")
		if code != http.StatusUnauthorized || !strings.Contains(body, `"kind":"Status"`) {
			t.Errorf("Authorization %q: got %d %s, want a 401 Status", auth, code, body)
		}
	}
	if n := len(cl.requests()); n != 0 {
		t.Fatalf("%d refused requests reached the cluster", n)
	}

	code, body := relayDo(t, client, http.MethodGet, r.host+"/api/v1/namespaces?limit=1", "Bearer "+r.secret, "")
	if code != http.StatusOK {
		t.Fatalf("with the secret: %d %s", code, body)
	}
	seen := cl.requests()
	if len(seen) != 1 || seen[0].token != tok || seen[0].path != "/gw/c1/api/v1/namespaces" {
		t.Errorf("the cluster saw %+v, want the platform's token on the server's path", seen)
	}
}

func TestTheRelayFetchesBeforeTheTokenExpires(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	cl := newRelayCluster(t)
	first := testJWT("first", clock.now().Add(15*time.Minute))
	second := testJWT("second", clock.now().Add(29*time.Minute))
	cl.allow(first, second)
	f := &relayFetcher{tokens: []string{second}}
	r := startTestRelay(t, cl, first, f, clock)
	client := relayClient(t, r)
	get := func() {
		t.Helper()
		if code, body := relayDo(t, client, http.MethodGet, r.host+"/api", "Bearer "+r.secret, ""); code != http.StatusOK {
			t.Fatalf("got %d %s", code, body)
		}
	}

	get()
	clock.advance(13*time.Minute + 59*time.Second)
	get()
	if n := f.calls.Load(); n != 0 {
		t.Fatalf("fetched %d times before the token was due", n)
	}
	// Within a minute of `exp`, a request starts a fetch but uses the current token.
	clock.advance(2 * time.Second)
	get()
	waitForRelayToken(t, r, second)
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("fetched %d times once the token was due, want 1", n)
	}
	// Revoked, so a request still using it would fail.
	cl.revoke(first)
	get()
	for _, s := range cl.requests() {
		if s.status == http.StatusUnauthorized {
			t.Errorf("the cluster had to refuse a token: %+v", s)
		}
	}
	if last := cl.requests()[3]; last.token != second {
		t.Errorf("the request after the fetch carried %q, want the new token", last.token)
	}
}

func waitForRelayToken(t *testing.T, r *kubeconfigRelay, token string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		r.mu.Lock()
		got := r.up.conn.Token
		r.mu.Unlock()
		if got == token {
			return
		}
	}
	t.Fatal("the fetch never replaced the token")
}

func TestASlowFetchHoldsBackOnlyRequestsWithAnExpiredToken(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	cl := newRelayCluster(t)
	first := testJWT("first", clock.now().Add(15*time.Minute))
	second := testJWT("second", clock.now().Add(30*time.Minute))
	cl.allow(first, second)
	f := &relayFetcher{tokens: []string{second}, gate: make(chan struct{})}
	r := startTestRelay(t, cl, first, f, clock)
	client := relayClient(t, r)
	client.Timeout = 5 * time.Second
	clock.advance(14*time.Minute + 30*time.Second)

	for range 3 {
		if code, body := relayDo(t, client, http.MethodGet, r.host+"/api", "Bearer "+r.secret, ""); code != http.StatusOK {
			t.Fatalf("a request with a token the cluster still accepts got %d %s while the fetch was held", code, body)
		}
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("%d fetches, want the one the first due request started", n)
	}
	for _, s := range cl.requests() {
		if s.token != first {
			t.Errorf("a request while the fetch was held carried %q, want the current token", s.token)
		}
	}

	// Expired: a request now waits for the fetch.
	clock.advance(time.Minute)
	req, err := http.NewRequest(http.MethodGet, r.host+"/api", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.secret)
	got := make(chan int, 1)
	go func() {
		resp, err := client.Do(req)
		if err != nil {
			got <- 0
			return
		}
		_ = resp.Body.Close()
		got <- resp.StatusCode
	}()
	select {
	case code := <-got:
		t.Fatalf("a request with an expired token got %d without waiting for the fetch", code)
	case <-time.After(200 * time.Millisecond):
	}
	close(f.gate)
	if code := <-got; code != http.StatusOK {
		t.Fatalf("the request that waited for the fetch got %d", code)
	}
	if last := cl.requests()[len(cl.requests())-1]; last.token != second {
		t.Errorf("the request after the fetch carried %q, want the new token", last.token)
	}
}

// A token without `exp` is known expired only when the cluster says so.
func TestTheRelayFetchesAndReplaysOnA401(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	cl := newRelayCluster(t)
	cl.allow("opaque-2")
	f := &relayFetcher{tokens: []string{"opaque-2"}}
	r := startTestRelay(t, cl, "opaque-1", f, clock)
	client := relayClient(t, r)

	payload := `{"kind":"ConfigMap","metadata":{"name":"x"}}`
	code, body := relayDo(t, client, http.MethodPost, r.host+"/api/v1/namespaces/default/configmaps", "Bearer "+r.secret, payload)
	if code != http.StatusOK {
		t.Fatalf("got %d %s", code, body)
	}
	seen := cl.requests()
	if len(seen) != 2 || seen[0].token != "opaque-1" || seen[0].status != http.StatusUnauthorized ||
		seen[1].token != "opaque-2" || seen[1].status != http.StatusOK {
		t.Fatalf("the cluster saw %+v, want a 401 and then the replay with the new token", seen)
	}
	if seen[0].body != payload || seen[1].body != payload {
		t.Errorf("bodies %q and %q, want the request's body both times", seen[0].body, seen[1].body)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1", n)
	}
}

// A 401 on a just-fetched token isn't expiry, so the relay passes it back.
func TestTheRelayDoesNotFetchForAFreshTokenTheClusterRefuses(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	cl := newRelayCluster(t)
	f := &relayFetcher{tokens: []string{"opaque-2", "opaque-3"}}
	r := startTestRelay(t, cl, "opaque-1", f, clock)
	client := relayClient(t, r)

	// Past the fetch gap each time, so only freshness holds the relay back.
	for i := range 3 {
		code, _ := relayDo(t, client, http.MethodGet, r.host+"/api", "Bearer "+r.secret, "")
		if code != http.StatusUnauthorized {
			t.Fatalf("request %d: got %d, want the cluster's 401", i, code)
		}
		clock.advance(relayMaxFetchGap + time.Second)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("fetched %d times for a refused fresh token, want 1", n)
	}
	clock.advance(relayFreshFor)
	if code, _ := relayDo(t, client, http.MethodGet, r.host+"/api", "Bearer "+r.secret, ""); code != http.StatusUnauthorized {
		t.Fatalf("got %d, want the cluster's 401", code)
	}
	if n := f.calls.Load(); n != 2 {
		t.Errorf("fetched %d times once the token was no longer fresh, want 2", n)
	}
}

// The error carries the platform's reason, and the relay doesn't refetch on
// every request.
func TestTheRelayReportsAFetchThePlatformRefuses(t *testing.T) {
	t.Parallel()
	refusal := errors.New("Could not download the kubeconfig: the service account is disabled")

	t.Run("token expired", func(t *testing.T) {
		t.Parallel()
		clock := newTestClock()
		cl := newRelayCluster(t)
		first := testJWT("first", clock.now().Add(15*time.Minute))
		cl.allow(first)
		f := &relayFetcher{err: refusal}
		r := startTestRelay(t, cl, first, f, clock)
		client := relayClient(t, r)

		// Due but not expired: the fetch fails, and the token's last minute still works.
		clock.advance(14*time.Minute + 30*time.Second)
		if code, body := relayDo(t, client, http.MethodGet, r.host+"/api", "Bearer "+r.secret, ""); code != http.StatusOK {
			t.Fatalf("with a minute left: got %d %s", code, body)
		}
		clock.advance(31 * time.Second)
		cl.revoke(first)
		for range 3 {
			code, body := relayDo(t, client, http.MethodGet, r.host+"/api", "Bearer "+r.secret, "")
			if code != http.StatusBadGateway || !strings.Contains(body, "the service account is disabled") ||
				!strings.Contains(body, "could not fetch a new kubeconfig") {
				t.Fatalf("got %d %s, want a 502 that names the platform's refusal", code, body)
			}
		}
		if n := f.calls.Load(); n != 2 {
			t.Errorf("fetched %d times, want 2: once when due and once when expired, not once a request", n)
		}
		for _, s := range cl.requests() {
			if s.status == http.StatusUnauthorized {
				t.Errorf("a request reached the cluster with an expired token: %+v", s)
			}
		}
	})

	t.Run("token refused", func(t *testing.T) {
		t.Parallel()
		clock := newTestClock()
		cl := newRelayCluster(t)
		f := &relayFetcher{err: refusal}
		r := startTestRelay(t, cl, "opaque-1", f, clock)
		client := relayClient(t, r)
		code, body := relayDo(t, client, http.MethodGet, r.host+"/api", "Bearer "+r.secret, "")
		if code != http.StatusBadGateway || !strings.Contains(body, "the service account is disabled") ||
			!strings.Contains(body, "the cluster refused the token") {
			t.Fatalf("got %d %s, want a 502 that names the platform's refusal", code, body)
		}
		if n := f.calls.Load(); n != 1 {
			t.Errorf("fetched %d times, want 1", n)
		}
	})
}

// Under -race, this also checks the token swap.
func TestConcurrentRequestsShareOneFetch(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	cl := newRelayCluster(t)
	first := testJWT("first", clock.now().Add(15*time.Minute))
	second := testJWT("second", clock.now().Add(30*time.Minute))
	cl.allow(second)
	f := &relayFetcher{tokens: []string{second}, gate: make(chan struct{})}
	r := startTestRelay(t, cl, first, f, clock)
	client := relayClient(t, r)
	clock.advance(15 * time.Minute)

	const n = 20
	var wg sync.WaitGroup
	codes := make(chan int, n)
	for range n {
		wg.Go(func() {
			req, err := http.NewRequest(http.MethodGet, r.host+"/api", nil)
			if err != nil {
				codes <- 0
				return
			}
			req.Header.Set("Authorization", "Bearer "+r.secret)
			resp, err := client.Do(req)
			if err != nil {
				codes <- 0
				return
			}
			defer func() { _ = resp.Body.Close() }()
			if _, err := io.Copy(io.Discard, resp.Body); err != nil {
				codes <- 0
				return
			}
			codes <- resp.StatusCode
		})
	}
	for f.calls.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(f.gate)
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != http.StatusOK {
			t.Errorf("a request got %d", code)
		}
	}
	if got := f.calls.Load(); got != 1 {
		t.Errorf("%d fetches for %d concurrent requests, want 1", got, n)
	}
	if got := len(cl.requests()); got != n {
		t.Errorf("the cluster saw %d requests, want %d", got, n)
	}
}

// Well past the limit, so most of it is still streaming when the relay stops buffering.
func TestABodyOverTheLimitIsSentOnce(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	cl := newRelayCluster(t)
	cl.allow("opaque-2")
	f := &relayFetcher{tokens: []string{"opaque-2"}}
	r := startTestRelay(t, cl, "opaque-1", f, clock)
	client := relayClient(t, r)

	big := strings.Repeat("x", relayBodyLimit+64<<10)
	code, _ := relayDo(t, client, http.MethodPost, r.host+"/api", "Bearer "+r.secret, big)
	if code != http.StatusUnauthorized {
		t.Fatalf("got %d, want the cluster's 401", code)
	}
	seen := cl.requests()
	if len(seen) != 1 || len(seen[0].body) != len(big) {
		t.Fatalf("the cluster saw %d requests, want the whole body once", len(seen))
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("fetched %d times for a request that couldn't be sent again", n)
	}
}

func TestAWatchStreamsAndEndsOnClose(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	cl := newRelayCluster(t)
	cl.allow("opaque-1")
	cl.handle("/gw/c1/api/v1/pods", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := io.WriteString(w, `{"type":"ADDED"}`+"\n"); err != nil {
			return
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	})
	f := &relayFetcher{}
	f.cl = cl
	r, err := startKubeconfigRelayAt(context.Background(), cl.conn("opaque-1"), f.fetch, time.Minute, clock.now)
	if err != nil {
		t.Fatal(err)
	}
	client := relayClient(t, r)
	req, err := http.NewRequest(http.MethodGet, r.host+"/api/v1/pods?watch=1", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.secret)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, "ADDED") {
		t.Fatalf("first event %q, %v", line, err)
	}

	closed := make(chan struct{})
	go func() { r.close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("close waited on the open watch")
	}
	done := make(chan error, 1)
	go func() { _, err := io.Copy(io.Discard, resp.Body); done <- err }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the watch kept streaming after close")
	}
	if _, err := client.Get(r.host + "/api"); err == nil {
		t.Error("the relay still accepts connections after close")
	}
}

// kubectl exec and port-forward upgrade. An upgrade has no body, so it can replay.
func TestAnUpgradeGoesThroughAfterAReplay(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	cl := newRelayCluster(t)
	cl.allow("opaque-2")
	cl.handle("/gw/c1/api/v1/namespaces/default/pods/p/exec", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "test-stream" {
			http.Error(w, "no upgrade", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the test cluster can't hijack a connection")
			return
		}
		conn, rw, err := hj.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if _, err = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test-stream\r\n\r\n"); err != nil {
			return
		}
		if err = rw.Flush(); err != nil {
			return
		}
		line, err := rw.ReadString('\n')
		if err != nil {
			return
		}
		if _, err = rw.WriteString("echo " + line); err != nil {
			return
		}
		if err = rw.Flush(); err != nil {
			t.Errorf("echo: %v", err)
		}
	})
	f := &relayFetcher{tokens: []string{"opaque-2"}}
	r := startTestRelay(t, cl, "opaque-1", f, clock)

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(r.caPEM))
	conn, err := tls.Dial("tcp", strings.TrimPrefix(r.host, "https://"), &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "POST /api/v1/namespaces/default/pods/p/exec HTTP/1.1\r\nHost: x\r\n"+
		"Authorization: Bearer %s\r\nConnection: Upgrade\r\nUpgrade: test-stream\r\n\r\n", r.secret)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("got %d, want 101", resp.StatusCode)
	}
	fmt.Fprint(conn, "ping\n")
	line, err := br.ReadString('\n')
	if err != nil || line != "echo ping\n" {
		t.Fatalf("read %q, %v", line, err)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1", n)
	}
}

func TestTokenExpiry(t *testing.T) {
	t.Parallel()
	enc := base64.RawURLEncoding.EncodeToString
	exp := time.Unix(1_800_000_900, 0)
	for _, tc := range []struct {
		name, token string
		want        time.Time
	}{
		{"a JWT", testJWT("x", exp), exp},
		{"a padded payload", enc([]byte("{}")) + "." + base64.URLEncoding.EncodeToString([]byte(`{"exp":1800000900}`)) + ".s", exp},
		{"a fractional exp", "h." + enc([]byte(`{"exp":1800000900.5}`)) + ".s", exp},
		{"no exp", "h." + enc([]byte(`{"sub":"x"}`)) + ".s", time.Time{}},
		{"not JSON", "h." + enc([]byte(`exp`)) + ".s", time.Time{}},
		{"opaque", "not-a-jwt", time.Time{}},
		{"two parts", "a.b", time.Time{}},
	} {
		if got := tokenExpiry(tc.token); !got.Equal(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Every attribute, `raw` included, points at the relay. Only names carry over.
func TestTheEphemeralResultPointsAtTheRelay(t *testing.T) {
	t.Parallel()
	// The relay checks the cluster's CA on open, so this one is real.
	clusterCA, _, err := relayCertificates()
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(twoContexts, "https://training.invalid:6443", "https://training.invalid:6443/gw", 1)
	body = strings.Replace(body, b64(testCAPEM), b64(clusterCA), 1)
	body = strings.Replace(body, "context: {cluster: training, user: training-user}",
		"context: {cluster: training, user: training-user, namespace: jobs}", 1)
	p := &kubeconfigPlatform{clusterState: "active", body: body}
	meta := p.meta(t, time.Minute)
	e := &clusterKubeconfigEphemeral{meta: meta}

	ctx := context.Background()
	var schemaResp ephemeral.SchemaResponse
	e.Schema(ctx, ephemeral.SchemaRequest{}, &schemaResp)
	object, ok := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	if !ok {
		t.Fatal("ephemeral schema is not an object")
	}
	values := make(map[string]tftypes.Value, len(object.AttributeTypes))
	for name, attrType := range object.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	values["cluster_id"] = tftypes.NewValue(tftypes.String, "c1")
	values["relay"] = tftypes.NewValue(tftypes.Bool, true)
	config := tfsdk.Config{Raw: tftypes.NewValue(object, values), Schema: schemaResp.Schema}

	resp := ephemeral.OpenResponse{Result: tfsdk.EphemeralResultData{Schema: schemaResp.Schema}}
	initPrivate(&resp.Private)
	e.Open(ctx, ephemeral.OpenRequest{Config: config}, &resp)
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() > 0 {
		t.Fatalf("diagnostics: %v", resp.Diagnostics)
	}
	var got clusterKubeconfigEphemeralModel
	resp.Diagnostics.Append(resp.Result.Get(ctx, &got)...)
	if resp.Diagnostics.HasError() {
		t.Fatalf("result: %v", resp.Diagnostics)
	}
	if !strings.HasPrefix(got.Host.ValueString(), "https://127.0.0.1:") {
		t.Errorf("host %q is not the relay", got.Host.ValueString())
	}
	if got.Token.ValueString() == "training-token" || len(got.Token.ValueString()) != 64 {
		t.Errorf("token is not the relay's secret")
	}
	for _, leaked := range []string{"training-token", "other-token", "training.invalid", "other"} {
		if strings.Contains(got.Raw.ValueString(), leaked) {
			t.Errorf("raw carries %q from the platform's kubeconfig", leaked)
		}
	}
	conn, warnings := parseKubeconfigConnection(got.Raw.ValueString())
	if len(warnings) > 0 || conn != (kubeconfigConnection{Host: got.Host.ValueString(),
		ClusterCACertificate: got.ClusterCACertificate.ValueString(), Token: got.Token.ValueString()}) {
		t.Errorf("raw doesn't hand over what the attributes do: %v", warnings)
	}
	if names := currentKubeconfigNames(got.Raw.ValueString()); names != (kubeconfigNames{
		context: "training", cluster: "training", user: "training-user", namespace: "jobs"}) {
		t.Errorf("raw's names are %+v, want the platform kubeconfig's", names)
	}

	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM([]byte(got.ClusterCACertificate.ValueString()))
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	if code, _ := relayDo(t, client, http.MethodGet, got.Host.ValueString()+"/version", "", ""); code != http.StatusUnauthorized {
		t.Errorf("the relay answered %d without the secret, want 401", code)
	}
	closeResp := ephemeral.CloseResponse{}
	e.Close(ctx, ephemeral.CloseRequest{Private: resp.Private}, &closeResp)
	if closeResp.Diagnostics.HasError() {
		t.Fatalf("close: %v", closeResp.Diagnostics)
	}
	if _, err := client.Get(got.Host.ValueString() + "/version"); err == nil {
		t.Error("the relay still answers after Close")
	}
	if len(meta.relays.open) != 0 {
		t.Errorf("%d relays still registered after Close", len(meta.relays.open))
	}
}

// No token to keep current, so the result points at the cluster, with a warning.
func TestACertificateKubeconfigIsNotRelayed(t *testing.T) {
	t.Parallel()
	body := `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://c.invalid:6443"}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
users: [{name: u, user: {client-certificate-data: ` + b64(testCertPEM) + `, client-key-data: ` + b64(testKeyPEM) + `}}]
`
	p := &kubeconfigPlatform{clusterState: "active", body: body}
	e := &clusterKubeconfigEphemeral{meta: p.meta(t, time.Minute)}
	ctx := context.Background()
	var schemaResp ephemeral.SchemaResponse
	e.Schema(ctx, ephemeral.SchemaRequest{}, &schemaResp)
	object, ok := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	if !ok {
		t.Fatal("ephemeral schema is not an object")
	}
	values := make(map[string]tftypes.Value, len(object.AttributeTypes))
	for name, attrType := range object.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	values["cluster_id"] = tftypes.NewValue(tftypes.String, "c1")
	values["relay"] = tftypes.NewValue(tftypes.Bool, true)
	resp := ephemeral.OpenResponse{Result: tfsdk.EphemeralResultData{Schema: schemaResp.Schema}}
	initPrivate(&resp.Private)
	e.Open(ctx, ephemeral.OpenRequest{Config: tfsdk.Config{Raw: tftypes.NewValue(object, values), Schema: schemaResp.Schema}}, &resp)
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 ||
		!strings.Contains(resp.Diagnostics.Warnings()[0].Detail(), "client certificate and no token") {
		t.Fatalf("diagnostics: %v", resp.Diagnostics)
	}
	var got clusterKubeconfigEphemeralModel
	resp.Diagnostics.Append(resp.Result.Get(ctx, &got)...)
	if got.Host.ValueString() != "https://c.invalid:6443" || got.ClientCertificate.ValueString() != testCertPEM ||
		got.Raw.ValueString() != body {
		t.Errorf("the result does not point at the cluster with the certificate kubeconfig")
	}
}

// The framework's private-data type is internal, so build it from the field's type.
func initPrivate(field any) {
	v := reflect.ValueOf(field).Elem()
	v.Set(reflect.New(v.Type().Elem()))
}

// The relay's Status bodies are what kubectl and the kubernetes provider show.
func TestARelayStatusIsAKubernetesStatus(t *testing.T) {
	t.Parallel()
	w := httptest.NewRecorder()
	writeRelayStatus(w, http.StatusBadGateway, "BadGateway", "a reason")
	var status struct {
		Kind    string `json:"kind"`
		Message string `json:"message"`
		Code    int    `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil || status.Kind != "Status" ||
		status.Message != "a reason" || status.Code != http.StatusBadGateway || w.Code != http.StatusBadGateway {
		t.Errorf("got %d %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte(`"apiVersion":"v1"`)) {
		t.Errorf("no apiVersion: %s", w.Body.String())
	}
}

// Only the authenticator refuses before the request runs. A 401 from a proxied
// pod, a webhook or a front proxy may follow side effects, so it passes through.
func TestTheRelayReplaysOnlyTheAuthenticatorsRefusal(t *testing.T) {
	t.Parallel()
	answer := func(contentType, body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			if contentType != "" {
				w.Header().Set("Content-Type", contentType)
			}
			w.WriteHeader(http.StatusUnauthorized)
			if _, err := io.WriteString(w, body); err != nil {
				t.Errorf("the backend could not answer: %v", err)
			}
		}
	}
	authenticator := `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`
	for name, tc := range map[string]struct {
		path   string
		answer http.HandlerFunc
		body   string
	}{
		"a service behind the proxy, in the authenticator's words": {
			"/api/v1/namespaces/default/services/pay:80/proxy/charge", answer("application/json", authenticator), authenticator},
		"a pod behind the proxy, with no body": {
			"/api/v1/namespaces/default/pods/pay/proxy/charge", answer("", ""), ""},
		"a node behind the proxy, in protobuf": {
			"/api/v1/nodes/n1/proxy/charge", answer(relayProtobuf, string(protobufStatus("Unauthorized"))), string(protobufStatus("Unauthorized"))},
		"an admission webhook": {
			"/api/v1/namespaces/default/configmaps",
			answer("application/json", strings.Replace(authenticator, `"message":"Unauthorized"`,
				`"message":"admission webhook \"pay.example.invalid\" denied the request: Unauthorized"`, 1)),
			`admission webhook \"pay.example.invalid\" denied the request`},
		"something in front of the API server": {
			"/api/v1/namespaces/default/configmaps", answer("application/json", `{"message":"Unauthorized"}`), `{"message":"Unauthorized"}`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			clock := newTestClock()
			cl := newRelayCluster(t)
			cl.allow("opaque-1", "opaque-2")
			var acted atomic.Int32
			cl.handle("/gw/c1"+tc.path, func(w http.ResponseWriter, r *http.Request) {
				acted.Add(1)
				tc.answer(w, r)
			})
			f := &relayFetcher{tokens: []string{"opaque-2"}}
			r := startTestRelay(t, cl, "opaque-1", f, clock)
			code, body := relayDo(t, relayClient(t, r), http.MethodPost, r.host+tc.path, "Bearer "+r.secret, `{"amount":1}`)
			if code != http.StatusUnauthorized || acted.Load() != 1 || f.calls.Load() != 0 {
				t.Errorf("the client got %d, the backend acted %d times and the relay fetched %d times; "+
					"want the 401 passed back, the backend run once and no fetch", code, acted.Load(), f.calls.Load())
			}
			if !strings.Contains(body, tc.body) || (tc.body == "" && body != "") {
				t.Errorf("the client got %q, want the backend's answer %q as it was", body, tc.body)
			}
		})
	}
}

// kubectl can ask for protobuf as well as JSON.
func TestTheRelayReplaysTheAuthenticatorsRefusalInProtobuf(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	cl := newRelayCluster(t)
	cl.allow("opaque-2")
	f := &relayFetcher{tokens: []string{"opaque-2"}}
	r := startTestRelay(t, cl, "opaque-1", f, clock)
	req, err := http.NewRequest(http.MethodPost, r.host+"/api/v1/namespaces/default/configmaps", strings.NewReader(`{"kind":"ConfigMap"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.secret)
	req.Header.Set("Accept", relayProtobuf+", application/json")
	resp, err := relayClient(t, r).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	seen := cl.requests()
	if resp.StatusCode != http.StatusOK || len(seen) != 2 || seen[0].status != http.StatusUnauthorized || seen[1].token != "opaque-2" {
		t.Fatalf("got %d, and the cluster saw %+v; want a protobuf 401 and then the replay with the new token", resp.StatusCode, seen)
	}
}

func TestTokenRefusedReadsTheAuthenticatorsStatus(t *testing.T) {
	t.Parallel()
	authenticator := `{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`
	for _, tc := range []struct {
		name, path, body string
		code             int
		want             bool
	}{
		{"the authenticator, in JSON", "/api/v1/namespaces", authenticator, 401, true},
		{"the authenticator, in protobuf", "/apis/apps/v1/deployments", string(protobufStatus("Unauthorized")), 401, true},
		{"another message, in protobuf", "/api/v1/namespaces", string(protobufStatus("token expired")), 401, false},
		{"a 403 in the same words", "/api/v1/namespaces", strings.Replace(authenticator, "401", "403", 1), 403, false},
		{"a Status that says 403", "/api/v1/namespaces", strings.Replace(authenticator, "401", "403", 1), 401, false},
		{"no reason", "/api/v1/namespaces", strings.Replace(authenticator, `"reason":"Unauthorized",`, "", 1), 401, false},
		{"not a Status", "/api/v1/namespaces", strings.Replace(authenticator, `"Status"`, `"Pod"`, 1), 401, false},
		{"not JSON", "/api/v1/namespaces", "Unauthorized\n", 401, false},
		{"broken protobuf", "/api/v1/namespaces", "k8s\x00\x0a\xff", 401, false},
		{"too long", "/api/v1/namespaces", authenticator + strings.Repeat(" ", relayStatusPeek), 401, false},
		{"a proxy path", "/api/v1/namespaces/default/services/s/proxy", authenticator, 401, false},
		{"a proxy path, escaped", "/api/v1/namespaces/default/pods/p/%70roxy/x", authenticator, 401, false},
	} {
		out := httptest.NewRequest(http.MethodGet, tc.path, nil)
		resp := &http.Response{StatusCode: tc.code, Body: io.NopCloser(strings.NewReader(tc.body))}
		if got := tokenRefused(out, resp); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
		if b, err := io.ReadAll(resp.Body); err != nil || string(b) != tc.body {
			t.Errorf("%s: the body passed on is %q, want it whole", tc.name, b)
		}
	}
}

const relayProtobuf = "application/vnd.kubernetes.protobuf"

// An API server's protobuf 401: "k8s\x00", then a runtime.Unknown. Kubernetes'
// generated code writes every string field, even empty ones.
func protobufStatus(message string) []byte {
	field := func(b []byte, num protowire.Number, v []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(b, num, protowire.BytesType), v)
	}
	status := field(nil, 1, nil)
	status = field(status, 2, []byte("Failure"))
	status = field(status, 3, []byte(message))
	status = field(status, 4, []byte("Unauthorized"))
	status = protowire.AppendVarint(protowire.AppendTag(status, 6, protowire.VarintType), 401)
	typeMeta := field(field(nil, 1, []byte("v1")), 2, []byte("Status"))
	unknown := field(field(field(field(nil, 1, typeMeta), 2, status), 3, nil), 4, nil)
	return append([]byte("k8s\x00"), unknown...)
}

// Taking `exp` at its word here would fetch on every request and fail them all
// while the platform refuses. The cluster's 401 decides.
func TestTheRelayIgnoresAnExpAlreadyPastByTheLocalClock(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	cl := newRelayCluster(t)
	tok := testJWT("first", clock.now().Add(-5*time.Minute))
	cl.allow(tok)
	f := &relayFetcher{err: errors.New("the platform is down")}
	r := startTestRelay(t, cl, tok, f, clock)
	if code, body := relayDo(t, relayClient(t, r), http.MethodGet, r.host+"/api", "Bearer "+r.secret, ""); code != http.StatusOK {
		t.Fatalf("got %d %s, want the cluster's answer to the token it accepts", code, body)
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("fetched %d times for a token the cluster accepts", n)
	}
}

// A 20s token is due 5s before `exp`. After a failed fetch the relay retries at
// `exp`, not after the 10s gap a longer token gets.
func TestTheRelayWaitsAQuarterOfAShortTokensLifetimeBetweenFetches(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	cl := newRelayCluster(t)
	first := testJWT("first", clock.now().Add(20*time.Second))
	second := testJWT("second", clock.now().Add(40*time.Second))
	cl.allow(first, second)
	f := &relayFetcher{err: errors.New("the platform is busy")}
	r := startTestRelay(t, cl, first, f, clock)
	client := relayClient(t, r)

	clock.advance(15 * time.Second)
	if code, body := relayDo(t, client, http.MethodGet, r.host+"/api", "Bearer "+r.secret, ""); code != http.StatusOK {
		t.Fatalf("with 5 seconds left: got %d %s", code, body)
	}
	f.set(nil, second)
	clock.advance(5 * time.Second)
	cl.revoke(first)
	if code, body := relayDo(t, client, http.MethodGet, r.host+"/api", "Bearer "+r.secret, ""); code != http.StatusOK {
		t.Fatalf("at `exp`, 5 seconds after the failed fetch: got %d %s, want the request sent with a new token", code, body)
	}
	if n := f.calls.Load(); n != 2 {
		t.Errorf("fetched %d times, want 2", n)
	}
}

// A cluster that then refuses the new token gets its own 401, not the old failure.
func TestASuccessfulFetchClearsTheLastFailure(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	cl := newRelayCluster(t)
	first := testJWT("first", clock.now().Add(15*time.Minute))
	second := testJWT("second", clock.now().Add(30*time.Minute))
	cl.allow(first)
	f := &relayFetcher{err: errors.New("the platform is busy")}
	r := startTestRelay(t, cl, first, f, clock)
	client := relayClient(t, r)

	clock.advance(14*time.Minute + 30*time.Second)
	if code, body := relayDo(t, client, http.MethodGet, r.host+"/api", "Bearer "+r.secret, ""); code != http.StatusOK {
		t.Fatalf("with 30 seconds left: got %d %s", code, body)
	}
	f.set(nil, second)
	clock.advance(relayMaxFetchGap + time.Second)
	// This request starts the fetch and goes with the first token, still good.
	if code, body := relayDo(t, client, http.MethodGet, r.host+"/api", "Bearer "+r.secret, ""); code != http.StatusOK {
		t.Fatalf("with 19 seconds left: got %d %s", code, body)
	}
	waitForRelayToken(t, r, second)
	r.mu.Lock()
	err := r.fetchErr
	r.mu.Unlock()
	if err != nil {
		t.Errorf("the successful fetch left the earlier failure: %v", err)
	}
	code, body := relayDo(t, client, http.MethodGet, r.host+"/api", "Bearer "+r.secret, "")
	if code != http.StatusUnauthorized || strings.Contains(body, "the platform is busy") {
		t.Errorf("got %d %s, want the cluster's 401 for the new token, not the earlier failure", code, body)
	}
	if n := f.calls.Load(); n != 2 {
		t.Errorf("fetched %d times, want 2", n)
	}
}

func TestTheRelayDoesNotReplayWithTheSameToken(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	cl := newRelayCluster(t)
	f := &relayFetcher{tokens: []string{"opaque-1"}}
	r := startTestRelay(t, cl, "opaque-1", f, clock)
	code, _ := relayDo(t, relayClient(t, r), http.MethodPost, r.host+"/api/v1/namespaces/default/configmaps", "Bearer "+r.secret, `{}`)
	if n := len(cl.requests()); code != http.StatusUnauthorized || n != 1 {
		t.Errorf("got %d, and the cluster saw %d requests; want its 401 and the request sent once", code, n)
	}
	if n := f.calls.Load(); n != 1 {
		t.Errorf("fetched %d times, want 1", n)
	}
}

func TestTheRelayPresentsTheClientCertificate(t *testing.T) {
	t.Parallel()
	certPEM, keyPEM, cert := testClientCertificate(t)
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	cl := newRelayClusterTLS(t, &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, MinVersion: tls.VersionTLS12})
	cl.allow("opaque-1")
	f := &relayFetcher{cl: cl}
	conn := cl.conn("opaque-1")
	conn.ClientCertificate, conn.ClientKey = certPEM, keyPEM
	r, err := startKubeconfigRelayAt(context.Background(), conn, f.fetch, time.Minute, newTestClock().now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	if code, body := relayDo(t, relayClient(t, r), http.MethodGet, r.host+"/api", "Bearer "+r.secret, ""); code != http.StatusOK {
		t.Fatalf("got %d %s, want the cluster to accept the relay's client certificate", code, body)
	}
}

// The relay presents the certificate and uses tls-server-name, so neither warns.
func TestTheEphemeralResultHasNoClientCertificateWhenRelayed(t *testing.T) {
	t.Parallel()
	certPEM, keyPEM, _ := testClientCertificate(t)
	clusterCA, _, err := relayCertificates()
	if err != nil {
		t.Fatal(err)
	}
	body := `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://c.invalid:6443", tls-server-name: api.c.invalid, certificate-authority-data: ` + b64(clusterCA) + `}}]
contexts: [{name: c, context: {cluster: c, user: u}}]
current-context: c
users: [{name: u, user: {token: platform-token, client-certificate-data: ` + b64(certPEM) + `, client-key-data: ` + b64(keyPEM) + `}}]
`
	p := &kubeconfigPlatform{clusterState: "active", body: body}
	e := &clusterKubeconfigEphemeral{meta: p.meta(t, time.Minute)}
	resp, got := openTestEphemeral(t, e, nil)
	t.Cleanup(func() {
		e.Close(context.Background(), ephemeral.CloseRequest{Private: resp.Private}, &ephemeral.CloseResponse{})
	})
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() > 0 {
		t.Fatalf("diagnostics: %v", resp.Diagnostics)
	}
	if !strings.HasPrefix(got.Host.ValueString(), "https://127.0.0.1:") {
		t.Fatalf("host %q is not the relay", got.Host.ValueString())
	}
	if !got.ClientCertificate.IsNull() || !got.ClientKey.IsNull() {
		t.Errorf("client_certificate and client_key are %v and set=%v, want both null", got.ClientCertificate, !got.ClientKey.IsNull())
	}
}

// tweak changes the response before the open, for a test of an open that fails.
func openTestEphemeral(t *testing.T, e *clusterKubeconfigEphemeral, tweak func(*ephemeral.OpenResponse)) (ephemeral.OpenResponse, clusterKubeconfigEphemeralModel) {
	t.Helper()
	ctx := context.Background()
	var schemaResp ephemeral.SchemaResponse
	e.Schema(ctx, ephemeral.SchemaRequest{}, &schemaResp)
	object, ok := schemaResp.Schema.Type().TerraformType(ctx).(tftypes.Object)
	if !ok {
		t.Fatal("ephemeral schema is not an object")
	}
	values := make(map[string]tftypes.Value, len(object.AttributeTypes))
	for name, attrType := range object.AttributeTypes {
		values[name] = tftypes.NewValue(attrType, nil)
	}
	values["cluster_id"] = tftypes.NewValue(tftypes.String, "c1")
	values["relay"] = tftypes.NewValue(tftypes.Bool, true)
	resp := ephemeral.OpenResponse{Result: tfsdk.EphemeralResultData{Schema: schemaResp.Schema}}
	initPrivate(&resp.Private)
	if tweak != nil {
		tweak(&resp)
	}
	e.Open(ctx, ephemeral.OpenRequest{Config: tfsdk.Config{Raw: tftypes.NewValue(object, values), Schema: schemaResp.Schema}}, &resp)
	var got clusterKubeconfigEphemeralModel
	if !resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.Result.Get(ctx, &got)...)
	}
	return resp, got
}

func testClientCertificate(t *testing.T) (string, string, *x509.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "relay-test-user"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})), cert
}

// Terraform calls Close only after an open that succeeded.
func TestAnOpenThatFailsStopsTheRelay(t *testing.T) {
	t.Parallel()
	clusterCA, _, err := relayCertificates()
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(twoContexts, b64(testCAPEM), b64(clusterCA), 1)
	for name, tweak := range map[string]func(*ephemeral.OpenResponse){
		"the private data can't be set": func(resp *ephemeral.OpenResponse) { resp.Private = nil },
		"the result can't be set": func(resp *ephemeral.OpenResponse) {
			resp.Result.Schema = schema.Schema{Attributes: map[string]schema.Attribute{"host": schema.StringAttribute{Computed: true}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := &kubeconfigPlatform{clusterState: "active", body: body}
			meta := p.meta(t, time.Minute)
			resp, _ := openTestEphemeral(t, &clusterKubeconfigEphemeral{meta: meta}, tweak)
			if !resp.Diagnostics.HasError() {
				t.Fatal("the open succeeded")
			}
			meta.relays.mu.Lock()
			defer meta.relays.mu.Unlock()
			if n := len(meta.relays.open); n != 0 {
				t.Errorf("%d relays still open after the open failed", n)
			}
		})
	}
}

// As kubectl does. The test certificate names example.com and 127.0.0.1, not localhost.
func TestTheRelayConnectsWithTheTLSServerName(t *testing.T) {
	t.Parallel()
	cl := newRelayCluster(t)
	cl.allow("opaque-1")
	f := &relayFetcher{cl: cl}
	conn := cl.conn("opaque-1")
	conn.Host = strings.Replace(conn.Host, "127.0.0.1", "localhost", 1)
	conn.TLSServerName = "example.com"
	r, err := startKubeconfigRelayAt(context.Background(), conn, f.fetch, time.Minute, newTestClock().now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	if code, body := relayDo(t, relayClient(t, r), http.MethodGet, r.host+"/api", "Bearer "+r.secret, ""); code != http.StatusOK {
		t.Fatalf("got %d %s, want the cluster's certificate checked against example.com", code, body)
	}
}

// A proxy-url it can't use is refused without being quoted.
func TestTheRelayConnectsThroughTheProxyURL(t *testing.T) {
	t.Parallel()
	cl := newRelayCluster(t)
	cl.allow("opaque-1")
	var tunnels atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		upstream, err := net.Dial("tcp", req.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer func() { _ = upstream.Close() }()
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the test proxy can't hijack a connection")
			return
		}
		client, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer func() { _ = client.Close() }()
		tunnels.Add(1)
		if _, err := io.WriteString(client, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return
		}
		// Closing either side ends both copies, so neither error is a failure.
		go func() {
			if _, err := io.Copy(upstream, client); err != nil {
				return
			}
		}()
		if _, err := io.Copy(client, upstream); err != nil {
			return
		}
	}))
	t.Cleanup(proxy.Close)

	f := &relayFetcher{cl: cl}
	conn := cl.conn("opaque-1")
	conn.ProxyURL = proxy.URL
	r, err := startKubeconfigRelayAt(context.Background(), conn, f.fetch, time.Minute, newTestClock().now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	if code, body := relayDo(t, relayClient(t, r), http.MethodGet, r.host+"/api", "Bearer "+r.secret, ""); code != http.StatusOK {
		t.Fatalf("got %d %s", code, body)
	}
	if n := tunnels.Load(); n != 1 {
		t.Errorf("the proxy carried %d tunnels, want the request through it", n)
	}

	conn.ProxyURL = "ftp://ops:hunter2@proxy.invalid"
	if _, err := startKubeconfigRelayAt(context.Background(), conn, f.fetch, time.Minute, newTestClock().now); err == nil ||
		strings.Contains(err.Error(), "hunter2") {
		t.Errorf("an ftp proxy-url: %v, want an error that doesn't quote it", err)
	}
}

// The cluster, or a load balancer, may close after its 401. On a kept-alive
// connection the transport's own retry hides a replay that lost its body.
func TestTheRelayReplaysTheBodyOnANewConnection(t *testing.T) {
	t.Parallel()
	cl := newRelayCluster(t)
	cl.server.Config.SetKeepAlivesEnabled(false)
	cl.allow("opaque-2")
	f := &relayFetcher{tokens: []string{"opaque-2"}}
	r := startTestRelay(t, cl, "opaque-1", f, newTestClock())
	payload := `{"kind":"ConfigMap","metadata":{"name":"x"}}`
	code, body := relayDo(t, relayClient(t, r), http.MethodPost, r.host+"/api/v1/namespaces/default/configmaps", "Bearer "+r.secret, payload)
	seen := cl.requests()
	if code != http.StatusOK || len(seen) != 2 || seen[1].body != payload {
		t.Fatalf("got %d %s, and the cluster saw %+v; want the replay to carry the body", code, body, seen)
	}
}

// A 502, rather than holding every request until its client gives up.
func TestTheRelayStopsAFetchAtItsTimeLimit(t *testing.T) {
	t.Parallel()
	cl := newRelayCluster(t)
	f := &relayFetcher{gate: make(chan struct{}), cl: cl}
	r, err := startKubeconfigRelayAt(context.Background(), cl.conn("opaque-1"), f.fetch, 200*time.Millisecond, newTestClock().now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	client := relayClient(t, r)
	client.Timeout = 3 * time.Second
	code, body := relayDo(t, client, http.MethodGet, r.host+"/api", "Bearer "+r.secret, "")
	if code != http.StatusBadGateway || !strings.Contains(body, "deadline exceeded") {
		t.Errorf("got %d %s, want a 502 for the fetch that ran out of time", code, body)
	}
}

// One run's `token`, or a kubeconfig saved from its `raw`, gets nowhere with another's relay.
func TestEachRelayHasItsOwnSecret(t *testing.T) {
	t.Parallel()
	cl := newRelayCluster(t)
	cl.allow("opaque-1")
	a := startTestRelay(t, cl, "opaque-1", &relayFetcher{}, newTestClock())
	b := startTestRelay(t, cl, "opaque-1", &relayFetcher{}, newTestClock())
	if a.secret == b.secret || a.host == b.host {
		t.Errorf("two relays share a secret or an address")
	}
	if code, _ := relayDo(t, relayClient(t, b), http.MethodGet, b.host+"/api", "Bearer "+a.secret, ""); code != http.StatusUnauthorized {
		t.Errorf("one relay's secret got %d from another, want 401", code)
	}
}

// As after the platform rotates the CA.
func TestTheRelayConnectsWithTheNewKubeconfigsCA(t *testing.T) {
	t.Parallel()
	old := newRelayCluster(t)
	caPEM, cert, err := relayCertificates()
	if err != nil {
		t.Fatal(err)
	}
	moved := newRelayClusterTLS(t, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	moved.allow("opaque-2")
	next := moved.conn("opaque-2")
	next.ClusterCACertificate = caPEM
	fetch := func(context.Context) (kubeconfigConnection, error) { return next, nil }
	r, err := startKubeconfigRelayAt(context.Background(), old.conn("opaque-1"), fetch, time.Minute, newTestClock().now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.close)
	code, body := relayDo(t, relayClient(t, r), http.MethodGet, r.host+"/api", "Bearer "+r.secret, "")
	if code != http.StatusOK || len(moved.requests()) != 1 {
		t.Errorf("got %d %s, want the request sent again to the cluster under its new CA", code, body)
	}
}

// It still opens, with the fail-closed result and its warning.
func TestAKubeconfigThatCantBeParsedIsNotRelayed(t *testing.T) {
	t.Parallel()
	body := strings.Replace(twoContexts, "{token: training-token}", "{exec: {command: get-token}}", 1)
	p := &kubeconfigPlatform{clusterState: "active", body: body}
	meta := p.meta(t, time.Minute)
	resp, got := openTestEphemeral(t, &clusterKubeconfigEphemeral{meta: meta}, nil)
	if resp.Diagnostics.HasError() || resp.Diagnostics.WarningsCount() != 1 {
		t.Fatalf("diagnostics: %v, want the open to succeed with the parse's warning", resp.Diagnostics)
	}
	meta.relays.mu.Lock()
	defer meta.relays.mu.Unlock()
	if !got.Host.IsNull() || got.Raw.ValueString() != body || len(meta.relays.open) != 0 {
		t.Errorf("host %v, raw changed %t, %d relays: want a null host, the platform's raw and no relay",
			got.Host, got.Raw.ValueString() != body, len(meta.relays.open))
	}
}

func TestTheRelayFetchesWithTheOpensTimeLimit(t *testing.T) {
	t.Parallel()
	clusterCA, _, err := relayCertificates()
	if err != nil {
		t.Fatal(err)
	}
	p := &kubeconfigPlatform{clusterState: "active", body: strings.Replace(twoContexts, b64(testCAPEM), b64(clusterCA), 1)}
	meta := p.meta(t, 7*time.Minute)
	e := &clusterKubeconfigEphemeral{meta: meta}
	resp, _ := openTestEphemeral(t, e, nil)
	t.Cleanup(func() {
		e.Close(context.Background(), ephemeral.CloseRequest{Private: resp.Private}, &ephemeral.CloseResponse{})
	})
	if resp.Diagnostics.HasError() {
		t.Fatalf("diagnostics: %v", resp.Diagnostics)
	}
	meta.relays.mu.Lock()
	defer meta.relays.mu.Unlock()
	if len(meta.relays.open) != 1 {
		t.Fatalf("%d relays open, want 1", len(meta.relays.open))
	}
	for _, r := range meta.relays.open {
		if r.fetchTimeout != 7*time.Minute {
			t.Errorf("the relay fetches with a %v limit, want the open's 7m", r.fetchTimeout)
		}
	}
}

// kubectl needs it, though this provider takes a lone context without one.
func TestTheRelayKubeconfigNamesItsCurrentContext(t *testing.T) {
	t.Parallel()
	single := `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://c.invalid:6443"}}]
contexts: [{name: only, context: {cluster: c, user: u}}]
users: [{name: u, user: {token: platform-token}}]
`
	for name, tc := range map[string]struct{ body, context string }{
		"a current-context":                  {twoContexts, "training"},
		"one context and no current-context": {single, "only"},
	} {
		raw, err := relayKubeconfig(currentKubeconfigNames(tc.body), "https://127.0.0.1:1", testCAPEM, "secret")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var file kubeconfigFile
		if err := yaml.Unmarshal([]byte(raw), &file); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if file.CurrentContext != tc.context || len(file.Contexts) != 1 || file.Contexts[0].Name != tc.context {
			t.Errorf("%s: current-context %q and contexts %+v, want %q current", name, file.CurrentContext, file.Contexts, tc.context)
		}
	}
}
