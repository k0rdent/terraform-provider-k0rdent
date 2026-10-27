package provider

// The kubeconfig relay behind cluster_kubeconfig's `relay`. Terraform can't
// give a configured kubernetes or helm provider a new token, so the relay
// hands out a fixed loopback address and secret, and forwards each request
// with the platform's current token.
//
// It fetches a new kubeconfig before the JWT's `exp`. A 401 is only a
// fallback, replayed once, and only the authenticator's 401, the one that
// means the request never ran.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/terraform-plugin-log/tflog"
	"google.golang.org/protobuf/encoding/protowire"
)

const (
	// relayBodyLimit is the largest body kept for a replay. The API server
	// refuses bodies over 3 MiB anyway.
	relayBodyLimit = 4 << 20

	// relayMaxMargin is how long before `exp` to fetch, at most. A short
	// token gets a quarter of its lifetime.
	relayMaxMargin = time.Minute

	// relayMaxFetchGap is the gap between fetches, since the platform
	// rate-limits kubeconfig mints. A short token gets a quarter of its
	// lifetime.
	relayMaxFetchGap = 10 * time.Second

	// relayFreshFor is how long a token fetched after a 401 is fresh. A 401
	// on a fresh token isn't expiry, so it goes back as it is.
	relayFreshFor = time.Minute
)

// errRelayRejected means the cluster's 401 goes back as it is.
var errRelayRejected = errors.New("the cluster refused the token, and the relay has already fetched once for it")

type relayFetch func(ctx context.Context) (kubeconfigConnection, error)

// relayUpstream is one fetched kubeconfig. It never changes.
type relayUpstream struct {
	conn      kubeconfigConnection
	url       *url.URL
	transport *http.Transport

	fetched time.Time
	// expires is the token's `exp`, zero when it carries none.
	expires time.Time
	// refreshAt is when to fetch again, zero without `exp`.
	refreshAt time.Time
	// afterRejection is set when this token replaced one the cluster refused.
	afterRejection bool
}

func (u *relayUpstream) expired(now time.Time) bool {
	return !u.expires.IsZero() && !now.Before(u.expires)
}

func (u *relayUpstream) fetchGap() time.Duration {
	if life := u.expires.Sub(u.fetched); life > 0 && life/4 < relayMaxFetchGap {
		return life / 4
	}
	return relayMaxFetchGap
}

type kubeconfigRelay struct {
	// host, caPEM and secret are what the ephemeral hands out.
	host   string
	caPEM  string
	secret string

	srv *http.Server
	// ctx carries the open's logger. Cancelling it ends every open request.
	ctx    context.Context
	cancel context.CancelFunc

	fetch        relayFetch
	fetchTimeout time.Duration
	now          func() time.Time

	mu        sync.Mutex
	up        *relayUpstream
	flight    chan struct{} // closed when the fetch in flight ends, nil when none is
	lastFetch time.Time
	fetchErr  error // the last fetch's failure, nil after a success
}

func startKubeconfigRelay(ctx context.Context, first kubeconfigConnection, fetch relayFetch, fetchTimeout time.Duration) (*kubeconfigRelay, error) {
	return startKubeconfigRelayAt(ctx, first, fetch, fetchTimeout, time.Now)
}

