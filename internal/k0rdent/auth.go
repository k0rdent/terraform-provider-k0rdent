package k0rdent

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultRefreshSkew is how long before expiry a cached token is replaced.
//
// Access tokens are minted with a fixed 900-second lifetime and machine grants
// issue no refresh token, so the only way to get a new one is to present the
// long-lived secret again. Sixty seconds of headroom covers request latency and
// modest clock skew between this process and the token issuer. Widen it with
// [WithRefreshSkew] if a landscape turns out to be further off than that.
//
// The skew never exceeds half a token's lifetime. An operator can shorten the
// lifetime, and a 30-second token with 60 seconds of skew would be stale the
// moment it was cached, so every request would mint.
const DefaultRefreshSkew = 60 * time.Second

// Credential mints access tokens. It returns the request body for
// `POST /v1/regions/global/auth/token`, built from a generated grant type --
// there is no hand-written grant struct in this package (ADR D1).
//
// Build one with [NewServiceAccountCredential] or [NewAPIKeyCredential]. A
// service account is the right choice for a provider: an API key carries the
// intersection of its own roles and its creating user's current roles, so it
// stops working when that person is off-boarded.
type Credential interface {
	// Grant returns a fresh mint body. It is called once per mint, not once
	// per client, so an implementation may read rotating material.
	Grant() (MintTokenJSONBody, error)
}

type credentialFunc func() (MintTokenJSONBody, error)

func (f credentialFunc) Grant() (MintTokenJSONBody, error) { return f() }

// NewServiceAccountCredential returns a Credential for the
// `client_credentials` grant. narrowing is optional and may only reduce the
// credential's own scope and roles; pass nil for the credential's full
// privileges.
func NewServiceAccountCredential(clientID, clientSecret string, narrowing *ScopeNarrowing) Credential {
	return credentialFunc(func() (MintTokenJSONBody, error) {
		var body MintTokenJSONBody
		err := body.FromClientCredentialsGrant(ClientCredentialsGrant{
			GrantType:      ClientCredentialsGrantGrantTypeClientCredentials,
			ClientId:       clientID,
			ClientSecret:   clientSecret,
			ScopeNarrowing: narrowing,
		})
		return body, err
	})
}

// NewAPIKeyCredential returns a Credential for the `api_key` grant.
//
// Prefer [NewServiceAccountCredential]. Beyond inheriting a human's roles, this
// grant has an undocumented precondition: the owning user must have completed
// an interactive OIDC login at least once, or the mint returns 401 with nothing
// in the response to say why.
func NewAPIKeyCredential(apiKey string, narrowing *ScopeNarrowing) Credential {
	return credentialFunc(func() (MintTokenJSONBody, error) {
		var body MintTokenJSONBody
		err := body.FromApiKeyGrant(ApiKeyGrant{
			GrantType:      ApiKeyGrantGrantTypeApiKey,
			ApiKey:         apiKey,
			ScopeNarrowing: narrowing,
		})
		return body, err
	})
}

// tokenCache holds one access token and hands it to every caller, minting at
// most once per expiry window however many goroutines ask at once.
//
// That sharing is the whole point. The auth service allows 10 mints per 60
// seconds per credential -- each one costs an argon2id verification -- and
// Terraform's default parallelism is also 10. A transport that minted per
// request, or per resource, would spend its entire budget on the first apply
// and start collecting 429s.
//
// ADR D4 says the client holds no package-level state, and this does not: a
// cache belongs to the client that [New] created, so two clients with two
// credentials keep two tokens and a test holds nothing that outlives it. D4's
// target is shared mutable state reachable from anywhere, which is what makes
// concurrent provider runs interfere; per-client state is just the credential
// the caller handed over, kept where they handed it.
type tokenCache struct {
	mint  func(ctx context.Context, body MintTokenJSONBody) (*TokenResponse, error)
	cred  Credential
	skew  time.Duration
	clock func() time.Time

	// gate serialises minting. A channel rather than a sync.Mutex so a
	// caller waiting behind someone else's mint still honours its context:
	// a cancelled Terraform run should stop waiting, not block until an
	// unrelated HTTP call returns.
	gate chan struct{}

	mu          sync.RWMutex
	accessToken string
	refreshAt   time.Time
}

