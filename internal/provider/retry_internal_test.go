package provider

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/k0rdent/terraform-provider-k0rdent/internal/k0rdent"
)

type reply struct {
	status     int
	retryAfter string
}

// scriptedServer answers each request with the next reply, repeating the last
// one, and records every request body it was sent.
type scriptedServer struct {
	*httptest.Server
	mu      sync.Mutex
	replies []reply
	bodies  []string
}

func newScriptedServer(t *testing.T, replies ...reply) *scriptedServer {
	t.Helper()
	s := &scriptedServer{replies: replies}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		s.mu.Lock()
		s.bodies = append(s.bodies, string(raw))
		next := s.replies[0]
		if len(s.replies) > 1 {
			s.replies = s.replies[1:]
		}
		s.mu.Unlock()
		if next.retryAfter != "" {
			w.Header().Set("Retry-After", next.retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(next.status)
		if _, err := io.WriteString(w, `{"error":{"code":429,"message":"rate_limit_exceeded","requestId":"req-1"}}`); err != nil {
			t.Errorf("writing reply: %v", err)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *scriptedServer) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

// sleeps records every wait instead of taking it.
type sleeps struct{ waits []time.Duration }

func (s *sleeps) sleep(ctx context.Context, d time.Duration) error {
	s.waits = append(s.waits, d)
	return ctx.Err()
}

// fastTransport takes the largest delay the jitter allows, so the schedule is
// deterministic, and records waits instead of sleeping.
func fastTransport(tt *testing.T) (rateLimitTransport, *sleeps) {
	s := &sleeps{}
	t := newRateLimitTransport(testTransport(tt))
	t.sleep = s.sleep
	t.jitter = func(d time.Duration) time.Duration { return d }
	return t, s
}

func send(t *testing.T, ctx context.Context, rt http.RoundTripper, method, url, body string) (*http.Response, error) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, r)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := rt.RoundTrip(req)
	if resp != nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}
	return resp, err
}

// A 429 from the gateway's per-address limit carries no Retry-After, and the
// contract says to back off on our own schedule rather than give up.
func TestRateLimitBacksOffFromAHeaderless429(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 429}, reply{status: 429}, reply{status: 200})
	rt, s := fastTransport(t)

	resp, err := send(t, context.Background(), rt, http.MethodGet, srv.URL, "")
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
	if srv.calls() != 3 {
		t.Errorf("%d calls, want 3", srv.calls())
	}
	if len(s.waits) != 2 || s.waits[0] != time.Second || s.waits[1] != 2*time.Second {
		t.Errorf("waits %v, want [1s 2s]", s.waits)
	}
}

// The schedule doubles, caps each wait, and stops after a bounded number of
// attempts with the platform's own 429.
func TestRateLimitBackoffIsCappedAndBounded(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 429})
	rt, s := fastTransport(t)

	resp, err := send(t, context.Background(), rt, http.MethodGet, srv.URL, "")
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status %d, want the 429 surfaced", resp.StatusCode)
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
	if len(s.waits) != len(want) {
		t.Fatalf("waits %v, want %v", s.waits, want)
	}
	for i := range want {
		if s.waits[i] != want[i] {
			t.Errorf("wait %d is %v, want %v", i, s.waits[i], want[i])
		}
	}
	if srv.calls() != len(want)+1 {
		t.Errorf("%d calls, want %d", srv.calls(), len(want)+1)
	}
}