// startKubeconfigRelayAt takes a clock the tests can move.
func startKubeconfigRelayAt(ctx context.Context, first kubeconfigConnection, fetch relayFetch, fetchTimeout time.Duration,
	now func() time.Time) (*kubeconfigRelay, error) {
	r := &kubeconfigRelay{fetch: fetch, fetchTimeout: fetchTimeout, now: now}
	up, err := r.newUpstream(first, nil, false)
	if err != nil {
		return nil, err
	}
	r.up = up

	caPEM, cert, err := relayCertificates()
	if err != nil {
		return nil, fmt.Errorf("could not make the relay's certificate: %w", err)
	}
	r.caPEM = caPEM
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return nil, fmt.Errorf("could not make the relay's secret: %w", err)
	}
	r.secret = hex.EncodeToString(secret)

	// HTTP/1.1 only, for exec, attach and port-forward upgrades.
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"http/1.1"},
	})
	if err != nil {
		return nil, fmt.Errorf("could not listen on the loopback address: %w", err)
	}
	r.host = "https://" + ln.Addr().String()
	r.ctx, r.cancel = context.WithCancel(context.WithoutCancel(ctx))

	proxy := &httputil.ReverseProxy{
		// Rewrite, not Director, so no X-Forwarded headers. send sets the URL
		// and token per attempt.
		Rewrite:       func(*httputil.ProxyRequest) {},
		Transport:     relayTransport{r},
		FlushInterval: -1,
		ErrorHandler:  r.proxyError,
		ErrorLog:      log.New(relayLogWriter{r}, "", 0),
	}
	r.srv = &http.Server{
		Handler:           r.guard(proxy),
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       90 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return r.ctx },
		ErrorLog:          log.New(relayLogWriter{r}, "", 0),
	}
	go func() {
		if err := r.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			tflog.Warn(r.ctx, "kubeconfig relay stopped serving", map[string]any{"error": redactDiagnosticText(err.Error())})
		}
	}()
	tflog.Debug(r.ctx, "kubeconfig relay started", map[string]any{"address": ln.Addr().String()})
	return r, nil
}

// close stops the relay and ends every open request, watches included.
func (r *kubeconfigRelay) close() {
	r.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := r.srv.Shutdown(ctx); err != nil {
		_ = r.srv.Close()
	}
	r.mu.Lock()
	up := r.up
	r.mu.Unlock()
	up.transport.CloseIdleConnections()
	tflog.Debug(r.ctx, "kubeconfig relay closed")
}

// guard refuses a request without the secret, and keeps the body for a replay.
func (r *kubeconfigRelay) guard(next http.Handler) http.Handler {
	want := []byte("Bearer " + r.secret)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), want) != 1 {
			writeRelayStatus(w, http.StatusUnauthorized, "Unauthorized",
				"the kubeconfig relay accepts only the token it was opened with")
			return
		}
		if req.Body != nil && req.Body != http.NoBody {
			body, err := io.ReadAll(io.LimitReader(req.Body, relayBodyLimit+1))
			if err != nil {
				writeRelayStatus(w, http.StatusBadRequest, "BadRequest", "the kubeconfig relay could not read the request body")
				return
			}
			if len(body) <= relayBodyLimit {
				req.Body = io.NopCloser(bytes.NewReader(body))
				req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
				req.ContentLength = int64(len(body))
			} else {
				// Too large to keep, so sent once.
				req.Body = struct {
					io.Reader
					io.Closer
				}{io.MultiReader(bytes.NewReader(body), req.Body), req.Body}
				req.GetBody = nil
			}
		}
		next.ServeHTTP(w, req)
	})
}

type relayTransport struct{ r *kubeconfigRelay }

func (t relayTransport) RoundTrip(out *http.Request) (*http.Response, error) {
	return t.r.roundTrip(out)
}

// roundTrip sends out, and once more with a new token if the authenticator
// refused it.
func (r *kubeconfigRelay) roundTrip(out *http.Request) (*http.Response, error) {
	ctx := out.Context()
	up, err := r.upstream(ctx, nil)
	if err != nil {
		return nil, err
	}
	resp, err := r.send(out, up)
	if err != nil || !tokenRefused(out, resp) {
		return resp, err
	}
	if out.Body != nil && out.Body != http.NoBody && out.GetBody == nil {
		return resp, nil
	}
	next, err := r.upstream(ctx, up)
	switch {
	case errors.Is(err, errRelayRejected):
		return resp, nil
	case err != nil:
		_ = resp.Body.Close()
		return nil, err
	case next.conn.Token == up.conn.Token:
		return resp, nil
	}
	_ = resp.Body.Close()
	return r.send(out, next)
}

const relayStatusPeek = 1 << 10

// authenticatorRefusal is the Status the API server's authentication filter
// writes for a token it refuses, before any handler runs.
var authenticatorRefusal = apiStatus{Kind: "Status", Status: "Failure", Message: "Unauthorized", Reason: "Unauthorized", Code: http.StatusUnauthorized}