func newTokenCache(cred Credential, skew time.Duration, mint func(context.Context, MintTokenJSONBody) (*TokenResponse, error)) *tokenCache {
	return &tokenCache{
		mint:  mint,
		cred:  cred,
		skew:  skew,
		clock: time.Now,
		gate:  make(chan struct{}, 1),
	}
}

// token returns a cached access token, minting one if none is usable.
func (c *tokenCache) get(ctx context.Context) (string, error) {
	if tok, ok := c.cached(); ok {
		return tok, nil
	}

	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return "", ctx.Err()
	}

	// Someone else may have minted while this call waited for the gate.
	if tok, ok := c.cached(); ok {
		return tok, nil
	}

	body, err := c.cred.Grant()
	if err != nil {
		return "", fmt.Errorf("%w: build token grant: %w", ErrTokenMint, err)
	}
	resp, err := c.mint(ctx, body)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrTokenMint, err)
	}
	if resp.AccessToken == "" {
		return "", fmt.Errorf("%w: token endpoint returned no access_token", ErrTokenMint)
	}

	// Read expires_in rather than assuming 900: it is stamped on the
	// identity-provider client at registration and an operator can change it.
	// The contract requires it and calls it the lifetime in seconds, so zero
	// or less is either a dead token or a missing field decoded as 0.
	if resp.ExpiresIn <= 0 {
		return "", fmt.Errorf("%w: token endpoint returned expires_in %d, want a positive lifetime in seconds",
			ErrTokenMint, resp.ExpiresIn)
	}
	lifetime := time.Duration(resp.ExpiresIn) * time.Second
	skew := min(c.skew, lifetime/2)

	c.mu.Lock()
	c.accessToken = resp.AccessToken
	c.refreshAt = c.clock().Add(lifetime - skew)
	c.mu.Unlock()

	return resp.AccessToken, nil
}

func (c *tokenCache) cached() (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.accessToken == "" {
		return "", false
	}
	if !c.clock().Before(c.refreshAt) {
		return "", false
	}
	return c.accessToken, true
}

// authTransport attaches a bearer token to every request for the endpoint.
//
// It does one thing beyond that: it mints before the token expires. It does not
// replay a request after a 401, and that is deliberate rather than an omission.
// Credential deletion and service-account disable revoke outstanding tokens
// immediately, so a mid-apply 401 usually means revoked, not expired --
// replaying burns the mint budget and cannot recover. Proactive refresh already
// covers expiry, which is the case a long apply actually hits. And D4 forbids
// retrying, for a sharper reason here: replaying a create after a 401 risks a
// second cluster if the request had in fact been accepted.
//
// The token goes only to the endpoint's scheme and host. Go strips a
// caller-set Authorization header when a redirect crosses hosts, but this one
// is added per hop, inside the transport, so that rule never applies here. A
// request anywhere else goes out without it.
type authTransport struct {
	base   http.RoundTripper
	origin *url.URL
	cache  *tokenCache
}

func (t *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Never overwrite an Authorization header the caller set, and never add
	// one to the mint itself -- the token endpoint is anonymous, and the
	// mint runs on its own unauthenticated client anyway.
	if req.Header.Get("Authorization") != "" || !t.ownsHost(req.URL) {
		return t.base.RoundTrip(req)
	}

	tok, err := t.cache.get(req.Context())
	if err != nil {
		return nil, err
	}

	// RoundTrip must not modify its argument.
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer "+tok)
	return t.base.RoundTrip(clone)
}

func (t *authTransport) ownsHost(u *url.URL) bool {
	return u.Scheme == t.origin.Scheme && strings.EqualFold(u.Host, t.origin.Host)
}