// Full jitter: parallel resources throttled together must not come back
// together. The draw comes from a seeded source, so the test answers the same
// every run, and covers both ends of the range.
func TestRateLimitJitterSpreadsTheWait(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	draw := func(n time.Duration) time.Duration { return time.Duration(r.Int64N(int64(n))) }
	seen := map[time.Duration]bool{}
	for i := 0; i < 100; i++ {
		d := jitterUpTo(time.Second, draw)
		if d < 0 || d > time.Second {
			t.Fatalf("jitter(1s) = %v, want within [0, 1s]", d)
		}
		seen[d] = true
	}
	if len(seen) < 50 {
		t.Errorf("100 waits took %d distinct values, want them spread", len(seen))
	}
	if d := jitterUpTo(time.Second, func(n time.Duration) time.Duration { return n - 1 }); d != time.Second {
		t.Errorf("the largest draw gave %v, want the whole 1s", d)
	}
	if d := jitterUpTo(0, draw); d != 0 {
		t.Errorf("jitter(0) = %v, want 0", d)
	}
}

// A per-account window is 60 seconds, so a Retry-After past 30 is ordinary and
// has to be honoured in full: coming back early counts against the window.
// fastTransport takes the whole second of spread on top.
func TestRateLimitWaitsAsLongAsThePlatformAsks(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 429, retryAfter: "45"}, reply{status: 200})
	rt, s := fastTransport(t)

	resp, err := send(t, context.Background(), rt, http.MethodGet, srv.URL, "")
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
	if len(s.waits) != 1 || s.waits[0] != 46*time.Second {
		t.Errorf("waits %v, want [46s]", s.waits)
	}
}

// Waiting past the operation's deadline only to fail anyway hides the reason.
// Fail now and say what the platform asked for.
func TestRateLimitFailsFastWhenTheWaitOutlivesTheDeadline(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 429, retryAfter: "45"}, reply{status: 200})
	rt, s := fastTransport(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := send(t, ctx, rt, http.MethodGet, srv.URL, "")
	if err == nil {
		t.Fatal("round trip succeeded, want an error naming the wait")
	}
	for _, want := range []string{"45s", "req-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if len(s.waits) != 0 || srv.calls() != 1 {
		t.Errorf("%d waits and %d calls, want none and 1", len(s.waits), srv.calls())
	}
}

// With no deadline, a server-controlled wait still needs a ceiling: an apply
// asleep for an hour looks the same as one that has crashed.
func TestRateLimitRefusesAnHourLongWait(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 429, retryAfter: "3600"})
	rt, s := fastTransport(t)

	_, err := send(t, context.Background(), rt, http.MethodGet, srv.URL, "")
	if err == nil || !strings.Contains(err.Error(), "1h0m0s") {
		t.Errorf("error %v, want one naming the 1h0m0s wait", err)
	}
	if len(s.waits) != 0 {
		t.Errorf("waited %v, want no wait", s.waits)
	}
}

// consumingTransport reads the body itself before sending, as any wrapping
// transport may, so net/http's own rewind cannot hide a missing replay.
type consumingTransport struct{ base http.RoundTripper }

func (c consumingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body io.Reader
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = strings.NewReader(string(raw))
	}
	out, err := http.NewRequestWithContext(req.Context(), req.Method, req.URL.String(), body)
	if err != nil {
		return nil, err
	}
	return c.base.RoundTrip(out)
}

// A 429 means the request was not acted on, so a POST is replayed, and it
// has to carry its body again.
func TestRateLimitReplaysThePostBody(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 429}, reply{status: 429, retryAfter: "1"}, reply{status: 201})
	rt, _ := fastTransport(t)
	rt.base = consumingTransport{base: testTransport(t)}

	resp, err := send(t, context.Background(), rt, http.MethodPost, srv.URL, `{"id":"c1"}`)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status %d, want 201", resp.StatusCode)
	}
	if srv.calls() != 3 {
		t.Errorf("%d calls, want 3", srv.calls())
	}
	for i, b := range srv.bodies {
		if b != `{"id":"c1"}` {
			t.Errorf("attempt %d sent body %q", i, b)
		}
	}
}

