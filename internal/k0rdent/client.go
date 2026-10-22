package k0rdent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Option configures [New].
type Option func(*options) error

type options struct {
	cred       Credential
	transport  http.RoundTripper
	skew       time.Duration
	timeout    time.Duration
	userAgent  string
	clientOpts []ClientOption
}

// WithCredential sets the credential the client mints access tokens with.
// Without it, [New] returns an unauthenticated client and the caller supplies
// its own Authorization header.
func WithCredential(c Credential) Option {
	return func(o *options) error {
		if c == nil {
			return errors.New("k0rdent: WithCredential given a nil Credential")
		}
		o.cred = c
		return nil
	}
}

// WithTransport sets the round tripper underneath the bearer-token transport.
// Use it for a proxy, custom TLS, or a test server. Defaults to a clone of
// [http.DefaultTransport], a connection pool this client holds alone.
func WithTransport(rt http.RoundTripper) Option {
	return func(o *options) error {
		if rt == nil {
			return errors.New("k0rdent: WithTransport given a nil RoundTripper")
		}
		o.transport = rt
		return nil
	}
}

// WithRefreshSkew sets how long before expiry a cached token is replaced.
// Defaults to [DefaultRefreshSkew]. It is capped at half of each token's
// lifetime.
func WithRefreshSkew(d time.Duration) Option {
	return func(o *options) error {
		if d < 0 {
			return fmt.Errorf("k0rdent: WithRefreshSkew given a negative duration %s", d)
		}
		o.skew = d
		return nil
	}
}

// WithTimeout sets the per-request timeout on the underlying [http.Client].
// Zero, the default, means no client-side timeout: callers pass a context, and
// a cluster create runs far longer than any timeout worth hard-coding.
func WithTimeout(d time.Duration) Option {
	return func(o *options) error {
		if d < 0 {
			return fmt.Errorf("k0rdent: WithTimeout given a negative duration %s", d)
		}
		o.timeout = d
		return nil
	}
}

// WithUserAgent sets the User-Agent header on every request a generated method
// sends, the token mint included. It is applied as a request editor, so a
// request sent straight through the client's doer goes without it.
func WithUserAgent(ua string) Option {
	return func(o *options) error {
		if ua == "" {
			return errors.New("k0rdent: WithUserAgent given an empty string")
		}
		o.userAgent = ua
		return nil
	}
}

// WithClientOption passes a generated [ClientOption] through to the generated
// constructor, for the request editors and base-URL handling it already
// provides. [New] refuses [WithHTTPClient] here, or any option that replaces
// or wraps the doer: it would replace the client that carries the credential.
// Use [WithTransport] and [WithTimeout] instead.
func WithClientOption(opts ...ClientOption) Option {
	return func(o *options) error {
		o.clientOpts = append(o.clientOpts, opts...)
		return nil
	}
}

// New returns a generated client for endpoint, wired to mint and attach bearer
// tokens.
//
// It returns the generated [ClientWithResponses] rather than a wrapper. There
// is nothing useful to wrap: the generated methods are the API, and every
// wrapper method would be a place for a hand-written struct to appear. What
// this constructor adds is the wiring the generated code cannot do for
// itself -- a transport that holds the credential, and one token shared across
// concurrent calls.
//
// endpoint is the API base URL, e.g. https://api.k0rdent.ai. It must be
// https, or http to a loopback address, because the mint sends the client
// secret. Region and project are path parameters on the generated methods,
// not client configuration.
//
// Check for failures with [CheckResponse]:
//
//	c, err := k0rdent.New(endpoint,
//	        k0rdent.WithCredential(k0rdent.NewServiceAccountCredential(id, secret, nil)))
//	if err != nil {
//	        return err
//	}
//	r, err := c.ListClusterTypesWithResponse(ctx, region, nil)
//	if err != nil {
//	        return err
//	}
//	if err := k0rdent.CheckResponse(r.HTTPResponse, r.Body); err != nil {
//	        return err
//	}
func New(endpoint string, opts ...Option) (*ClientWithResponses, error) {
	if endpoint == "" {
		return nil, errors.New("k0rdent: endpoint is required")
	}
	o := options{skew: DefaultRefreshSkew}
	for _, opt := range opts {
		if err := opt(&o); err != nil {
			return nil, err
		}
	}

	if o.transport == nil {
		o.transport = ownPool()
	}
	if o.userAgent != "" {
		o.clientOpts = append(o.clientOpts, WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("User-Agent", o.userAgent)
			return nil
		}))
	}

	httpClient := &http.Client{Timeout: o.timeout, Transport: o.transport, CheckRedirect: refuseRedirect}
	origin, err := checkClientOptions(endpoint, httpClient, o.clientOpts)
	if err != nil {
		return nil, err
	}

	if o.cred != nil {
		// The mint runs on its own client, off the authenticating
		// transport. The token endpoint is anonymous, and routing the
		// mint through the transport that needs the token would recurse.
		var minter *ClientWithResponses
		minter, err = NewClientWithResponses(endpoint,
			append([]ClientOption{WithHTTPClient(&http.Client{
				Timeout:       o.timeout,
				Transport:     o.transport,
				CheckRedirect: refuseRedirect,
			})}, o.clientOpts...)...)
		if err != nil {
			return nil, fmt.Errorf("k0rdent: build token client: %w", err)
		}
		httpClient.Transport = &authTransport{
			base:   o.transport,
			origin: origin,
			cache:  newTokenCache(o.cred, o.skew, mintWith(minter)),
		}
	}

	client, err := NewClientWithResponses(endpoint,
		append([]ClientOption{WithHTTPClient(httpClient)}, o.clientOpts...)...)
	if err != nil {
		return nil, fmt.Errorf("k0rdent: build client: %w", err)
	}
	return client, nil
}