// tokenRefused reports whether resp is the authenticator's 401, the only one
// that means the request never ran. A `proxy` path is never one: a pod behind
// it can answer in the same words. The body is put back.
func tokenRefused(out *http.Request, resp *http.Response) bool {
	if resp.StatusCode != http.StatusUnauthorized || slices.Contains(strings.Split(out.URL.Path, "/"), "proxy") {
		return false
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, relayStatusPeek+1))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	if err != nil || len(head) > relayStatusPeek {
		return false
	}
	status, ok := decodeAPIStatus(head)
	return ok && status == authenticatorRefusal
}

type apiStatus struct {
	Kind    string `json:"kind"`
	Status  string `json:"status"`
	Message string `json:"message"`
	Reason  string `json:"reason"`
	Code    int64  `json:"code"`
}

var k8sProtobufPrefix = []byte("k8s\x00")

// decodeAPIStatus reads a Status in JSON or protobuf. The protobuf is a
// runtime.Unknown: field 1 is TypeMeta (kind in field 2), field 2 the Status
// (status, message, reason and code in fields 2, 3, 4 and 6).
func decodeAPIStatus(b []byte) (apiStatus, bool) {
	var s apiStatus
	unknown, isProtobuf := bytes.CutPrefix(b, k8sProtobufPrefix)
	if !isProtobuf {
		return s, json.Unmarshal(b, &s) == nil
	}
	var raw []byte
	ok := eachProtobufField(unknown, func(num protowire.Number, v []byte, _ uint64) bool {
		switch num {
		case 1:
			return eachProtobufField(v, func(num protowire.Number, v []byte, _ uint64) bool {
				if num == 2 {
					s.Kind = string(v)
				}
				return true
			})
		case 2:
			raw = v
		}
		return true
	})
	return s, ok && eachProtobufField(raw, func(num protowire.Number, v []byte, n uint64) bool {
		switch num {
		case 2:
			s.Status = string(v)
		case 3:
			s.Message = string(v)
		case 4:
			s.Reason = string(v)
		case 6:
			s.Code = int64(int32(n))
		}
		return true
	})
}

// eachProtobufField calls f for each field of b. False when b is malformed or
// f stops.
func eachProtobufField(b []byte, f func(num protowire.Number, v []byte, n uint64) bool) bool {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return false
		}
		b = b[n:]
		var v []byte
		var x uint64
		switch typ {
		case protowire.BytesType:
			v, n = protowire.ConsumeBytes(b)
		case protowire.VarintType:
			x, n = protowire.ConsumeVarint(b)
		default:
			n = protowire.ConsumeFieldValue(num, typ, b)
		}
		if n < 0 || !f(num, v, x) {
			return false
		}
		b = b[n:]
	}
	return true
}

// send leaves out unchanged, for a replay.
func (r *kubeconfigRelay) send(out *http.Request, up *relayUpstream) (*http.Response, error) {
	req := out.Clone(out.Context())
	if out.GetBody != nil {
		body, err := out.GetBody()
		if err != nil {
			return nil, err
		}
		req.Body = body
	}
	(&httputil.ProxyRequest{Out: req}).SetURL(up.url)
	req.Header.Set("Authorization", "Bearer "+up.conn.Token)
	return up.transport.RoundTrip(req)
}