// Every other status on a POST the contract does not call safe to resend is an
// answer, not a deferral. A revoked credential answers 401 like an expired
// one, a 409 means something real, a 5xx on a POST may have been acted on,
// and the contract's generic 503 is a missing dependency rather than a pause.
func TestRateLimitRetriesNothingButATooManyRequests(t *testing.T) {
	for _, status := range []int{401, 409, 500, 502, 503, 504} {
		srv := newScriptedServer(t, reply{status: status, retryAfter: "1"}, reply{status: 200})
		rt, _ := fastTransport(t)
		resp, err := send(t, context.Background(), rt, http.MethodPost, srv.URL, `{}`)
		if err != nil {
			t.Fatalf("round trip: %v", err)
		}
		if resp.StatusCode != status || srv.calls() != 1 {
			t.Errorf("status %d: got %d after %d calls, want it surfaced after 1", status, resp.StatusCode, srv.calls())
		}
	}
}

// Ctrl-C during a throttled apply has to be honoured.
func TestRateLimitStopsWaitingWhenTheRunIsCancelled(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 429}, reply{status: 200})
	rt := newRateLimitTransport(testTransport(t))
	rt.jitter = func(time.Duration) time.Duration { return time.Hour }
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)

	start := time.Now()
	_, err := send(t, ctx, rt, http.MethodGet, srv.URL, "")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error %v, want context.Canceled", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %v to notice the cancel", time.Since(start))
	}
	if srv.calls() != 1 {
		t.Errorf("%d calls, want 1", srv.calls())
	}
}

// Both forms the specification allows, and a date already past meaning now
// rather than an error: a second of clock skew against the gateway is ordinary.
func TestRetryAfterReadsBothFormsAndToleratesSkew(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		header string
		want   time.Duration
		ok     bool
	}{
		{"5", 5 * time.Second, true},
		{"0", 0, true},
		{"-1", 0, false},
		{"soon", 0, false},
		{"", 0, false},
		{now.Add(30 * time.Second).Format(http.TimeFormat), 30 * time.Second, true},
		{now.Add(-30 * time.Second).Format(http.TimeFormat), 0, true},
	}
	for _, c := range cases {
		h := http.Header{}
		if c.header != "" {
			h.Set("Retry-After", c.header)
		}
		got, ok := retryAfter(h, now)
		if ok != c.ok || got != c.want {
			t.Errorf("Retry-After %q: got %v %v, want %v %v", c.header, got, ok, c.want, c.ok)
		}
	}
}

// A Retry-After too large to represent is refused, not wrapped into a negative
// wait that retries at once.
func TestAnUnrepresentableRetryAfterIsRefused(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 429, retryAfter: "10000000000"}, reply{status: 200})
	rt, _ := fastTransport(t)
	if _, err := send(t, context.Background(), rt, http.MethodGet, srv.URL, ""); err == nil {
		t.Fatal("a Retry-After of 10000000000 seconds was not refused")
	}
	if got := srv.calls(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

// A backoff that would end past the deadline hands back the platform's 429,
// so the diagnostic carries its code, message and request id rather than a
// bare deadline error.
func TestABackoffPastTheDeadlineReturnsThe429(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 429})
	rt := newRateLimitTransport(testTransport(t))
	rt.jitter = func(d time.Duration) time.Duration { return d }
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	resp, err := send(t, ctx, rt, http.MethodGet, srv.URL, "")
	if err != nil {
		t.Fatalf("err = %v, want the 429 response", err)
	}
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("status = %d, want 429", resp.StatusCode)
	}
}

const peeringsPath = "/v1/regions/us-texas-1/projects/demo/networking/vpcs/vpc-a/peerings"

// sendAs sends a request the way the provider does, through the generated
// client, so it carries the operation id the transport decides a 503 by.
func sendAs(t *testing.T, ctx context.Context, rt http.RoundTripper, server string,
	call func(context.Context, *k0rdent.Client) (*http.Response, error)) (*http.Response, error) {
	t.Helper()
	c, err := k0rdent.NewClient(server, k0rdent.WithHTTPClient(&http.Client{Transport: rt}))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := call(ctx, c)
	if resp != nil {
		t.Cleanup(func() { _ = resp.Body.Close() })
	}
	return resp, err
}