// checkClientOptions runs the caller's generated options against a scratch
// client that holds New's own doer, to see what they would change before New
// relies on it, and checks the endpoint requests will actually go to, after
// any WithBaseURL.
//
// An option that replaces the doer is refused rather than overridden. The
// generated constructor applies options in order, so a caller's doer replaces
// the one New built, and with it the auth transport and the redirect refusal:
// every request goes out with no token and nothing says why. Applying New's
// own doer last instead would drop the caller's just as quietly. Wrapping the
// doer is not supported either. A wrapper is a replacement here, and is
// refused the same way.
func checkClientOptions(endpoint string, own HttpRequestDoer, opts []ClientOption) (*url.URL, error) {
	scratch := Client{Server: endpoint, Client: own}
	for _, opt := range opts {
		if err := opt(&scratch); err != nil {
			return nil, fmt.Errorf("k0rdent: client option: %w", err)
		}
	}
	if scratch.Client != own {
		return nil, errors.New("k0rdent: a client option replaced the HTTP client, which would drop " +
			"the credential. Use WithTransport for a proxy, TLS or a test server, and " +
			"WithTimeout for a timeout")
	}
	return checkEndpoint(scratch.Server)
}

// checkEndpoint takes an absolute https URL, or http to a loopback address and
// nowhere else, and returns it parsed. The mint sends the client secret in its
// body, and every call after it a bearer token, so a cleartext endpoint puts
// both on the wire. The local fake needs http, and it is always loopback.
func checkEndpoint(endpoint string) (*url.URL, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("k0rdent: endpoint %q is not a URL: %w", endpoint, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("k0rdent: endpoint %q must be an absolute https:// URL", endpoint)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("k0rdent: endpoint %q has no host", endpoint)
	}
	if u.Scheme == "http" && !isLoopback(u.Hostname()) {
		return nil, fmt.Errorf("k0rdent: endpoint %q is http, and the credential would cross the wire "+
			"in cleartext. Use https. http is accepted only for a loopback address", endpoint)
	}
	return u, nil
}

// isLoopback reports whether a host is this machine. localhost is named
// because net.ParseIP does not resolve it, and a DNS lookup here would be slow
// and spoofable. Host names are case-insensitive and may carry the trailing dot
// of a fully qualified name, so LOCALHOST and localhost. count too.
func isLoopback(host string) bool {
	if strings.EqualFold(strings.TrimSuffix(host, "."), "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ownPool returns a connection pool for one client: a clone of
// http.DefaultTransport, so proxy, dial, TLS and keep-alive settings are Go's
// defaults, but nothing else in the process holds it.
//
// Sending through http.DefaultTransport itself would let any other user of it
// break this client's requests. Its CloseIdleConnections races a request that
// has just picked up an idle connection, which then fails with "http:
// CloseIdleConnections called", and net/http does not replay a POST after
// that. httptest.Server.Close calls it on every Close.
//
// If something in the process replaced the default with a RoundTripper that
// cannot be cloned, that replacement was deliberate (tracing, a proxy), so it
// is used as it is.
func ownPool() http.RoundTripper {
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		return t.Clone()
	}
	return http.DefaultTransport
}

// refuseRedirect is CheckRedirect for both of New's http.Clients. A 307 or 308
// replays the method and the body, and the mint's body is the client secret,
// so following one hands the secret to whatever host the Location names. Go
// drops Authorization on a cross-host redirect, but the follow-up goes back
// through authTransport, which stamps the bearer token on it again.
//
// It returns an error rather than [http.ErrUseLastResponse]. Handing back the
// 3xx would leave the generated response with every JSON field nil and a
// status that a caller checking only for 404 or 200 can read past. An error
// from the call itself cannot be missed.
func refuseRedirect(req *http.Request, via []*http.Request) error {
	from, resp := via[len(via)-1], req.Response
	var id string
	if v := RequestIDFromHeader(resp.Header); v != "" {
		id = fmt.Sprintf(" (requestId %q)", v)
	}
	// %q, not %s: the status line is server-controlled text on its way to
	// a log or a terminal, and quoting escapes any control sequences in it.
	return fmt.Errorf("k0rdent: refusing to follow %q%s from %s %s to %q: "+
		"a redirect replays the request, credential included. "+
		"Point the endpoint at the address the API is served on",
		resp.Status, id, from.Method, from.URL.Path, req.URL.String())
}

// mintWith adapts the generated MintToken operation to what tokenCache needs.
func mintWith(c *ClientWithResponses) func(context.Context, MintTokenJSONBody) (*TokenResponse, error) {
	return func(ctx context.Context, body MintTokenJSONBody) (*TokenResponse, error) {
		resp, err := c.MintTokenWithResponse(ctx, body)
		if err != nil {
			return nil, err
		}
		if err := CheckResponse(resp.HTTPResponse, resp.Body); err != nil {
			return nil, err
		}
		if resp.JSON200 == nil {
			return nil, fmt.Errorf("%s returned no token body", resp.Status())
		}
		return resp.JSON200, nil
	}
}