// upstream returns the kubeconfig to send with, starting a fetch when the
// token is due or rejected (the one the cluster just refused). One fetch runs
// at a time. Only requests whose token has expired or been refused wait for
// it, and they get its error if it fails.
func (r *kubeconfigRelay) upstream(ctx context.Context, rejected *relayUpstream) (*relayUpstream, error) {
	r.mu.Lock()
	for {
		up, now := r.up, r.now()
		if r.flight != nil {
			if rejected == nil && !up.expired(now) {
				r.mu.Unlock()
				return up, nil
			}
			done := r.flight
			r.mu.Unlock()
			select {
			case <-done:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			r.mu.Lock()
			continue
		}
		if rejected != nil && rejected != up {
			r.mu.Unlock()
			return up, nil
		}
		refused := rejected != nil
		due := !up.refreshAt.IsZero() && !now.Before(up.refreshAt)
		switch {
		case !refused && !due:
			r.mu.Unlock()
			return up, nil
		case refused && up.afterRejection && now.Sub(up.fetched) < relayFreshFor:
			r.mu.Unlock()
			return nil, errRelayRejected
		case now.Sub(r.lastFetch) < up.fetchGap():
			err := r.fetchErr
			r.mu.Unlock()
			switch {
			case err != nil && (refused || up.expired(now)):
				return nil, err
			case refused:
				return nil, errRelayRejected
			}
			return up, nil
		}
		done := make(chan struct{})
		r.flight, r.lastFetch = done, now
		r.mu.Unlock()
		go r.refresh(up, refused, done)
		r.mu.Lock()
	}
}

// refresh replaces prev and closes done. It uses the relay's context, not a
// request's, since other requests wait on it.
func (r *kubeconfigRelay) refresh(prev *relayUpstream, afterRejection bool, done chan struct{}) {
	why := "the token is due to expire"
	if afterRejection {
		why = "the cluster refused the token"
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.fetchTimeout)
	defer cancel()
	conn, err := r.fetch(ctx)
	var next *relayUpstream
	if err == nil {
		next, err = r.newUpstream(conn, prev, afterRejection)
	}

	r.mu.Lock()
	defer func() {
		r.flight = nil
		close(done)
		r.mu.Unlock()
	}()
	if err != nil {
		r.fetchErr = fmt.Errorf("the kubeconfig relay could not fetch a new kubeconfig for this cluster, "+
			"because %s: %w", why, err)
		tflog.Warn(r.ctx, "kubeconfig relay could not fetch a new kubeconfig", map[string]any{
			"reason": why,
			"error":  redactDiagnosticText(err.Error()),
		})
		return
	}
	if next.transport != prev.transport {
		prev.transport.CloseIdleConnections()
	}
	r.up, r.fetchErr = next, nil
	tflog.Debug(r.ctx, "kubeconfig relay fetched a new kubeconfig", map[string]any{
		"reason":     why,
		"expires_in": durationOrUnknown(next.expires, next.fetched),
	})
}

func durationOrUnknown(expires, from time.Time) string {
	if expires.IsZero() {
		return "unknown"
	}
	return expires.Sub(from).Round(time.Second).String()
}

// newUpstream keeps prev's transport when the cluster and certificates match.
func (r *kubeconfigRelay) newUpstream(conn kubeconfigConnection, prev *relayUpstream, afterRejection bool) (*relayUpstream, error) {
	if conn.Token == "" {
		return nil, errors.New("the kubeconfig carries no token")
	}
	u, err := url.Parse(conn.Host)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("the kubeconfig's server is not an https URL")
	}
	up := &relayUpstream{conn: conn, url: u, fetched: r.now(), afterRejection: afterRejection}
	if prev != nil && sameConnection(prev.conn, conn) {
		up.transport = prev.transport
	} else if up.transport, err = relayUpstreamTransport(conn); err != nil {
		return nil, err
	}
	if exp := tokenExpiry(conn.Token); exp.After(up.fetched) {
		up.expires = exp
		margin := exp.Sub(up.fetched) / 4
		if margin > relayMaxMargin {
			margin = relayMaxMargin
		}
		up.refreshAt = exp.Add(-margin)
	}
	return up, nil
}

// sameConnection reports whether a and b differ only in their token.
func sameConnection(a, b kubeconfigConnection) bool {
	a.Token, b.Token = "", ""
	return a == b
}