// peeringCreate sends createVPCPeering with body to peeringsPath.
func peeringCreate(body string) func(context.Context, *k0rdent.Client) (*http.Response, error) {
	return func(ctx context.Context, c *k0rdent.Client) (*http.Response, error) {
		return c.CreateVPCPeeringWithBody(ctx, "us-texas-1", "demo", "vpc-a", "application/json", strings.NewReader(body))
	}
}

// createVPCPeering answers 503 when it loses the VPC's row lock, and the
// contract says nothing was written and the create is safe to send again. It
// carries no Retry-After, so the wait is the transport's own backoff, and the
// body goes out again unchanged.
func TestARetrySafe503OnACreateIsWaitedOut(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 503}, reply{status: 201})
	rt, s := fastTransport(t)
	rt.base = consumingTransport{base: testTransport(t)}

	resp, err := sendAs(t, context.Background(), rt, srv.URL, peeringCreate(`{"id":"p1"}`))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("status %d, want 201", resp.StatusCode)
	}
	if len(s.waits) != 1 || s.waits[0] != time.Second {
		t.Errorf("waits %v, want [1s]", s.waits)
	}
	if srv.calls() != 2 || srv.bodies[1] != `{"id":"p1"}` {
		t.Errorf("%d calls with bodies %q, want the body sent twice", srv.calls(), srv.bodies)
	}
}

// A 503 that names a wait, as the contract's batchCreate lock timeout does,
// is waited out as asked, spread as on a 429.
func TestARetrySafe503HonoursRetryAfter(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 503, retryAfter: "7"}, reply{status: 200})
	rt, s := fastTransport(t)

	resp, err := sendAs(t, context.Background(), rt, srv.URL,
		func(ctx context.Context, c *k0rdent.Client) (*http.Response, error) {
			return c.CreateFileStoreWithBody(ctx, "us-texas-1", "demo", "application/json", strings.NewReader(`{}`))
		})
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if resp.StatusCode != http.StatusOK || len(s.waits) != 1 || s.waits[0] != 8*time.Second {
		t.Errorf("status %d after waits %v, want 200 after [8s]", resp.StatusCode, s.waits)
	}
}

// The 503 wait has the same ceiling as the 429 one: a Retry-After past the
// deadline fails at once and names the wait, the status and the request id.
func TestA503WaitPastTheDeadlineFailsAndSaysWhy(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 503, retryAfter: "45"}, reply{status: 201})
	rt, s := fastTransport(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := sendAs(t, ctx, rt, srv.URL, peeringCreate(`{}`))
	if err == nil {
		t.Fatal("round trip succeeded, want an error naming the wait")
	}
	for _, want := range []string{"service unavailable", "45s", "req-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if len(s.waits) != 0 || srv.calls() != 1 {
		t.Errorf("%d waits and %d calls, want none and 1", len(s.waits), srv.calls())
	}
}

// A lock that never frees stops after the same bounded schedule as a 429,
// and the platform's own 503 is what the run reports.
func TestARetrySafe503IsBounded(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 503})
	rt, s := fastTransport(t)

	resp, err := sendAs(t, context.Background(), rt, srv.URL, peeringCreate(`{}`))
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status %d, want the 503 surfaced", resp.StatusCode)
	}
	var total time.Duration
	for _, w := range s.waits {
		total += w
	}
	if len(s.waits) != 6 || total != 61*time.Second {
		t.Errorf("waits %v (total %v), want 6 adding to 61s", s.waits, total)
	}
}

