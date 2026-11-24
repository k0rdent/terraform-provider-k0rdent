package k0rdent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeAPI stands in for the gateway: it mints tokens and serves one read.
type fakeAPI struct {
	mints     atomic.Int64
	reads     atomic.Int64
	mintBody  atomic.Value // last mint body, as map[string]any
	authSeen  atomic.Value // last Authorization header on a read
	lifetime  int          // expires_in to report
	mintDelay time.Duration
	readSays  int // status for the read; 0 means 200
}

func (f *fakeAPI) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/regions/global/auth/token", func(w http.ResponseWriter, r *http.Request) {
		n := f.mints.Add(1)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body["_contentType"] = r.Header.Get("Content-Type")
		body["_authorization"] = r.Header.Get("Authorization")
		f.mintBody.Store(body)
		if f.mintDelay > 0 {
			time.Sleep(f.mintDelay)
		}
		lifetime := f.lifetime
		if lifetime == 0 {
			lifetime = 900
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "token-" + string(rune('0'+n)),
			"token_type":   "Bearer",
			"expires_in":   lifetime,
		})
	})
	mux.HandleFunc("GET /v1/regions/{region}/compute/cluster-types", func(w http.ResponseWriter, r *http.Request) {
		f.reads.Add(1)
		f.authSeen.Store(r.Header.Get("Authorization"))
		if f.readSays != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(f.readSays)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": f.readSays, "message": "denied"},
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"clusterTypes": []any{}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func testClient(t *testing.T, srv *httptest.Server, opts ...Option) *ClientWithResponses {
	t.Helper()
	all := append([]Option{
		WithCredential(NewServiceAccountCredential("sa-tf@vendor.iam", "k0r_secret", nil)),
	}, opts...)
	c, err := New(srv.URL, all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

// The mint is a JSON body with a grant_type discriminator, not RFC 6749
// form-encoded client credentials. Getting this wrong is the obvious reflex and
// produces a 400 that reads like a bad secret.
func TestMint_IsJSONWithGrantTypeDiscriminator(t *testing.T) {
	f := &fakeAPI{}
	srv := f.server(t)
	c := testClient(t, srv)

	if _, err := c.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil); err != nil {
		t.Fatalf("read: %v", err)
	}

	body, _ := f.mintBody.Load().(map[string]any)
	if body == nil {
		t.Fatal("no mint request was made")
	}
	if ct, _ := body["_contentType"].(string); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("mint Content-Type = %q, want application/json", ct)
	}
	if got := body["grant_type"]; got != "client_credentials" {
		t.Errorf("grant_type = %v, want client_credentials", got)
	}
	if got := body["client_id"]; got != "sa-tf@vendor.iam" {
		t.Errorf("client_id = %v", got)
	}
	if got := body["client_secret"]; got != "k0r_secret" {
		t.Errorf("client_secret = %v", got)
	}
	// The token endpoint is anonymous; sending a bearer to it is at best noise.
	if got, _ := body["_authorization"].(string); got != "" {
		t.Errorf("mint carried Authorization %q, want none", got)
	}
	if got, _ := f.authSeen.Load().(string); got != "Bearer token-1" {
		t.Errorf("read Authorization = %q, want the minted token", got)
	}
}

// The typed MintToken sends the grant. MintTokenJSONRequestBody used to be a
// defined type over the union, which drops the union's MarshalJSON, so the
// request went out as `{}` and the server answered 400.
func TestMintToken_TypedCallSendsTheGrant(t *testing.T) {
	f := &fakeAPI{}
	srv := f.server(t)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var grant MintTokenJSONBody
	if err := grant.FromClientCredentialsGrant(ClientCredentialsGrant{
		GrantType:    ClientCredentialsGrantGrantTypeClientCredentials,
		ClientId:     "id",
		ClientSecret: "secret",
	}); err != nil {
		t.Fatalf("FromClientCredentialsGrant: %v", err)
	}

	resp, err := c.MintTokenWithResponse(context.Background(), MintTokenJSONRequestBody(grant))
	if err != nil {
		t.Fatalf("MintTokenWithResponse: %v", err)
	}
	if resp.JSON200 == nil {
		t.Fatalf("mint answered %s with no token", resp.Status())
	}
	sent, _ := f.mintBody.Load().(map[string]any)
	if sent["grant_type"] != "client_credentials" || sent["client_id"] != "id" || sent["client_secret"] != "secret" {
		t.Errorf("the mint received %v, want the client_credentials grant", sent)
	}
}

func TestMint_APIKeyGrantShape(t *testing.T) {
	f := &fakeAPI{}
	srv := f.server(t)
	c, err := New(srv.URL, WithCredential(NewAPIKeyCredential("k0r_abc", nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil); err != nil {
		t.Fatalf("read: %v", err)
	}
	body, _ := f.mintBody.Load().(map[string]any)
	if got := body["grant_type"]; got != "api_key" {
		t.Errorf("grant_type = %v, want api_key", got)
	}
	if got := body["api_key"]; got != "k0r_abc" {
		t.Errorf("api_key = %v", got)
	}
}

// The reason the cache exists. The auth service allows 10 mints per 60s per
// credential and Terraform's default parallelism is 10, so ten concurrent
// operations must cost one mint, not ten.
func TestTokenCache_ConcurrentCallersMintOnce(t *testing.T) {
	f := &fakeAPI{mintDelay: 20 * time.Millisecond}
	srv := f.server(t)
	c := testClient(t, srv)

	const parallelism = 10 // Terraform's default
	var wg sync.WaitGroup
	errs := make(chan error, parallelism)
	for range parallelism {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("read: %v", err)
	}

	if got := f.mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1 for %d concurrent calls", got, parallelism)
	}
	if got := f.reads.Load(); got != parallelism {
		t.Errorf("reads = %d, want %d", got, parallelism)
	}
}

func TestTokenCache_ConcurrentCallersShareAFailedMint(t *testing.T) {
	var mints atomic.Int64
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	mintErr := errors.New("rate limited")
	var failing atomic.Bool
	failing.Store(true)
	cache := newTokenCache(
		NewServiceAccountCredential("id", "secret", nil), DefaultRefreshSkew,
		func(context.Context, MintTokenJSONBody) (*TokenResponse, error) {
			if mints.Add(1) == 1 {
				close(firstStarted)
				<-releaseFirst
			}
			if failing.Load() {
				return nil, mintErr
			}
			return &TokenResponse{AccessToken: "fresh-token", ExpiresIn: 900}, nil
		})

	ownerResult := make(chan error, 1)
	go func() {
		_, err := cache.get(context.Background())
		ownerResult <- err
	}()
	<-firstStarted

	waiterJoined := make(chan struct{}, 2)
	waiterResults := make(chan error, 2)
	for range 2 {
		waiterCtx := &observedContext{Context: context.Background(), doneCalled: waiterJoined}
		go func() {
			_, err := cache.get(waiterCtx)
			waiterResults <- err
		}()
	}
	for range 2 {
		awaitWaiterSelect(t, waiterJoined)
	}
	close(releaseFirst)

	if err := <-ownerResult; !errors.Is(err, mintErr) {
		t.Errorf("owner error = %v, want %v", err, mintErr)
	}
	for range 2 {
		if err := <-waiterResults; !errors.Is(err, mintErr) {
			t.Errorf("waiter error = %v, want the shared %v", err, mintErr)
		}
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1 shared failure", got)
	}

	failing.Store(false)
	token, err := cache.get(context.Background())
	if err != nil || token != "fresh-token" {
		t.Errorf("next get = %q, %v, want a fresh successful mint", token, err)
	}
	if got := mints.Load(); got != 2 {
		t.Errorf("mints = %d after the next call, want 2", got)
	}
}

type tokenResult struct {
	token string
	err   error
}

func TestTokenCache_MintPanicReleasesWaiters(t *testing.T) {
	var mints atomic.Int64
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	cache := newTokenCache(
		NewServiceAccountCredential("id", "secret", nil), DefaultRefreshSkew,
		func(context.Context, MintTokenJSONBody) (*TokenResponse, error) {
			if mints.Add(1) == 1 {
				close(firstStarted)
				<-releaseFirst
				panic("mint exploded")
			}
			return &TokenResponse{AccessToken: "fresh-token", ExpiresIn: 900}, nil
		})

	ownerPanic := make(chan any, 1)
	go func() {
		defer func() { ownerPanic <- recover() }()
		_, _ = cache.get(context.Background())
	}()
	<-firstStarted

	waiterCtx := &pausedContext{
		Context:     context.Background(),
		done:        make(chan struct{}),
		doneCalled:  make(chan struct{}),
		releaseDone: make(chan struct{}),
	}
	waiterResult := make(chan tokenResult, 1)
	go func() {
		token, err := cache.get(waiterCtx)
		waiterResult <- tokenResult{token: token, err: err}
	}()
	<-waiterCtx.doneCalled
	close(releaseFirst)
	close(waiterCtx.releaseDone)

	select {
	case got := <-ownerPanic:
		if got != "mint exploded" {
			t.Errorf("recovered panic = %v, want mint exploded", got)
		}
	case <-time.After(time.Second):
		t.Fatal("owner did not re-panic")
	}
	select {
	case got := <-waiterResult:
		if !errors.Is(got.err, ErrTokenMint) {
			t.Errorf("waiter get = %q, %v, want an ErrTokenMint for the panic", got.token, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter remained blocked after the attempt owner panicked")
	}

	token, err := cache.get(context.Background())
	if err != nil || token != "fresh-token" {
		t.Errorf("get after panic = %q, %v, want a fresh successful mint", token, err)
	}
	if got := mints.Load(); got != 2 {
		t.Errorf("mints = %d after recovery, want 2", got)
	}
}

func TestTokenCache_LateOwnerCancellationKeepsSharedFailure(t *testing.T) {
	var mints atomic.Int64
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	mintErr := errors.New("rate limited")
	cache := newTokenCache(
		NewServiceAccountCredential("id", "secret", nil), DefaultRefreshSkew,
		func(context.Context, MintTokenJSONBody) (*TokenResponse, error) {
			if mints.Add(1) == 1 {
				close(firstStarted)
				<-releaseFirst
				return nil, mintErr
			}
			return &TokenResponse{AccessToken: "token-2", ExpiresIn: 900}, nil
		})

	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	ownerResult := make(chan error, 1)
	go func() {
		_, err := cache.get(ownerCtx)
		ownerResult <- err
	}()
	<-firstStarted

	waiterCtx := &pausedContext{
		Context:     context.Background(),
		done:        make(chan struct{}),
		doneCalled:  make(chan struct{}),
		releaseDone: make(chan struct{}),
	}
	waiterResult := make(chan tokenResult, 1)
	go func() {
		token, err := cache.get(waiterCtx)
		waiterResult <- tokenResult{token: token, err: err}
	}()
	<-waiterCtx.doneCalled
	close(releaseFirst)
	if err := <-ownerResult; !errors.Is(err, mintErr) {
		t.Fatalf("owner error = %v, want %v", err, mintErr)
	}
	cancelOwner()
	close(waiterCtx.releaseDone)

	got := <-waiterResult
	if got.token != "" || !errors.Is(got.err, mintErr) {
		t.Errorf("waiter get = %q, %v, want the completed attempt's failure", got.token, got.err)
	}
	if got := mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1 completed failure", got)
	}
}

func TestTokenCache_WaiterRefreshesAnExpiredSharedToken(t *testing.T) {
	var mints atomic.Int64
	now := time.Now()
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	cache := newTokenCache(
		NewServiceAccountCredential("id", "secret", nil), 20*time.Second,
		func(context.Context, MintTokenJSONBody) (*TokenResponse, error) {
			n := mints.Add(1)
			if n == 1 {
				close(firstStarted)
				<-releaseFirst
			}
			return &TokenResponse{AccessToken: "token-" + string(rune('0'+n)), ExpiresIn: 40}, nil
		})
	cache.clock = func() time.Time { return now }

	ownerResult := make(chan tokenResult, 1)
	go func() {
		token, err := cache.get(context.Background())
		ownerResult <- tokenResult{token: token, err: err}
	}()
	<-firstStarted

	waiterCtx := &pausedContext{
		Context:     context.Background(),
		done:        make(chan struct{}),
		doneCalled:  make(chan struct{}),
		releaseDone: make(chan struct{}),
	}
	waiterResult := make(chan tokenResult, 1)
	go func() {
		token, err := cache.get(waiterCtx)
		waiterResult <- tokenResult{token: token, err: err}
	}()
	<-waiterCtx.doneCalled
	close(releaseFirst)
	if got := <-ownerResult; got.err != nil || got.token != "token-1" {
		t.Fatalf("owner get = %q, %v, want token-1", got.token, got.err)
	}
	now = now.Add(21 * time.Second)
	close(waiterCtx.releaseDone)

	got := <-waiterResult
	if got.err != nil || got.token != "token-2" {
		t.Errorf("waiter get = %q, %v, want token-2 from a fresh mint", got.token, got.err)
	}
	if got := mints.Load(); got != 2 {
		t.Errorf("mints = %d, want 2 after the first token reached refresh time", got)
	}
}

func awaitFreshMint(t *testing.T, started <-chan struct{}) {
	t.Helper()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a fresh mint")
	}
}

func awaitWaiterSelect(t *testing.T, joined <-chan struct{}) {
	t.Helper()
	select {
	case <-joined:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a waiter to reach its select")
	}
}

func TestTokenCache_CancelledOwnerWaitersShareFreshMint(t *testing.T) {
	var mints atomic.Int64
	firstStarted := make(chan struct{})
	freshStarted := make(chan struct{})
	releaseFresh := make(chan struct{})
	cache := newTokenCache(
		NewServiceAccountCredential("id", "secret", nil), DefaultRefreshSkew,
		func(ctx context.Context, _ MintTokenJSONBody) (*TokenResponse, error) {
			switch mints.Add(1) {
			case 1:
				close(firstStarted)
				<-ctx.Done()
				return nil, ctx.Err()
			case 2:
				close(freshStarted)
				<-releaseFresh
			}
			return &TokenResponse{AccessToken: "fresh-token", ExpiresIn: 900}, nil
		})

	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	ownerResult := make(chan error, 1)
	go func() {
		_, err := cache.get(ownerCtx)
		ownerResult <- err
	}()
	<-firstStarted

	waiterJoined := make(chan struct{}, 1)
	waiterResults := make(chan tokenResult, 2)
	firstWaiterCtx := &observedContext{Context: context.Background(), doneCalled: waiterJoined}
	go func() {
		token, err := cache.get(firstWaiterCtx)
		waiterResults <- tokenResult{token: token, err: err}
	}()
	awaitWaiterSelect(t, waiterJoined)
	cancelOwner()
	if err := <-ownerResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("owner error = %v, want context.Canceled", err)
	}

	awaitFreshMint(t, freshStarted)
	secondWaiterCtx := &observedContext{Context: context.Background(), doneCalled: waiterJoined}
	go func() {
		token, err := cache.get(secondWaiterCtx)
		waiterResults <- tokenResult{token: token, err: err}
	}()
	awaitWaiterSelect(t, waiterJoined)
	close(releaseFresh)
	for range 2 {
		got := <-waiterResults
		if got.err != nil || got.token != "fresh-token" {
			t.Errorf("waiter get = %q, %v, want fresh-token", got.token, got.err)
		}
	}
	if got := mints.Load(); got != 2 {
		t.Errorf("mints = %d, want the cancelled attempt and one shared fresh mint", got)
	}
}

func TestTokenCache_LiveOwnerCancellationIsShared(t *testing.T) {
	var mints atomic.Int64
	firstStarted := make(chan struct{})
	freshStarted := make(chan struct{})
	releaseFresh := make(chan struct{})
	cache := newTokenCache(
		NewServiceAccountCredential("id", "secret", nil), DefaultRefreshSkew,
		func(ctx context.Context, _ MintTokenJSONBody) (*TokenResponse, error) {
			switch mints.Add(1) {
			case 1:
				close(firstStarted)
				<-ctx.Done()
				return nil, ctx.Err()
			case 2:
				close(freshStarted)
				<-releaseFresh
			}
			return nil, context.Canceled
		})

	ownerCtx, cancelOwner := context.WithCancel(context.Background())
	ownerResult := make(chan error, 1)
	go func() {
		_, err := cache.get(ownerCtx)
		ownerResult <- err
	}()
	<-firstStarted

	waiterJoined := make(chan struct{}, 1)
	waiterResults := make(chan error, 2)
	firstWaiterCtx := &observedContext{Context: context.Background(), doneCalled: waiterJoined}
	go func() {
		_, err := cache.get(firstWaiterCtx)
		waiterResults <- err
	}()
	awaitWaiterSelect(t, waiterJoined)
	cancelOwner()
	if err := <-ownerResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("owner error = %v, want context.Canceled", err)
	}

	awaitFreshMint(t, freshStarted)
	secondWaiterCtx := &observedContext{Context: context.Background(), doneCalled: waiterJoined}
	go func() {
		_, err := cache.get(secondWaiterCtx)
		waiterResults <- err
	}()
	awaitWaiterSelect(t, waiterJoined)
	close(releaseFresh)
	for range 2 {
		if err := <-waiterResults; !errors.Is(err, context.Canceled) {
			t.Errorf("waiter error = %v, want the shared context.Canceled", err)
		}
	}
	if got := mints.Load(); got != 2 {
		t.Errorf("mints = %d, want the cancelled attempt and one shared fresh failure", got)
	}
}

type observedContext struct {
	context.Context
	doneCalled chan<- struct{}
}

func (c *observedContext) Done() <-chan struct{} {
	c.doneCalled <- struct{}{}
	return c.Context.Done()
}

type pausedContext struct {
	context.Context
	done        chan struct{}
	doneCalled  chan struct{}
	releaseDone chan struct{}
}

func (c *pausedContext) Done() <-chan struct{} {
	close(c.doneCalled)
	<-c.releaseDone
	return c.done
}

func (c *pausedContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func TestTokenCache_CancelledWaiterGetsItsOwnError(t *testing.T) {
	const iterations = 100
	leaderErr := errors.New("leader failed")
	wrong := 0
	for range iterations {
		firstStarted := make(chan struct{})
		releaseFirst := make(chan struct{})
		var mints atomic.Int64
		cache := newTokenCache(
			NewServiceAccountCredential("id", "secret", nil), DefaultRefreshSkew,
			func(context.Context, MintTokenJSONBody) (*TokenResponse, error) {
				if mints.Add(1) == 1 {
					close(firstStarted)
					<-releaseFirst
				}
				return nil, leaderErr
			})

		ownerResult := make(chan error, 1)
		go func() {
			_, err := cache.get(context.Background())
			ownerResult <- err
		}()
		<-firstStarted

		waiterCtx := &pausedContext{
			Context:     context.Background(),
			done:        make(chan struct{}),
			doneCalled:  make(chan struct{}),
			releaseDone: make(chan struct{}),
		}
		waiterResult := make(chan error, 1)
		go func() {
			_, err := cache.get(waiterCtx)
			waiterResult <- err
		}()
		<-waiterCtx.doneCalled
		close(releaseFirst)
		if err := <-ownerResult; !errors.Is(err, leaderErr) {
			t.Fatalf("owner error = %v, want %v", err, leaderErr)
		}
		close(waiterCtx.done)
		close(waiterCtx.releaseDone)

		if err := <-waiterResult; err != context.Canceled {
			wrong++
		}
	}
	if wrong != 0 {
		t.Errorf("%d of %d cancelled waiters received the leader's error", wrong, iterations)
	}
}

func TestTokenCache_ReusesTokenAcrossSequentialCalls(t *testing.T) {
	f := &fakeAPI{}
	srv := f.server(t)
	c := testClient(t, srv)
	for range 5 {
		if _, err := c.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if got := f.mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1", got)
	}
}

// A cluster create outlives the 900-second token and there is no refresh token,
// so the credential has to be re-presented mid-run.
func TestTokenCache_RefreshesBeforeExpiry(t *testing.T) {
	cred := NewServiceAccountCredential("id", "secret", nil)
	var mints int
	now := time.Now()
	cache := newTokenCache(cred, DefaultRefreshSkew, func(context.Context, MintTokenJSONBody) (*TokenResponse, error) {
		mints++
		return &TokenResponse{AccessToken: "tok", ExpiresIn: 900, TokenType: TokenResponseTokenTypeBearer}, nil
	})
	cache.clock = func() time.Time { return now }

	if _, err := cache.get(context.Background()); err != nil {
		t.Fatalf("first get: %v", err)
	}
	if mints != 1 {
		t.Fatalf("mints = %d after first get, want 1", mints)
	}

	// Well inside the window: no new mint.
	now = now.Add(600 * time.Second)
	if _, err := cache.get(context.Background()); err != nil {
		t.Fatalf("get inside window: %v", err)
	}
	if mints != 1 {
		t.Errorf("mints = %d at t+600s, want 1", mints)
	}

	// Inside the skew: mint again rather than send a token about to die.
	now = now.Add(argMax(900-600, 0) * time.Second).Add(-30 * time.Second)
	if _, err := cache.get(context.Background()); err != nil {
		t.Fatalf("get inside skew: %v", err)
	}
	if mints != 2 {
		t.Errorf("mints = %d at 30s before expiry, want 2", mints)
	}
}

func argMax(a, b int) time.Duration {
	if a > b {
		return time.Duration(a)
	}
	return time.Duration(b)
}

// expires_in is read from the response, not assumed to be 900: it is stamped on
// the identity-provider client at registration and an operator can change it.
func TestTokenCache_HonoursShortExpiresIn(t *testing.T) {
	var mints int
	now := time.Now()
	cache := newTokenCache(
		NewServiceAccountCredential("id", "secret", nil),
		10*time.Second,
		func(context.Context, MintTokenJSONBody) (*TokenResponse, error) {
			mints++
			return &TokenResponse{AccessToken: "tok", ExpiresIn: 30, TokenType: TokenResponseTokenTypeBearer}, nil
		})
	cache.clock = func() time.Time { return now }

	if _, err := cache.get(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(25 * time.Second) // inside a 10s skew on a 30s token
	if _, err := cache.get(context.Background()); err != nil {
		t.Fatal(err)
	}
	if mints != 2 {
		t.Errorf("mints = %d, want 2 -- a 30s token must not be cached for 900s", mints)
	}
}

// A token that lives no longer than the skew would otherwise be stale the moment
// it is cached, so every request would mint and the 10-per-minute budget would
// run out on the eleventh. The skew shrinks to half the lifetime instead.
func TestTokenCache_TokenShorterThanSkewIsStillCached(t *testing.T) {
	f := &fakeAPI{lifetime: 30}
	srv := f.server(t)
	c := testClient(t, srv) // DefaultRefreshSkew, 60s
	for range 5 {
		if _, err := c.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if got := f.mints.Load(); got != 1 {
		t.Errorf("mints = %d for 5 calls on a 30s token, want 1", got)
	}
}

func TestTokenCache_ClampsSkewToHalfTheLifetime(t *testing.T) {
	var mints int
	now := time.Now()
	cache := newTokenCache(
		NewServiceAccountCredential("id", "secret", nil),
		DefaultRefreshSkew,
		func(context.Context, MintTokenJSONBody) (*TokenResponse, error) {
			mints++
			return &TokenResponse{AccessToken: "tok", ExpiresIn: 30, TokenType: TokenResponseTokenTypeBearer}, nil
		})
	cache.clock = func() time.Time { return now }

	start := now
	for _, step := range []struct {
		at    time.Duration
		mints int
	}{
		{0, 1},
		{14 * time.Second, 1}, // still in the first half of a 30s token
		{15 * time.Second, 2}, // half the lifetime left: replace it
	} {
		now = start.Add(step.at)
		if _, err := cache.get(context.Background()); err != nil {
			t.Fatalf("get at t+%s: %v", step.at, err)
		}
		if mints != step.mints {
			t.Errorf("mints = %d at t+%s, want %d", mints, step.at, step.mints)
		}
	}
}

// expires_in is required and is the token's lifetime in seconds. Zero or less
// means a token that is already dead, or a field the server left out and JSON
// decoded to 0. Caching it would mint on every request, so say so instead.
func TestTokenCache_RefusesANonPositiveExpiresIn(t *testing.T) {
	for _, expiresIn := range []int{0, -1} {
		cache := newTokenCache(
			NewServiceAccountCredential("id", "secret", nil),
			DefaultRefreshSkew,
			func(context.Context, MintTokenJSONBody) (*TokenResponse, error) {
				return &TokenResponse{AccessToken: "tok", ExpiresIn: expiresIn, TokenType: TokenResponseTokenTypeBearer}, nil
			})
		tok, err := cache.get(context.Background())
		if !errors.Is(err, ErrTokenMint) || !strings.Contains(err.Error(), "expires_in") {
			t.Errorf("expires_in %d: get = %q, %v, want an ErrTokenMint naming expires_in", expiresIn, tok, err)
		}
	}
}

// ADR D4: never retry. A 401 mid-apply usually means the credential was
// revoked, and replaying a create that may already have been accepted is how a
// provider builds two clusters.
func TestAuthTransport_DoesNotReplayOn401(t *testing.T) {
	f := &fakeAPI{readSays: http.StatusUnauthorized}
	srv := f.server(t)
	c := testClient(t, srv)

	r, err := c.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := CheckResponse(r.HTTPResponse, r.Body); err == nil {
		t.Fatal("CheckResponse = nil for a 401")
	}
	if got := f.reads.Load(); got != 1 {
		t.Errorf("reads = %d, want 1 -- the request must not be replayed", got)
	}
	if got := f.mints.Load(); got != 1 {
		t.Errorf("mints = %d, want 1 -- a 401 must not trigger a re-mint", got)
	}
}

func TestAuthTransport_MintFailureSurfaces(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/regions/global/auth/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": 429, "message": "rate limit exceeded", "requestId": "req-429"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := New(srv.URL, WithCredential(NewServiceAccountCredential("id", "secret", nil)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = c.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil)
	if err == nil {
		t.Fatal("read succeeded despite a failing mint")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("error %v (%T) does not unwrap to *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests || apiErr.RequestID != "req-429" {
		t.Errorf("got %+v, want the 429 and its requestId", apiErr)
	}
}

// A refused mint and a refused operation can carry the same 401. Only the
// first says the credential itself is bad, so a caller has to be able to
// tell them apart without parsing text.
func TestAuthTransport_MintFailureIsErrTokenMint(t *testing.T) {
	mintAnswers := func(status int, body map[string]any) *httptest.Server {
		mux := http.NewServeMux()
		mux.HandleFunc("POST /v1/regions/global/auth/token", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return srv
	}
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()

	unbuildable := credentialFunc(func() (MintTokenJSONBody, error) {
		return MintTokenJSONBody{}, errors.New("key file unreadable")
	})

	tests := []struct {
		name      string
		srv       *httptest.Server
		cred      Credential
		wantAPI   int
		wantInMsg string
	}{
		{"the mint is refused", mintAnswers(http.StatusUnauthorized, map[string]any{
			"error": map[string]any{"code": 401, "message": "invalid client credentials"},
		}), nil, http.StatusUnauthorized, "mint access token: "},
		{"the mint answers without a token", mintAnswers(http.StatusOK, map[string]any{
			"token_type": "Bearer", "expires_in": 900,
		}), nil, 0, "no access_token"},
		{"the mint never reaches the server", gone, nil, 0, "mint access token: "},
		{"the grant can't be built", mintAnswers(http.StatusOK, map[string]any{}), unbuildable, 0,
			"build token grant: key file unreadable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cred := tc.cred
			if cred == nil {
				cred = NewServiceAccountCredential("id", "secret", nil)
			}
			c, err := New(tc.srv.URL, WithCredential(cred))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = c.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil)
			if !errors.Is(err, ErrTokenMint) {
				t.Fatalf("error %v is not ErrTokenMint", err)
			}
			if !strings.Contains(err.Error(), tc.wantInMsg) {
				t.Errorf("error %q, want it to contain %q", err, tc.wantInMsg)
			}
			var apiErr *APIError
			if tc.wantAPI != 0 && (!errors.As(err, &apiErr) || apiErr.StatusCode != tc.wantAPI) {
				t.Errorf("error %v, want it to still unwrap to the %d *APIError", err, tc.wantAPI)
			}
		})
	}

	// The operation's own 401, after a good mint, is not a mint failure.
	f := &fakeAPI{readSays: http.StatusUnauthorized}
	r, err := testClient(t, f.server(t)).ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := CheckResponse(r.HTTPResponse, r.Body); err == nil || errors.Is(err, ErrTokenMint) {
		t.Errorf("CheckResponse = %v, want a 401 that is not ErrTokenMint", err)
	}
}

func TestAuthTransport_HonoursContextWhileWaitingForAMint(t *testing.T) {
	cache := newTokenCache(
		NewServiceAccountCredential("id", "secret", nil),
		DefaultRefreshSkew,
		func(ctx context.Context, _ MintTokenJSONBody) (*TokenResponse, error) {
			<-ctx.Done() // hold the gate until the first caller gives up
			return nil, ctx.Err()
		})

	holding := make(chan struct{})
	holdCtx, releaseHold := context.WithCancel(context.Background())
	go func() {
		close(holding)
		_, _ = cache.get(holdCtx)
	}()
	<-holding
	time.Sleep(10 * time.Millisecond) // let the goroutine take the gate

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := cache.get(ctx)
	releaseHold()

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("get = %v, want context.DeadlineExceeded -- a waiter must not block past its context", err)
	}
	if errors.Unwrap(err) != nil || errors.Is(err, ErrTokenMint) {
		t.Errorf("get = %v, want ctx.Err() as it is: a waiter that gave up says nothing about the credential", err)
	}
}

func TestNew_WithoutCredentialSendsNoAuthorization(t *testing.T) {
	f := &fakeAPI{}
	srv := f.server(t)
	c, err := New(srv.URL)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := c.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := f.mints.Load(); got != 0 {
		t.Errorf("mints = %d, want 0 without a credential", got)
	}
	if got, _ := f.authSeen.Load().(string); got != "" {
		t.Errorf("Authorization = %q, want none", got)
	}
}

// A caller that sets its own Authorization header keeps it: the transport must
// not mint over a token someone deliberately supplied.
func TestAuthTransport_LeavesACallerSuppliedHeaderAlone(t *testing.T) {
	f := &fakeAPI{}
	srv := f.server(t)
	c := testClient(t, srv, WithClientOption(WithRequestEditorFn(
		func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer caller-supplied")
			return nil
		})))

	if _, err := c.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil); err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := f.mints.Load(); got != 0 {
		t.Errorf("mints = %d, want 0 when the caller supplied a token", got)
	}
	if got, _ := f.authSeen.Load().(string); got != "Bearer caller-supplied" {
		t.Errorf("Authorization = %q, want the caller's header", got)
	}
}

// Two clients keep two tokens. ADR D4 forbids package-level state; the cache
// belongs to the client, so nothing leaks between them.
func TestTokenCache_IsPerClientNotPackageLevel(t *testing.T) {
	f := &fakeAPI{}
	srv := f.server(t)
	first := testClient(t, srv)
	second := testClient(t, srv)

	for _, c := range []*ClientWithResponses{first, second} {
		if _, err := c.ListClusterTypesWithResponse(context.Background(), "us-texas-1", nil); err != nil {
			t.Fatalf("read: %v", err)
		}
	}
	if got := f.mints.Load(); got != 2 {
		t.Errorf("mints = %d, want 2 -- each client holds its own token", got)
	}
}

func TestNew_Validation(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Error("New(\"\") = nil error, want a missing-endpoint error")
	}
	if _, err := New("https://example.test", WithCredential(nil)); err == nil {
		t.Error("WithCredential(nil) = nil error, want an error")
	}
	if _, err := New("https://example.test", WithRefreshSkew(-time.Second)); err == nil {
		t.Error("WithRefreshSkew(-1s) = nil error, want an error")
	}
	if _, err := New("https://example.test", WithTransport(nil)); err == nil {
		t.Error("WithTransport(nil) = nil error, want an error")
	}
}