// relayUpstreamTransport connects as kubectl would: the kubeconfig's CA (or
// the system roots), tls-server-name, client certificate and proxy-url, else
// HTTPS_PROXY and NO_PROXY.
func relayUpstreamTransport(conn kubeconfigConnection) (*http.Transport, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: conn.TLSServerName}
	if conn.ClusterCACertificate != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(conn.ClusterCACertificate)) {
			return nil, errors.New("the kubeconfig's certificate authority is not a PEM certificate")
		}
		cfg.RootCAs = pool
	}
	if conn.ClientCertificate != "" && conn.ClientKey != "" {
		pair, err := tls.X509KeyPair([]byte(conn.ClientCertificate), []byte(conn.ClientKey))
		if err != nil {
			return nil, errors.New("the kubeconfig's client certificate and key are not a PEM pair")
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	proxy := http.ProxyFromEnvironment
	if conn.ProxyURL != "" {
		// Never quoted: it can carry a password.
		u, err := url.Parse(conn.ProxyURL)
		if err != nil || u.Host == "" || !slices.Contains([]string{"http", "https", "socks5", "socks5h"}, u.Scheme) {
			return nil, errors.New("the kubeconfig's proxy-url is not an http, https or socks5 URL")
		}
		proxy = http.ProxyURL(u)
	}
	return &http.Transport{
		Proxy:               proxy,
		TLSClientConfig:     cfg,
		TLSHandshakeTimeout: 10 * time.Second,
		MaxIdleConnsPerHost: 16,
		IdleConnTimeout:     90 * time.Second,
	}, nil
}

// tokenExpiry reads a JWT's `exp` without checking the signature, which the
// cluster does. Zero when there's none.
func tokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return time.Time{}
	}
	exp, err := claims.Exp.Float64()
	if err != nil || exp <= 0 {
		return time.Time{}
	}
	return time.Unix(int64(exp), 0)
}

// proxyError answers with a Kubernetes Status, so clients show its message.
func (r *kubeconfigRelay) proxyError(w http.ResponseWriter, req *http.Request, err error) {
	if r.ctx.Err() != nil {
		writeRelayStatus(w, http.StatusServiceUnavailable, "ServiceUnavailable",
			"the kubeconfig relay has closed, because Terraform has finished with this kubeconfig")
		return
	}
	tflog.Warn(r.ctx, "kubeconfig relay could not complete a request", map[string]any{
		"method": req.Method,
		"path":   req.URL.Path,
		"error":  redactDiagnosticText(err.Error()),
	})
	writeRelayStatus(w, http.StatusBadGateway, "BadGateway", redactDiagnosticText(err.Error()))
}

func writeRelayStatus(w http.ResponseWriter, code int, reason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	// A failed write means the client has gone.
	if err := json.NewEncoder(w).Encode(map[string]any{
		"kind":       "Status",
		"apiVersion": "v1",
		"metadata":   map[string]any{},
		"status":     "Failure",
		"message":    message,
		"reason":     reason,
		"code":       code,
	}); err != nil {
		return
	}
}

// relayLogWriter sends the server's and proxy's log lines to the provider's
// log. They never carry a header.
type relayLogWriter struct{ r *kubeconfigRelay }

func (l relayLogWriter) Write(p []byte) (int, error) {
	tflog.Debug(l.r.ctx, "kubeconfig relay: "+strings.TrimSpace(string(p)))
	return len(p), nil
}

// relayCertificates makes a CA for this open and a 127.0.0.1 certificate. The
// CA key is dropped after signing, so it can issue nothing else.
func relayCertificates() (string, tls.Certificate, error) {
	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", tls.Certificate{}, err
	}
	ca := &x509.Certificate{
		SerialNumber:          relaySerial(),
		Subject:               pkix.Name{CommonName: "k0rdent kubeconfig relay CA"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(7 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return "", tls.Certificate{}, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return "", tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", tls.Certificate{}, err
	}
	leaf := &x509.Certificate{
		SerialNumber: relaySerial(),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(7 * 24 * time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		return "", tls.Certificate{}, err
	}
	caPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	return caPEM, tls.Certificate{Certificate: [][]byte{leafDER}, PrivateKey: key}, nil
}

func relaySerial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return big.NewInt(time.Now().UnixNano())
	}
	return n
}

// relayRegistry lets Close find its relay by the id in the private data.
type relayRegistry struct {
	mu   sync.Mutex
	open map[string]*kubeconfigRelay
}

func (g *relayRegistry) add(r *kubeconfigRelay) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.open == nil {
		g.open = map[string]*kubeconfigRelay{}
	}
	g.open[id] = r
	return id, nil
}

func (g *relayRegistry) remove(id string) *kubeconfigRelay {
	g.mu.Lock()
	defer g.mu.Unlock()
	r := g.open[id]
	delete(g.open, id)
	return r
}