// Which 503s are waited out: any on a method that can be sent twice, and on a
// POST or PATCH only the operations the contract calls safe to resend.
func TestWhich503sAreRetried(t *testing.T) {
	cases := []struct {
		operation string
		want      bool
	}{
		{"createVPCPeering", true},
		{"getClusterKubeconfig", true},
		{"deleteCluster", true},
		// v1.3.0-dev.4: the project or quota check couldn't be made, and nothing was written.
		{"createFileStore", true},
		{"updateFileStore", true},
		// v1.3.0-dev.4: a storage raise couldn't be checked against the quota.
		{"updateCluster", true},

		{"createCluster", false},
		{"scaleCluster", false},
		// The instance group resource waits this one out itself.
		{"scaleInstanceGroup", false},
		{"mintToken", false},
		{"updateNetwork", false},
	}
	for _, c := range cases {
		op, ok := k0rdent.Operations[c.operation]
		if !ok {
			t.Errorf("the client has no operation %s", c.operation)
			continue
		}
		what, got := retryable(op.Method, c.operation, http.StatusServiceUnavailable)
		if got != c.want {
			t.Errorf("503 on %s (%s): retried %v, want %v", c.operation, op.Method, got, c.want)
		}
		if got && what != whyUnavailable {
			t.Errorf("503 on %s logged as %q, want %q", c.operation, what, whyUnavailable)
		}
	}
	// A POST the generated client didn't make names no operation.
	if _, got := retryable(http.MethodPost, "", http.StatusServiceUnavailable); got {
		t.Error("503 on a POST with no operation id was retried")
	}
	if what, ok := retryable(http.MethodPost, "mintToken", http.StatusTooManyRequests); !ok || what != whyRateLimited {
		t.Errorf("429 on the token mint: %q %v, want %q true", what, ok, whyRateLimited)
	}
}

// The gateway's per-address limit on the token mint answers 429 with no
// Retry-After. The mint is a POST with the credential in its body, so the
// retry has to back off on its own schedule and send the same body again.
func TestAHeaderless429OnTheTokenMintBacksOffAndRetries(t *testing.T) {
	const mint = `{"grant_type":"client_credentials","client_id":"sa@p.iam","client_secret":"s"}`
	srv := newScriptedServer(t, reply{status: 429}, reply{status: 429}, reply{status: 429}, reply{status: 200})
	rt, s := fastTransport(t)
	rt.base = consumingTransport{base: testTransport(t)}

	resp, err := send(t, context.Background(), rt, http.MethodPost, srv.URL+"/v1/regions/global/auth/token", mint)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}
	if len(s.waits) != len(want) {
		t.Fatalf("waits %v, want %v", s.waits, want)
	}
	for i := range want {
		if s.waits[i] != want[i] {
			t.Errorf("wait %d is %v, want %v", i, s.waits[i], want[i])
		}
	}
	for i, b := range srv.bodies {
		if b != mint {
			t.Errorf("attempt %d sent body %q, want the mint body", i, b)
		}
	}
}

// closeTracking answers every request with one canned response and records
// whether its body was closed.
type closeTracking struct {
	status     int
	retryAfter string
	closed     bool
}

func (c *closeTracking) RoundTrip(req *http.Request) (*http.Response, error) {
	h := http.Header{}
	if c.retryAfter != "" {
		h.Set("Retry-After", c.retryAfter)
	}
	return &http.Response{
		StatusCode: c.status,
		Status:     http.StatusText(c.status),
		Header:     h,
		Body:       &trackedBody{Reader: strings.NewReader(`{"error":{"code":429,"message":"m","requestId":"req-9"}}`), closed: &c.closed},
		Request:    req,
	}, nil
}

type trackedBody struct {
	io.Reader
	closed *bool
}

func (b *trackedBody) Close() error { *b.closed = true; return nil }

// A refused wait hands back no response for the caller to close, so the
// transport has to close it, or the connection under it is never reused.
func TestARefusedWaitClosesTheResponse(t *testing.T) {
	base := &closeTracking{status: http.StatusTooManyRequests, retryAfter: "3600"}
	rt, _ := fastTransport(t)
	rt.base = base

	resp, err := send(t, context.Background(), rt, http.MethodGet, "https://api.example/v1/regions", "")
	if err == nil || resp != nil {
		t.Fatalf("got %v, %v, want no response and an error", resp, err)
	}
	if !base.closed {
		t.Error("the refused response's body was left open")
	}
}

// Retry-After: 0, or a date already past, names no wait at all. Every apply
// that read it would come back at the same instant, so it's treated as no
// header: the waits are the transport's own backoff schedule, each drawn
// through the jitter. A wait of nothing taken as a named one would instead
// get the second of spread each time, and come back as [1s 1s].
func TestARetryAfterOfZeroIsSpreadLikeNoHeader(t *testing.T) {
	past := time.Now().Add(-time.Minute).UTC().Format(http.TimeFormat)
	for _, header := range []string{"0", past} {
		srv := newScriptedServer(t, reply{status: 429, retryAfter: header}, reply{status: 429, retryAfter: header}, reply{status: 200})
		rt, s := fastTransport(t)
		var drawn []time.Duration
		rt.jitter = func(d time.Duration) time.Duration {
			drawn = append(drawn, d)
			return d
		}
		if _, err := send(t, context.Background(), rt, http.MethodGet, srv.URL, ""); err != nil {
			t.Fatalf("Retry-After %q: %v", header, err)
		}
		want := []time.Duration{time.Second, 2 * time.Second}
		if !slices.Equal(s.waits, want) {
			t.Errorf("Retry-After %q: waits %v, want %v", header, s.waits, want)
		}
		if !slices.Equal(drawn, want) {
			t.Errorf("Retry-After %q: jittered %v, want the backoff %v", header, drawn, want)
		}
	}
}

// A Retry-After the platform does name is honoured in full and never cut
// short, and up to a backoffBase is added on top so that every apply told the
// same number doesn't come back in the same instant. The two ends of the
// jitter give the two ends of the wait.
func TestANamedRetryAfterIsNeverShortenedAndSpreadALittle(t *testing.T) {
	for _, c := range []struct {
		jitter func(time.Duration) time.Duration
		want   time.Duration
	}{
		{func(time.Duration) time.Duration { return 0 }, 5 * time.Second},
		{func(d time.Duration) time.Duration { return d }, 6 * time.Second},
	} {
		srv := newScriptedServer(t, reply{status: 429, retryAfter: "5"}, reply{status: 200})
		rt, s := fastTransport(t)
		var drawn []time.Duration
		rt.jitter = func(d time.Duration) time.Duration {
			drawn = append(drawn, d)
			return c.jitter(d)
		}
		if _, err := send(t, context.Background(), rt, http.MethodGet, srv.URL, ""); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(s.waits, []time.Duration{c.want}) {
			t.Errorf("waits %v, want [%v]", s.waits, c.want)
		}
		if !slices.Equal(drawn, []time.Duration{time.Second}) {
			t.Errorf("jittered %v, want one draw over the 1s spread", drawn)
		}
	}
}

// The spread never pushes a wait that fits the deadline past it: the
// platform's own wait is what the deadline is checked against.
func TestTheSpreadNeverOutlivesTheDeadline(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 429, retryAfter: "9"}, reply{status: 200})
	rt, s := fastTransport(t)
	ctx, cancel := context.WithTimeout(context.Background(), 9500*time.Millisecond)
	defer cancel()

	if _, err := send(t, ctx, rt, http.MethodGet, srv.URL, ""); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if len(s.waits) != 1 || s.waits[0] != 9*time.Second {
		t.Errorf("waits %v, want exactly the 9s asked for", s.waits)
	}
}

// A 502 or 504 is the gateway saying the service behind it didn't answer in
// time or at all. A request that can be sent twice is sent again, the way a
// 503 on one is. On a create the service may already have acted, so it's
// reported.
func TestAGatewayFailureIsRetriedOnlyWhereResendingIsSafe(t *testing.T) {
	for _, status := range []int{http.StatusBadGateway, http.StatusGatewayTimeout} {
		for _, c := range []struct {
			method string
			want   int
		}{
			{http.MethodGet, 2},
			{http.MethodDelete, 2},
			{http.MethodPut, 2},
			{http.MethodPost, 1},
			{http.MethodPatch, 1},
		} {
			srv := newScriptedServer(t, reply{status: status}, reply{status: 200})
			rt, _ := fastTransport(t)
			if _, err := send(t, context.Background(), rt, c.method, srv.URL+peeringsPath, ""); err != nil {
				t.Fatalf("%d on %s: %v", status, c.method, err)
			}
			if srv.calls() != c.want {
				t.Errorf("%d on %s: %d calls, want %d", status, c.method, srv.calls(), c.want)
			}
		}
	}
}

// dropFirst closes the connection without answering the first n requests,
// the way a gateway or load balancer does when a backend restarts under it.
func dropFirst(t *testing.T, n int) (*httptest.Server, func() int) {
	t.Helper()
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		drop := calls <= n
		mu.Unlock()
		if drop {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("the test server can't hijack a connection")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			_ = conn.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, func() int { mu.Lock(); defer mu.Unlock(); return calls }
}

// A connection dropped before any answer says nothing about whether the
// request was acted on. A read is sent again after a backoff. A create isn't.
func TestADroppedConnectionIsRetriedOnlyWhereResendingIsSafe(t *testing.T) {
	srv, calls := dropFirst(t, 2)
	rt, s := fastTransport(t)
	resp, err := send(t, context.Background(), rt, http.MethodGet, srv.URL, "")
	if err != nil {
		t.Fatalf("GET after two dropped connections: %v", err)
	}
	if resp.StatusCode != http.StatusOK || calls() != 3 {
		t.Errorf("status %d after %d calls, want 200 after 3", resp.StatusCode, calls())
	}
	if len(s.waits) != 2 || s.waits[0] != time.Second || s.waits[1] != 2*time.Second {
		t.Errorf("waits %v, want [1s 2s]", s.waits)
	}

	srv, calls = dropFirst(t, 1)
	rt, _ = fastTransport(t)
	if _, err := send(t, context.Background(), rt, http.MethodPost, srv.URL, `{}`); err == nil {
		t.Error("a POST whose connection dropped was retried into a success, want the error")
	}
	if calls() != 1 {
		t.Errorf("POST: %d calls, want 1", calls())
	}
}

// An address nothing listens on is a wrong endpoint, not a blip, and a run
// shouldn't spend a minute of backoff before saying so.
func TestARefusedConnectionIsNotRetried(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	rt, s := fastTransport(t)
	if _, err := send(t, context.Background(), rt, http.MethodGet, url, ""); err == nil {
		t.Fatal("a GET to a closed port succeeded")
	}
	if len(s.waits) != 0 {
		t.Errorf("waited %v before reporting a refused connection, want no wait", s.waits)
	}
}

// Ctrl-C during a wait ends the run with the cancellation, and the error still
// says what the platform had answered, so the request id isn't lost.
func TestACancelledWaitKeepsThePlatformsAnswer(t *testing.T) {
	srv := newScriptedServer(t, reply{status: 429}, reply{status: 200})
	rt, _ := fastTransport(t)
	rt.sleep = func(context.Context, time.Duration) error { return context.Canceled }

	_, err := send(t, context.Background(), rt, http.MethodGet, srv.URL, "")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want context.Canceled", err)
	}
	for _, want := range []string{"429", "req-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// RetriedStatuses is what the documentation is held to, so it has to be what
// retryable retries, method by method.
func TestRetriedStatusesAreWhatRetryableRetries(t *testing.T) {
	want := []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout}
	if got := RetriedStatuses(); !slices.Equal(got, want) {
		t.Errorf("RetriedStatuses() = %v, want %v", got, want)
	}
}
